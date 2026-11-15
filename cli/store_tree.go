package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	gopath "path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"toft/cli/internal/client"
	"toft/cli/internal/ui"
)

// `toft store cp | mv | mkdir`, and the recursive halves of `rm` and `ls` — a
// store worked on as a tree, the way `aws s3` works on a bucket.
//
// **Nothing here is a new kind of request.** Every object still moves on its own
// signed URL, minted and audited one at a time, at the level the caller holds on
// *that* key; a tree is a walk over the routes `put`, `get` and `rm` already use.
// So a recursive copy can never do what the one-file commands could not, and a
// folder-scoped grant narrows it exactly as it narrows them.
//
// Three things are consequences of the store being S3-shaped, and are taken
// rather than hidden:
//
//   - **A folder is a prefix.** It exists while something is under it. `mkdir`
//     writes the zero-byte marker S3's own console writes (`path/`), which is
//     what makes an empty folder survive; a folder with objects needs none.
//   - **There is no rename.** `mv` is copy, then delete, *per object*, and the
//     delete runs only after that object's copy has landed — the upload's MD5 is
//     signed, so a 2xx is the store having verified the bytes. A move that fails
//     halfway leaves two copies of what it had not finished, never none.
//   - **A store-to-store copy passes through this machine.** Download to a
//     temporary file, upload from it: the digests have to be known before the
//     upload URL is minted, and the bytes never pass through the install.

// remoteScheme marks the store side of a cp or mv. `ls`, `rm` and `mkdir` take a
// bare `store/path` as well, because they have no local side to confuse it with.
const remoteScheme = "toft://"

// remoteRef is one place in one store. path carries no leading or trailing
// slash; dir is whether it was written as a folder (a trailing slash, or the
// store alone).
type remoteRef struct {
	store, path string
	dir         bool
}

func (r remoteRef) String() string {
	s := remoteScheme + r.store + "/" + r.path
	if r.dir && r.path != "" {
		s += "/"
	}
	return s
}

// join names something under this ref taken as a folder.
func (r remoteRef) join(rel string) string {
	if r.path == "" {
		return rel
	}
	return r.path + "/" + rel
}

func isRemote(arg string) bool { return strings.HasPrefix(arg, remoteScheme) }

// parseRemote reads `toft://store/path` or, where allowed, `store/path`.
func parseRemote(arg string) (remoteRef, error) {
	raw := strings.TrimPrefix(arg, remoteScheme)
	store, rest, _ := strings.Cut(raw, "/")
	if store == "" {
		return remoteRef{}, usageCodef("no_store", "%q names no store — write it as toft://<store>/<path>", arg)
	}
	path := strings.Trim(rest, "/")
	// Collapse a doubled separator the way `ls` always has, so `s//f` and `s/f`
	// are one question. Anything stranger is the server's to refuse.
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	return remoteRef{store: store, path: path, dir: path == "" || strings.HasSuffix(rest, "/")}, nil
}

// --- Walking a store ---

// walkRemote returns every object under a folder, named relative to it. A name
// ending in a slash is a folder marker.
//
// One flat listing, paged — except where the install answers with folders
// instead of objects, which it does at the root of a store the caller holds
// only some folders of: that root is assembled from their grants and lists
// nothing real. Each folder it names is then walked in turn, so a recursive
// command over such a store covers exactly what was granted.
func walkRemote(s *session, store, folder string) ([]storeObject, error) {
	var out []storeObject
	cursor := ""
	var prefixes []string
	for {
		query := url.Values{"flat": {"1"}}
		if folder != "" {
			query.Set("path", folder+"/")
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page storeObjectPage
		path := "/api/v1/store/" + url.PathEscape(store) + "/objects?" + query.Encode()
		if err := s.client.Do(http.MethodGet, path, nil, &page); err != nil {
			return nil, storeError(err, store)
		}
		out = append(out, page.Objects...)
		prefixes = append(prefixes, page.Prefixes...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	for _, p := range prefixes {
		sub := strings.TrimSuffix(p, "/")
		under, err := walkRemote(s, store, joinRemote(folder, sub))
		if err != nil {
			return nil, err
		}
		for _, o := range under {
			o.Name = sub + "/" + o.Name
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func joinRemote(folder, name string) string {
	if folder == "" {
		return name
	}
	return folder + "/" + name
}

func isMarker(name string) bool { return strings.HasSuffix(name, "/") }

// --- One object at a time ---

func objectPath(store, name string) string {
	return "/api/v1/store/" + url.PathEscape(store) + "/objects/" + escapeObjectName(name)
}

// uploadFile puts one local file at one name. The digests come first, from one
// pass over the file, because they are signed into the URL.
func uploadFile(e *env, s *session, store, name, local string) (int64, error) {
	// A local file this machine cannot read is the caller's to fix, coded as
	// every other unreadable file is, so a batch item names it (BB-015).
	file, err := os.Open(local)
	if err != nil {
		return 0, usageCodef("unreadable_file", "cannot read %s: %v", local, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, usageCodef("unreadable_file", "cannot read %s: %v", local, err)
	}
	if info.IsDir() {
		return 0, usageCodef("is_a_folder", "%s is a directory — pass -r to copy what is in it", local)
	}
	sha, md5sum, err := digests(file)
	if err != nil {
		return 0, usageCodef("unreadable_file", "reading %s: %v", local, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("rewinding %s: %w", local, err)
	}
	var signed signedRequest
	req := map[string]any{"name": name, "size": info.Size(), "sha256": sha, "md5": md5sum}
	if err := s.client.Do(http.MethodPost,
		"/api/v1/store/"+url.PathEscape(store)+"/objects", req, &signed); err != nil {
		return 0, storeError(err, store)
	}
	return info.Size(), transfer(e, &signed, file, info.Size())
}

// downloadObject writes one object to a local path, through a temporary file
// beside it, so an interrupted download never leaves half a file under the
// real name — and never truncates the file it was about to replace. meter is
// the byte meter, for the one download a person is watching; a copy of many
// runs several at once, and one \r line cannot show them.
func downloadObject(e *env, s *session, store, name, dest string, meter bool) (int64, error) {
	var signed signedRequest
	if err := s.client.Do(http.MethodGet, objectPath(store, name), nil, &signed); err != nil {
		return 0, storeError(err, store)
	}
	body, size, err := fetch(e, &signed)
	if errors.Is(err, errObjectMissing) {
		return 0, noSuchStorePath(remoteRef{store: store, path: name})
	}
	if err != nil {
		return 0, err
	}
	defer body.Close()

	if dest == "-" {
		// The bytes are the answer, perhaps to this same terminal: no meter
		// among them.
		// `-o -` is the bytes on stdout, as every Unix tool spells it — not a
		// file called "-" in the working directory.
		written, err := io.Copy(e.out.Stdout, body)
		if err != nil {
			return written, fmt.Errorf("writing to stdout: %w", err)
		}
		return written, nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, fmt.Errorf("cannot create %s: %w", filepath.Dir(dest), err)
	}
	var r io.Reader = body
	if meter {
		r = metered(e, "download", body, size)
	}
	return writeBeside(dest, r)
}

// writeBeside writes a download through a temporary file next to dest and
// renames it into place, so an interrupted transfer never leaves a truncated
// file under the real name, nor takes away the one that was there.
func writeBeside(dest string, body io.Reader) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".toft-*")
	if err != nil {
		return 0, fmt.Errorf("cannot write beside %s: %w", dest, err)
	}
	written, err := io.Copy(tmp, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dest)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return 0, fmt.Errorf("writing %s: %w", dest, err)
	}
	return written, nil
}

func deleteObject(e *env, s *session, store, name string) error {
	if isMarker(name) {
		return deleteMarker(e, s, store, strings.TrimSuffix(name, "/"))
	}
	var signed signedRequest
	if err := s.client.Do(http.MethodDelete, objectPath(store, name), nil, &signed); err != nil {
		return storeError(err, store)
	}
	return transfer(e, &signed, nil, 0)
}

// createMarker writes `folder/`. At `write` the URL is new-objects-only, so a
// marker that is already there answers 412 — which is the folder existing,
// and therefore success.
func createMarker(e *env, s *session, store, folder string) (existed bool, err error) {
	var signed signedRequest
	if err := s.client.Do(http.MethodPost, "/api/v1/store/"+url.PathEscape(store)+"/folders",
		map[string]string{"path": folder}, &signed); err != nil {
		return false, storeError(err, store)
	}
	err = transferStatus(e, &signed, http.NoBody, 0, http.StatusPreconditionFailed)
	if errors.Is(err, errTolerated) {
		return true, nil
	}
	return false, err
}

// deleteMarker removes `folder/` and nothing under it. A folder that never had a
// marker is not a failure: S3 answers 204 regardless, Azure 404.
func deleteMarker(e *env, s *session, store, folder string) error {
	var signed signedRequest
	if err := s.client.Do(http.MethodDelete,
		"/api/v1/store/"+url.PathEscape(store)+"/folders/"+escapeObjectName(folder), nil, &signed); err != nil {
		return storeError(err, store)
	}
	err := transferStatus(e, &signed, nil, 0, http.StatusNotFound)
	if errors.Is(err, errTolerated) {
		return nil
	}
	return err
}

var errTolerated = errors.New("tolerated status")

// transferStatus is transfer with one status that means "already so" rather
// than failure.
func transferStatus(e *env, signed *signedRequest, body io.Reader, size int64, tolerate int) error {
	req, err := http.NewRequestWithContext(e.ctx, signed.Method, signed.URL, body)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	applySignedHeaders(req, signed)
	req.ContentLength = size
	resp, err := storeHTTP().Do(req)
	if err != nil {
		return unreachableStore(signed.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == tolerate {
		return errTolerated
	}
	if resp.StatusCode >= 300 {
		return objectStoreRefusal(resp)
	}
	return nil
}

// --- The plan, then the work ---

type opKind int

const (
	opUpload   opKind = iota // local file → object
	opDownload               // object → local file
	opRelay                  // object → object, through a temporary file
	opMkdirRemote
	opMkdirLocal
)

// op is one step of a copy. For a move, the source is removed after the step
// succeeds, and only then.
type op struct {
	kind       opKind
	srcStore   string
	src        string // a local path or an object name
	dstStore   string
	dst        string
	removeSrc  bool
	srcIsLocal bool
}

func (o op) describe() string {
	from, to := o.src, o.dst
	if !o.srcIsLocal {
		from = remoteScheme + o.srcStore + "/" + o.src
	}
	switch o.kind {
	case opUpload, opRelay, opMkdirRemote:
		to = remoteScheme + o.dstStore + "/" + o.dst
	case opMkdirLocal:
		// A folder, written as one everywhere else: `tree/empty/`.
		to = strings.TrimSuffix(to, string(filepath.Separator)) + string(filepath.Separator)
	}
	return from + " → " + to
}

type opResult struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Bytes int64  `json:"bytes"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// refuseJobs is -j below 1, refused rather than quietly read as 1: a flag that
// accepts 0 says the caller asked for no transfers at all, and the run that
// then copies everything anyway answered a different question. --lines refuses
// the same way.
func refuseJobs(jobs int) error {
	if jobs < 1 {
		return usageCodef("invalid_jobs", "-j takes a count of 1 or more, not %d", jobs)
	}
	return nil
}

// runOps executes a plan, `jobs` at a time, and carries on past a failure: one
// object a grant does not reach should not abandon the other nine hundred. The
// failures are counted and the command exits non-zero naming how many.
//
// The verb is past tense for the line each success prints — "copied",
// "moved" — and the lines go to stdout, because the list of what was done is
// the answer a pipeline reads.
func runOps(e *env, s *session, g *globalFlags, ops []op, jobs int, verb string) error {
	if jobs < 1 {
		jobs = 1
	}
	results := make([]opResult, len(ops))
	errs := make([]error, len(ops))
	var mu sync.Mutex
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				n, err := runOpPaced(e, s, ops[i])
				r := opResult{From: ops[i].describeFrom(), To: ops[i].describeTo(), Bytes: n}
				mu.Lock()
				errs[i] = err
				if err != nil {
					// Nothing landed, so nothing is reported as moved.
					r.Bytes = 0
					r.Error = err.Error()
					// The install's code or this client's own reading of
					// the object store (already_exists), alike.
					r.Code = refusalCode(err)
					// A name is another party's text: shown with its
					// controls as `?`, the OS error included, which repeats
					// the path (security review 2026-11-15, CL-6). The
					// result document keeps the exact names.
					e.out.Failed("%s", listedName(fmt.Sprintf("%s: %v", ops[i].describe(), err)))
				} else if !g.json {
					e.out.Answer("%s  %s %s", e.out.OK(verb), listedName(ops[i].describe()),
						e.out.Dimmed("("+ui.FormatBytes(n)+")"))
				}
				results[i] = r
				mu.Unlock()
			}
		}()
	}
	for i := range ops {
		if e.ctx.Err() != nil {
			break
		}
		work <- i
	}
	close(work)
	wg.Wait()

	// Only what ran: an interrupt left the unreached slots as zero values,
	// which under --json were rows reading as successes, and the command
	// exited 0 (code review 2026-11-11, M-4).
	results = ranOnly(results)
	failed := 0
	for _, r := range results {
		if r.Error != "" {
			failed++
		}
	}
	if g.json {
		if err := writeNDJSON(e.out.Stdout, results); err != nil {
			return err
		}
	}
	if err := e.ctx.Err(); err != nil {
		return err
	}
	if failed > 0 {
		return batchError(errs, failed)
	}
	return nil
}

func (o op) describeFrom() string {
	if o.srcIsLocal {
		return o.src
	}
	return remoteScheme + o.srcStore + "/" + o.src
}

func (o op) describeTo() string {
	switch o.kind {
	case opDownload, opMkdirLocal:
		return o.dst
	}
	return remoteScheme + o.dstStore + "/" + o.dst
}

// partialError is some of a tree having failed. Each failure was already
// printed as it happened; this is the count, and exit 1.
type partialError struct{ failed, total int }

// batchError is how a batch ends when something in it failed. When nothing
// succeeded and every item was refused for the same reason, the batch *is*
// that refusal, so it keeps the refusal's own exit code and `code`: `toft store
// cp x toft://s/loose.txt` outside a partition's folders is exit 3
// outside_mapped_folders exactly as `toft store put` is, not a "1 of 1 failed"
// exit 1 an agent has to parse the line above to explain.
func batchError(errs []error, failed int) error {
	if failed == len(errs) && len(errs) > 0 {
		// Every item refused with one code — the install's, or this client's
		// own (not_found, is_a_folder, already_exists): the batch is that
		// refusal, with its exit (BB-017).
		if code := refusalCode(errs[0]); code != "" {
			alike := true
			for _, err := range errs {
				if refusalCode(err) != code {
					alike = false
					break
				}
			}
			if alike {
				return errs[0]
			}
		}
		var first *client.Error
		same := true
		for _, err := range errs {
			var apiErr *client.Error
			if !errors.As(err, &apiErr) || apiErr.Code == "" || (first != nil && apiErr.Code != first.Code) {
				same = false
				break
			}
			if first == nil {
				first = apiErr
			}
		}
		if same {
			return errs[0]
		}
		// The client's own reading of an object store's answer — `write`
		// refusing to replace a name — is a coded refusal too, and a batch of
		// only that is exit 3 already_exists as `put` is, not exit 1.
		var own *authError
		if errors.As(errs[0], &own) && own.code != "" {
			alike := true
			for _, err := range errs {
				var a *authError
				if !errors.As(err, &a) || a.code != own.code {
					alike = false
					break
				}
			}
			if alike {
				return errs[0]
			}
		}
		// The same for an object store nobody could reach: every item failed
		// for one reason, so the batch is exit 4 as `put` is, not "1 of 1
		// failed" exit 1 (docs/bugs 2026-10-22-e2e-storage #2, the related one).
		unreachable := true
		for _, err := range errs {
			var u *client.UnreachableError
			if !errors.As(err, &u) {
				unreachable = false
				break
			}
		}
		if unreachable && len(errs) > 0 {
			return errs[0]
		}
	}
	return &partialError{failed: failed, total: len(errs)}
}

func (p *partialError) Error() string {
	return fmt.Sprintf("%d of %d failed — each is named above; what succeeded stays done", p.failed, p.total)
}

// busyRetries is how many times one item is tried again when the store says
// it is pacing this caller (store_busy), and busyBackoff the first wait,
// doubled each time. -j above what the store admits then slows down instead of
// failing items (M2, 2026-11-04 black-box list).
var (
	busyRetries = 6
	busyBackoff = 250 * time.Millisecond
)

// runOpPaced is runOp, tried again with backoff while the store is busy.
func runOpPaced(e *env, s *session, o op) (int64, error) {
	var n int64
	err := paced(e, func() error {
		var err error
		n, err = runOp(e, s, o)
		return err
	})
	return n, err
}

// paced runs one store step, tried again with backoff while the store says it
// is pacing this caller — a copy's transfer and rm -r's delete alike (BB-004).
func paced(e *env, step func() error) error {
	wait := busyBackoff
	for attempt := 0; ; attempt++ {
		err := step()
		var f *failedError
		if err == nil || attempt >= busyRetries || !errors.As(err, &f) || f.code != "store_busy" {
			return err
		}
		select {
		case <-e.ctx.Done():
			return err
		case <-time.After(wait):
		}
		if wait < 4*time.Second {
			wait *= 2
		}
	}
}

func runOp(e *env, s *session, o op) (int64, error) {
	var n int64
	var err error
	switch o.kind {
	case opUpload:
		n, err = uploadFile(e, s, o.dstStore, o.dst, o.src)
	case opDownload:
		n, err = downloadObject(e, s, o.srcStore, o.src, o.dst, false)
	case opRelay:
		n, err = relayObject(e, s, o.srcStore, o.src, o.dstStore, o.dst)
	case opMkdirRemote:
		_, err = createMarker(e, s, o.dstStore, strings.TrimSuffix(o.dst, "/"))
	case opMkdirLocal:
		err = os.MkdirAll(o.dst, 0o755)
	}
	if err != nil || !o.removeSrc {
		return n, err
	}
	// The copy landed; only now does the source go.
	if o.srcIsLocal {
		if o.kind == opMkdirRemote {
			return n, nil // directories are pruned after every file has moved
		}
		return n, os.Remove(o.src)
	}
	if rerr := deleteObject(e, s, o.srcStore, o.src); rerr != nil {
		return n, fmt.Errorf("copied, but the source was not removed: %w", rerr)
	}
	return n, nil
}

// relayObject copies an object to another name, or another store, through a
// temporary file on this machine. TMPDIR decides where; the file is gone when
// this returns.
func relayObject(e *env, s *session, srcStore, src, dstStore, dst string) (int64, error) {
	dir, err := os.MkdirTemp("", "toft-store-")
	if err != nil {
		return 0, fmt.Errorf("cannot make a temporary directory: %w", err)
	}
	defer os.RemoveAll(dir)
	local := filepath.Join(dir, "object")
	if _, err := downloadObject(e, s, srcStore, src, local, false); err != nil {
		return 0, err
	}
	return uploadFile(e, s, dstStore, dst, local)
}

// --- cp and mv ---

func cmdStoreCopy(e *env, args []string) error { return copyOrMove(e, args, false) }
func cmdStoreMove(e *env, args []string) error { return copyOrMove(e, args, true) }

func copyOrMove(e *env, args []string, move bool) error {
	name := "cp"
	if move {
		name = "mv"
	}
	var g globalFlags
	var recursive, force bool
	var jobs int
	fs := newFlags("store "+name, storeUsage)
	g.register(fs)
	fs.BoolVar(&recursive, "r", false, "the whole folder")
	fs.BoolVar(&recursive, "recursive", false, "the whole folder")
	fs.BoolVar(&force, "force", false, "replace local files that already exist")
	fs.IntVar(&jobs, "j", 4, "transfers at once")
	fs.IntVar(&jobs, "jobs", 4, "transfers at once")
	if err := parseOperands(fs, args, -1); err != nil {
		return skipHelp(err)
	}
	if err := refuseJobs(jobs); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		// Too few is a missing operand, too many a stray one — the two codes
		// every other command uses for them.
		code := "unexpected_argument"
		if fs.NArg() < 2 {
			code = "missing_argument"
		}
		return usageCodef(code, "toft store %s [-r] <from> <to> — one side local, or both toft://<store>/<path>", name)
	}
	from, to := fs.Arg(0), fs.Arg(1)

	s, err := e.setup(&g)
	if err != nil {
		return err
	}
	if !isRemote(from) && !isRemote(to) {
		// `<store>/<path>` works here as it does for every other subcommand
		// (BB-019, third 2026-11-03 list), by the rule storeSide states.
		from, to, err = resolveStoreSides(s, from, to)
		if err != nil {
			return err
		}
	}
	if move {
		if err := checkMoveAccess(s, from, to); err != nil {
			return err
		}
	}
	ops, prune, err := planCopy(s, from, to, recursive, force, move)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		e.out.Say("Nothing to %s under %s.", name, from)
		return nil
	}
	if move && !isRemote(from) {
		// Removing local files is the one thing here a store's grant does not
		// bound, so it is said before it happens.
		if err := confirm(e, &g, fmt.Sprintf("move %d item(s) from %s (each local file is removed once its upload lands)", len(ops), from)); err != nil {
			return err
		}
	}
	verb := "copied"
	if move {
		verb = "moved"
	}
	if err := runOps(e, s, &g, ops, jobs, verb); err != nil {
		return err
	}
	return finishMove(e, s, prune)
}

// resolveStoreSides reads cp/mv operands written with no toft:// on either
// side. Each is a store path when storeSide says so; when neither is, the
// refusal is the one it always was.
func resolveStoreSides(s *session, from, to string) (string, string, error) {
	var body struct {
		Stores []storeSummary `json:"stores"`
	}
	if err := s.client.Do(http.MethodGet, "/api/v1/store", nil, &body); err != nil {
		return "", "", storeError(err, "")
	}
	stores := map[string]bool{}
	for _, st := range body.Stores {
		stores[st.Slug] = true
	}
	if storeSide(from, stores) {
		from = remoteScheme + from
	}
	if storeSide(to, stores) {
		to = remoteScheme + to
	}
	if !isRemote(from) && !isRemote(to) {
		return "", "", usageCodef("no_store", "neither side is a store — write the store side as "+
			"toft://<store>/<path> (or <store>/<path>, when nothing here has that name)")
	}
	return from, to, nil
}

// storeSide is whether a cp/mv operand without toft:// is a store path. **The
// local filesystem wins**: it is `<store>/…` with <store> a store this
// credential can see, not written as a path (./, ../, /, ~), and nothing here
// exists under its first segment. A local folder that shares a store's name is
// the local folder; toft:// says otherwise.
func storeSide(arg string, stores map[string]bool) bool {
	if arg == "" || filepath.IsAbs(arg) || strings.HasPrefix(arg, ".") || strings.HasPrefix(arg, "~") {
		return false
	}
	first, _, _ := strings.Cut(arg, "/")
	if !stores[first] {
		return false
	}
	if _, err := os.Lstat(first); err == nil {
		return false
	}
	return true
}

// accessRank orders the three levels; an unknown one ranks nowhere. It mirrors
// the server's storeAccessRank (internal/database/store_grants.go), which this
// module cannot import; TestAccessRankIsTheServersLadder holds the two together.
var accessRank = map[string]int{"read": 1, "write": 2, "full": 3}

// checkMoveAccess refuses a move the grants already say cannot finish, before
// anything is copied. A move is copy-then-delete, and the delete is the one that
// needs `full`: at `write` the copy lands, the source stays, and the caller is
// left with two objects and exit 1. Refused here, it is exit 3 and nothing done.
//
// Only a level this credential is known to hold is judged. A path no row of
// `GET /store` covers is left to the install, which answers it per object.
func checkMoveAccess(s *session, from, to string) error {
	var body struct {
		Stores []storeSummary `json:"stores"`
	}
	if err := s.client.Do(http.MethodGet, "/api/v1/store", nil, &body); err != nil {
		return storeError(err, "")
	}
	need := func(arg, level, why string) error {
		if !isRemote(arg) {
			return nil
		}
		ref, err := parseRemote(arg)
		if err != nil {
			return err
		}
		have := accessAt(body.Stores, ref.store, ref.path)
		if have == "" || accessRank[have] >= accessRank[level] {
			return nil
		}
		return refusedCodef("insufficient_access", "nothing was moved: your access to %s is %q, and %s needs %q — "+
			"`toft store cp` copies without removing the source", ref, have, why, level)
	}
	if err := need(from, "full", "removing the source"); err != nil {
		return err
	}
	return need(to, "write", "writing the destination")
}

// accessAt is the highest level any row grants over path in store, or "" when
// no row covers it. Grants are per folder, so one store can answer at two levels.
func accessAt(stores []storeSummary, store, path string) string {
	best := ""
	for _, st := range stores {
		if st.Slug != store {
			continue
		}
		folder := strings.Trim(st.Folder, "/")
		if folder != "" && path != folder && !strings.HasPrefix(path, folder+"/") {
			continue
		}
		if accessRank[st.Access] > accessRank[best] {
			best = st.Access
		}
	}
	return best
}

// prune is what a move leaves to tidy after every object has gone: the local
// directories it emptied, or the source folder's own marker.
type prune struct {
	localRoot   string
	remoteStore string
	remoteDir   string
	// remoteMarkers are the markers of source folders that held something,
	// not copied, and removed — deepest first — once what was in them has.
	remoteMarkers []string
}

func finishMove(e *env, s *session, p prune) error {
	if p.localRoot != "" {
		removeEmptyDirs(p.localRoot)
	}
	if p.remoteStore != "" {
		if err := deleteMarkersDeepestFirst(e, s, p.remoteStore, p.remoteMarkers); err != nil {
			return err
		}
	}
	if p.remoteStore != "" && p.remoteDir != "" {
		return deleteMarker(e, s, p.remoteStore, p.remoteDir)
	}
	return nil
}

// removeEmptyDirs removes, deepest first, every directory under root that is
// now empty — and root itself if it is. One holding anything is left alone.
func removeEmptyDirs(root string) {
	var dirs []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // fails, harmlessly, on anything not empty
	}
}

// planCopy turns two arguments into the list of steps, before anything moves.
//
// The naming follows `aws s3 cp`: a destination written as a folder (a trailing
// slash, an existing directory, or a store alone) receives the source under its
// own last name; with -r, a folder's *contents* land under the destination.
func planCopy(s *session, from, to string, recursive, force, move bool) ([]op, prune, error) {
	switch {
	case !isRemote(from):
		dst, err := parseRemote(to)
		if err != nil {
			return nil, prune{}, err
		}
		return planUpload(from, dst, recursive, move)
	case !isRemote(to):
		src, err := parseRemote(from)
		if err != nil {
			return nil, prune{}, err
		}
		return planDownload(s, src, to, recursive, force, move)
	default:
		src, err := parseRemote(from)
		if err != nil {
			return nil, prune{}, err
		}
		dst, err := parseRemote(to)
		if err != nil {
			return nil, prune{}, err
		}
		return planRelay(s, src, dst, recursive, move)
	}
}

func planUpload(local string, dst remoteRef, recursive, move bool) ([]op, prune, error) {
	info, err := os.Stat(local)
	if err != nil {
		return nil, prune{}, usageCodef("unreadable_file", "cannot read %s: %v", local, err)
	}
	if !info.IsDir() {
		name := dst.path
		if dst.dir {
			name = dst.join(filepath.Base(local))
		}
		return []op{{kind: opUpload, src: local, srcIsLocal: true, dstStore: dst.store, dst: name, removeSrc: move}}, prune{}, nil
	}
	if !recursive {
		return nil, prune{}, usageCodef("is_a_folder", "%s is a directory — pass -r to copy what is in it", local)
	}
	// Stat followed the link to say "directory"; WalkDir Lstats its root and
	// does not descend a link, so `cp -r out` of a symlinked folder planned
	// nothing and exited 0 (code review 2026-11-11, M-3). Walk the target.
	if resolved, err := filepath.EvalSymlinks(local); err == nil {
		local = resolved
	}
	var ops []op
	err = filepath.WalkDir(local, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(local, path)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			// Only an empty directory needs a marker; one holding a file comes
			// into being with the file.
			if entries, _ := os.ReadDir(path); len(entries) == 0 {
				ops = append(ops, op{kind: opMkdirRemote, src: path, srcIsLocal: true,
					dstStore: dst.store, dst: dst.join(rel) + "/", removeSrc: move})
			}
		case d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0:
			if d.Type()&fs.ModeSymlink != 0 {
				if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
					return nil // a link to a directory or to nothing is not a file to carry
				}
			}
			ops = append(ops, op{kind: opUpload, src: path, srcIsLocal: true,
				dstStore: dst.store, dst: dst.join(rel), removeSrc: move})
		}
		return nil
	})
	if err != nil {
		return nil, prune{}, fmt.Errorf("walking %s: %w", local, err)
	}
	p := prune{}
	if move {
		p.localRoot = local
	}
	return ops, p, nil
}

// foldsLocalNames is whether this machine's disk is case-insensitive by
// default (APFS, NTFS), so `Report.csv` and `report.csv` are one file. A
// variable so a test can set it.
var foldsLocalNames = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

func planDownload(s *session, src remoteRef, local string, recursive, force, move bool) ([]op, prune, error) {
	if !recursive {
		if err := resolveSource(s, &src); err != nil {
			return nil, prune{}, err
		}
		dest := local
		if st, err := os.Stat(local); (err == nil && st.IsDir()) || strings.HasSuffix(local, string(os.PathSeparator)) || strings.HasSuffix(local, "/") {
			dest = filepath.Join(local, gopath.Base(src.path))
		}
		if err := refuseClobber(dest, force); err != nil {
			return nil, prune{}, err
		}
		return []op{{kind: opDownload, srcStore: src.store, src: src.path, dst: dest, removeSrc: move}}, prune{}, nil
	}
	objects, err := walkRemote(s, src.store, src.path)
	if err != nil {
		return nil, prune{}, err
	}
	if len(objects) == 0 {
		if single, err := emptyRecursiveSource(s, src); err != nil || single {
			if err != nil {
				return nil, prune{}, err
			}
			return planDownload(s, src, local, false, force, move)
		}
	}
	var ops []op
	var markers []string
	full := nonEmptyFolders(objects)
	// Two names that are one file on this disk would each be renamed over the
	// other, and a move then deletes both sources (security review
	// 2026-11-15, CL-2). The plan is whole before anything runs, so the disk
	// cannot tell; the plan itself can.
	seen := map[string]string{}
	for _, obj := range objects {
		if isMarker(obj.Name) && full[obj.Name] {
			// A folder with something in it comes into being with what is in
			// it. The host volume lists every directory as a marker, and
			// copying each was a line per folder for nothing (BB-017).
			markers = append(markers, src.join(obj.Name))
			continue
		}
		dest := filepath.Join(local, filepath.FromSlash(strings.TrimSuffix(obj.Name, "/")))
		if rel, err := filepath.Rel(local, dest); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			// The server validates every name it signs; this is the same rule
			// on the side that writes the disk.
			return nil, prune{}, fmt.Errorf("refusing %q: it would land outside %s", obj.Name, local)
		}
		if isMarker(obj.Name) {
			ops = append(ops, op{kind: opMkdirLocal, srcStore: src.store, src: src.join(obj.Name), dst: dest, removeSrc: move})
			continue
		}
		key := dest
		if foldsLocalNames {
			key = strings.ToLower(dest)
		}
		if other, dup := seen[key]; dup {
			return nil, prune{}, usageCodef("name_collision", "%s and %s would be one file in %s on this disk — "+
				"copy them one at a time under different names", listedName(other), listedName(obj.Name), listedName(local))
		}
		seen[key] = obj.Name
		if err := refuseClobber(dest, force); err != nil {
			return nil, prune{}, err
		}
		ops = append(ops, op{kind: opDownload, srcStore: src.store, src: src.join(obj.Name), dst: dest, removeSrc: move})
	}
	p := prune{}
	if move {
		p.remoteStore, p.remoteDir, p.remoteMarkers = src.store, src.path, markers
	}
	return ops, p, nil
}

func planRelay(s *session, src, dst remoteRef, recursive, move bool) ([]op, prune, error) {
	if !recursive {
		if err := resolveSource(s, &src); err != nil {
			return nil, prune{}, err
		}
		name := dst.path
		if dst.dir {
			name = dst.join(gopath.Base(src.path))
		}
		if src.store == dst.store && src.path == name {
			// A move onto itself would copy, then delete the only copy.
			return nil, prune{}, usageCodef("same_object", "%s and the destination are the same object", src)
		}
		return []op{{kind: opRelay, srcStore: src.store, src: src.path, dstStore: dst.store, dst: name, removeSrc: move}}, prune{}, nil
	}
	if src.store == dst.store && src.path == dst.path {
		return nil, prune{}, usageCodef("same_object", "%s and the destination are the same folder", src)
	}
	// One folder inside the other, in one store: the plan's ops would write
	// over objects that later ops still have to read, four at a time, and a
	// recursive move of a/b/ onto a/ destroyed a/b/x while printing "moved"
	// (code review 2026-11-11, M-1). Refused, as mv refuses a directory into
	// its own subdirectory.
	if src.store == dst.store && nestedFolders(src.path, dst.path) {
		return nil, prune{}, usageCodef("nested_paths", "%s and the destination are one inside the other; "+
			"copy or move to a folder outside the source, or name the objects one at a time", src)
	}
	objects, err := walkRemote(s, src.store, src.path)
	if err != nil {
		return nil, prune{}, err
	}
	if len(objects) == 0 {
		if single, err := emptyRecursiveSource(s, src); err != nil || single {
			if err != nil {
				return nil, prune{}, err
			}
			return planRelay(s, src, dst, false, move)
		}
	}
	var ops []op
	var markers []string
	full := nonEmptyFolders(objects)
	for _, obj := range objects {
		kind := opRelay
		if isMarker(obj.Name) {
			if full[obj.Name] {
				markers = append(markers, src.join(obj.Name))
				continue
			}
			kind = opMkdirRemote
		}
		ops = append(ops, op{kind: kind, srcStore: src.store, src: src.join(obj.Name),
			dstStore: dst.store, dst: dst.join(obj.Name), removeSrc: move})
	}
	p := prune{}
	if move {
		p.remoteStore, p.remoteDir, p.remoteMarkers = src.store, src.path, markers
	}
	return ops, p, nil
}

// nonEmptyFolders is every folder in a walk that something lies under,
// written as its marker (`a/`, `a/b/`).
func nonEmptyFolders(objects []storeObject) map[string]bool {
	set := map[string]bool{}
	for _, o := range objects {
		name := strings.TrimSuffix(o.Name, "/")
		for i := strings.LastIndexByte(name, '/'); i >= 0; i = strings.LastIndexByte(name[:i], '/') {
			if set[name[:i+1]] {
				break
			}
			set[name[:i+1]] = true
		}
	}
	return set
}

// depth is how many folders deep a name is.
func depth(name string) int { return strings.Count(strings.TrimSuffix(name, "/"), "/") }

// deleteMarkersDeepestFirst removes folder markers one at a time, a child's
// before its parent's: on the host volume a marker is a directory, and one
// removed while something is still in it stays (BB-017).
func deleteMarkersDeepestFirst(e *env, s *session, store string, markers []string) error {
	sorted := append([]string(nil), markers...)
	sort.SliceStable(sorted, func(i, j int) bool { return depth(sorted[i]) > depth(sorted[j]) })
	for _, m := range sorted {
		if err := deleteMarker(e, s, store, strings.TrimSuffix(m, "/")); err != nil {
			return err
		}
	}
	return nil
}

// emptyRecursiveSource reads a -r source the walk found nothing under, by
// rm -r's rule: an object by that name is copied as one (single is true), an
// empty folder is nothing to do, and a name that is neither is not_found, exit
// 2 — as the forms without -r answer it (BB-034), never "nothing to copy".
func emptyRecursiveSource(s *session, src remoteRef) (single bool, err error) {
	if src.path == "" {
		return false, nil // a store's root is there, however empty
	}
	isObject, isFolder, err := lookupName(s, src.store, src.path)
	if err != nil {
		return false, err
	}
	switch {
	case isObject && !src.dir:
		return true, nil
	case isFolder:
		return false, nil
	}
	return false, noSuchStorePath(src)
}

// resolveSource is the single-object source of a cp or mv, asked about before
// anything moves (BB-017): a folder is is_a_folder, and a name that is neither
// is not_found, exit 2 — not a transfer that fails as "1 of 1". A listing the
// install will not give leaves the transfer to answer for itself.
func resolveSource(s *session, src *remoteRef) error {
	if src.dir || src.path == "" {
		return folderRefusal(*src)
	}
	isObject, isFolder, err := lookupName(s, src.store, src.path)
	if err != nil {
		return nil
	}
	switch {
	case isObject:
		return nil
	case isFolder:
		return folderRefusal(*src)
	}
	return noSuchStorePath(*src)
}

// refuseClobber is `get`'s rule, kept: a download never silently replaces a
// local file. --force says it may.
func refuseClobber(dest string, force bool) error {
	if force {
		return nil
	}
	if _, err := os.Stat(dest); err == nil {
		return usageCodef("file_exists", "%s already exists — pass --force to replace it", listedName(dest))
	}
	return nil
}

// --- mkdir ---

func cmdStoreMkdir(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("store mkdir", storeUsage)
	g.register(fs)
	if err := parseOperands(fs, args, -1); err != nil {
		return skipHelp(err)
	}
	if fs.NArg() < 1 {
		return usageCodef("missing_argument", "toft store mkdir <store>/<folder> …")
	}
	s, err := e.setup(&g)
	if err != nil {
		return err
	}
	// Every folder named, and which of them were new and which were already
	// there — text says "created" or "exists", and --json said neither
	// (BB-106, 2026-11-04 black-box list).
	made, created, already := []string{}, []string{}, []string{}
	for _, arg := range fs.Args() {
		ref, err := parseRemote(arg)
		if err != nil {
			return err
		}
		if ref.path == "" {
			return usageCodef("invalid_path", "%s is a store, not a folder in one", arg)
		}
		// Asked first, because only `write` makes the store say so itself: at
		// `full` the marker's PUT is unconditional, and a
		// second mkdir printed "created" over a folder that was already there.
		_, existed, err := lookupName(s, ref.store, ref.path)
		if err != nil {
			return err
		}
		if !existed {
			if existed, err = createMarker(e, s, ref.store, ref.path); err != nil {
				return err
			}
		}
		ref.dir = true
		made = append(made, ref.String())
		if existed {
			already = append(already, ref.String())
		} else {
			created = append(created, ref.String())
		}
		if g.json {
			continue
		}
		if existed {
			e.out.Answer("%s  %s", e.out.Dimmed("exists"), ref)
		} else {
			e.out.Answer("%s  %s", e.out.OK("created"), ref)
		}
	}
	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]any{"folders": made,
			"created": created, "existed": already})
	}
	return nil
}

// --- rm -r ---

// removeTree deletes everything under a folder, then the folder's own marker.
// One confirmation covers it, and it names the count and the size, because
// those are what a person checks before saying yes.
func removeTree(e *env, s *session, g *globalFlags, ref remoteRef, jobs int) error {
	objects, err := walkRemote(s, ref.store, ref.path)
	if err != nil {
		return err
	}
	if len(objects) == 0 && ref.path != "" {
		// Nothing beneath: an object by that name is deleted as rm would
		// (`rm -r file` is not a mistake worth refusing), an empty folder is
		// its marker, and neither is "not found".
		isObject, isFolder, err := lookupName(s, ref.store, ref.path)
		if err != nil {
			return err
		}
		switch {
		case isObject && !ref.dir:
			objects = []storeObject{{Name: ""}}
		case !isFolder:
			return noSuchStorePath(ref)
		}
	}
	// The count is the listing that drives the deletes, markers included, so
	// the number asked about is the number of lines that follow.
	var total int64
	for _, o := range objects {
		total += o.Size
	}
	single := len(objects) == 1 && objects[0].Name == ""
	if !single {
		ref.dir = true
	}
	question := fmt.Sprintf("delete %d object(s), %s, under %s (the store keeps no copy)",
		len(objects), ui.FormatBytes(total), ref)
	if single {
		question = fmt.Sprintf("delete %s (the store keeps no copy)", ref)
	}
	if err := confirm(e, g, question); err != nil {
		return err
	}
	// The objects first, several at once, and then the markers one at a time
	// and deepest first: on the host volume a marker is a directory, and one
	// removed while a sibling worker was still emptying it stayed behind, for
	// a second run to find (BB-017).
	sort.SliceStable(objects, func(i, j int) bool {
		mi, mj := isMarker(objects[i].Name), isMarker(objects[j].Name)
		if mi != mj {
			return mj
		}
		if mi {
			return depth(objects[i].Name) > depth(objects[j].Name)
		}
		return objects[i].Name > objects[j].Name
	})
	firstMarker := len(objects)
	for i, o := range objects {
		if isMarker(o.Name) {
			firstMarker = i
			break
		}
	}
	errs := make([]error, len(objects))
	results := make([]rmResult, len(objects))
	var mu sync.Mutex
	var wg sync.WaitGroup
	work := make(chan int)
	if jobs < 1 {
		jobs = 1
	}
	// One delete, paced as a copy's transfer is: host storage admits four
	// at once per person, and -j above that backs off rather than failing
	// items (BB-004). Each item is a document under --json, as cp's and mv's
	// are (BB-016).
	remove := func(i int, name string) {
		err := paced(e, func() error { return deleteObject(e, s, ref.store, name) })
		line := remoteRef{store: ref.store, path: strings.TrimSuffix(name, "/"), dir: isMarker(name) || i >= firstMarker}
		r := rmResult{Object: line.String(), Bytes: objects[i].Size, Deleted: err == nil}
		mu.Lock()
		defer mu.Unlock()
		errs[i] = err
		if err != nil {
			r.Bytes, r.Error, r.Code = 0, err.Error(), refusalCode(err)
			e.out.Failed("%s", listedName(fmt.Sprintf("%s: %v", line, err)))
		} else if !g.json {
			e.out.Answer("%s  %s", e.out.OK("deleted"), listedName(line.String()))
		}
		results[i] = r
	}
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				name := ref.path
				if !single {
					name = ref.join(objects[i].Name)
				}
				remove(i, name)
			}
		}()
	}
	for i := range objects[:firstMarker] {
		if e.ctx.Err() != nil {
			break
		}
		work <- i
	}
	close(work)
	wg.Wait()
	for i := firstMarker; i < len(objects); i++ {
		remove(i, ref.join(objects[i].Name))
	}

	failed := 0
	for _, err := range errs {
		if err != nil {
			failed++
		}
	}
	if g.json {
		done := results[:0:0]
		for _, r := range results {
			if r.Object != "" { // an interrupted run never reached it
				done = append(done, r)
			}
		}
		if err := writeNDJSON(e.out.Stdout, done); err != nil {
			return err
		}
	}
	if failed > 0 {
		return batchError(errs, failed)
	}
	if ref.path != "" && !single {
		if err := paced(e, func() error { return deleteMarker(e, s, ref.store, ref.path) }); err != nil {
			return err
		}
		// An empty folder is its marker alone: that is the one item.
		if len(objects) == 0 {
			if g.json {
				return json.NewEncoder(e.out.Stdout).Encode(rmResult{Object: ref.String(), Deleted: true})
			}
			e.out.Answer("%s  %s", e.out.OK("deleted"), ref)
		}
	}
	return nil
}

// rmResult is one item of rm -r under --json: the object as a toft:// path,
// whether it went, and a failure's code — cp's and mv's opResult, for a delete.
type rmResult struct {
	Object  string `json:"object"`
	Bytes   int64  `json:"bytes"`
	Deleted bool   `json:"deleted"`
	Error   string `json:"error,omitempty"`
	Code    string `json:"code,omitempty"`
}

// --- ls -r ---

func listTree(e *env, s *session, g *globalFlags, ref remoteRef) error {
	objects, err := walkRemote(s, ref.store, ref.path)
	if err != nil {
		return err
	}
	if g.json {
		// The one-level shape: markers are the folders, everything else the
		// objects, both named relative to what was listed.
		var folders []string
		var files []storeObject
		for _, obj := range objects {
			if isMarker(obj.Name) {
				folders = append(folders, obj.Name)
			} else {
				files = append(files, obj)
			}
		}
		return encodeListing(e, folders, files)
	}
	if len(objects) == 0 {
		e.out.Answer("Nothing in %s yet.", ref)
		return nil
	}
	if e.out.Rich() {
		var rows []storeRow
		for _, obj := range objects {
			full := remoteScheme + ref.store + "/" + ref.join(obj.Name)
			rows = append(rows, storeRow{path: full, dir: isMarker(obj.Name), size: obj.Size, modified: obj.LastModified})
		}
		printStoreRows(e, rows)
		return nil
	}
	for _, obj := range objects {
		full := listedName(remoteScheme + ref.store + "/" + ref.join(obj.Name))
		if isMarker(obj.Name) {
			e.out.Answer("%12s  %-16s  %s", "", "", e.out.Bold(full))
			continue
		}
		e.out.Answer("%12s  %s  %s", ui.FormatBytes(obj.Size),
			e.out.Dimmed(obj.LastModified.Local().Format("2006-01-02 15:04 MST")), full)
	}
	return nil
}

// nestedFolders reports two folder paths in one store of which one contains
// the other. "" is the store's root and contains everything.
func nestedFolders(a, b string) bool {
	a, b = strings.Trim(a, "/"), strings.Trim(b, "/")
	if a == "" || b == "" {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// ranOnly drops the slots of ops the run never reached (M-4): a zero row read
// as a success under --json.
func ranOnly(results []opResult) []opResult {
	ran := results[:0]
	for _, r := range results {
		if r.From != "" || r.To != "" || r.Error != "" {
			ran = append(ran, r)
		}
	}
	return ran
}
