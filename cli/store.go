package main

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	gopath "path"
	"path/filepath"
	"strings"
	"time"

	"toft/cli/internal/client"
	"toft/cli/internal/ui"
)

// `toft store` — a file into a store, from outside the platform.
//
// The shape of every command here is the same and it is worth stating once: the
// install signs a URL, and **the bytes go straight to the object store**. They
// do not pass through Toft, which is why an upload of any size is one
// request and why the platform needs no upload buffer.
//
// That has one consequence worth designing for rather than discovering: a
// network that can reach the install cannot necessarily reach the object store.
// A proxy-only corporate network is the common case. So when the transfer fails
// the error names *which host* was unreachable, because "upload failed" sends
// somebody to the wrong team.
//
// The credential is never a flag. It comes from the same resolution order as
// every other command — `toft login`, then TOFT_TOKEN, then the environment — for
// the reason stated in docs/invariants.md § The CLI: a token on a command line
// is in the shell history, in `ps`, and in the CI log.

const storeUsage = `toft store — files in a store or volume, from outside the platform

<store> is any storage resource's name — an object store, or a volume with a
data plane: every volume on an install without a cluster (a folder on the Toft
host), and S3 Files volumes on one. An EFS or Azure Files volume on a cluster
has none; only a pod mounting it reaches its files. ` + "`toft store ls`" + ` lists
everything you can reach.

Usage:
  toft store ls                          What you can reach, and at what level. A store
                                       you were granted one folder of is listed as
                                       store/folder/, and one you hold outright by
                                       its name alone
  toft store ls [-r] <store>[/<folder>]  One level of one: its folders, then its
                                       files. -r lists everything beneath
  toft store cp [-r] <from> <to>         Copy. The store side is toft://<store>/<path>;
                                       the other is a local path, or a second store
                                       path. -r copies a folder's contents
  toft store mv [-r] <from> <to>         Move: copy, then remove each source once its
                                       copy has landed. Renames within a store too
  toft store mkdir <store>/<folder>      Create an empty folder
  toft store rm [-r] <store>/<name>      Delete one file, or with -r a folder and
                                       everything under it (needs full access)
  toft store put <store> <file>          Upload one file
  toft store get <store>/<name> [dest]   Download one, to dest (a file, or a folder
                                       it goes into) or under its own name here.
                                       Or: get <store> <name> [-o dest]

Examples:
  toft store cp -r ./out toft://reports/2026/march/
  toft store cp -r toft://reports/2026/ ./reports
  toft store mv toft://reports/draft.csv toft://reports/final.csv
  toft store rm -r reports/2025/

Flags:
  -r, --recursive     The whole folder
  -j, --jobs <n>      Transfers at once for -r (default 4)
  --force             Replace local files a download would overwrite (get, cp, mv)
  --as <name>         put: the name to store it under. Otherwise the file's own.
  -o, --out <file>    get: where to write a download. Otherwise the object's name.
  --yes               Answer the confirmation for rm and for moving local files
  --json              Machine-readable output on stdout

Flags may come before or after the arguments. Every path is printed as
toft://<store>/<path>, which every subcommand here takes back. <store>/<path>
works too; for cp and mv, written with toft:// on neither side, it is a store
path only when nothing here is named <store> — a local folder wins.

A folder is a prefix: it exists while something is under it, and mkdir writes
the zero-byte marker that keeps an empty one. There is no rename in an object
store, so mv is copy-then-delete per object; a move that fails partway leaves
both copies of what it had not finished, never neither. A copy between two store
paths passes through this machine (TMPDIR), never through the install.

An administrator grants you access to a store; there is no self-service. What
you may do is one of three levels:

  read        download and list
  write       upload new objects, never replace one
  full        replace and delete as well

A partitioned store takes writes only inside the folders apps are mapped to;
` + "`toft store ls`" + ` names them, or says that none is mapped yet.

Transfers go directly to the object store, not through the install.
`

func cmdStore(e *env, args []string) error {
	if len(args) == 0 {
		ui.Help(os.Stderr, storeUsage)
		return usageCodef("missing_argument", "toft store needs a subcommand: ls, cp, mv, mkdir, rm, put or get")
	}
	switch args[0] {
	case "-h", "--help", "help":
		ui.Help(os.Stdout, storeUsage)
		return nil
	case "ls":
		return cmdStoreList(e, args[1:])
	case "put":
		return cmdStorePut(e, args[1:])
	case "get":
		return cmdStoreGet(e, args[1:])
	case "rm":
		return cmdStoreRemove(e, args[1:])
	case "cp":
		return cmdStoreCopy(e, args[1:])
	case "mv":
		return cmdStoreMove(e, args[1:])
	case "mkdir":
		return cmdStoreMkdir(e, args[1:])
	}
	return unknownSubcommand("store", args, "ls, cp, mv, mkdir, rm, put or get")
}

// --- The wire ---

type storeSummary struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Folder is which directory of the store this row is about, and empty is
	// the whole store. **A slug can appear twice**: reach is per scope, so
	// somebody granted two folders of one store has two rows, at two levels.
	Folder string `json:"folder"`
	Access string `json:"access"`
	// Partitioned stores take writes only in WritableFolders — the folders
	// apps are mapped to that this row reaches. An install older than the
	// fields sends neither, and the row reads as it always did.
	Partitioned     bool     `json:"partitioned,omitempty"`
	WritableFolders []string `json:"writable_folders,omitempty"`
}

type storeObject struct {
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
}

type storeObjectPage struct {
	Objects []storeObject `json:"objects"`
	// Prefixes are the folders one level below what was listed, named relative
	// to it and keeping the trailing slash that is the only thing separating
	// `backup/` the folder from `backup` the object.
	//
	// **Declared because the API has always sent them and this decoded them
	// away.** An app that archives under a generated name — a timestamp, a run
	// id — put its objects somewhere `toft store ls` could not show and could not
	// be pointed at, so the only way to reach one was to type its exact name,
	// which is the one thing the operator did not have.
	Prefixes   []string `json:"prefixes"`
	NextCursor string   `json:"next_cursor"`
}

// signedRequest is what the install answers with: a URL, a verb, and the exact
// headers to send.
//
// **The headers are sent verbatim and nothing is added.** They are covered by
// the signature, so a client that drops one or invents one gets a 403 from the
// object store rather than a different outcome — which is what makes a signed
// condition a condition. `write` access, for instance, arrives as
// `If-None-Match: *`, and sending the request without it is not "uploading
// anyway", it is an invalid signature.
type signedRequest struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	Key       string            `json:"key"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// --- ls ---

func cmdStoreList(e *env, args []string) error {
	var g globalFlags
	var recursive bool
	fs := newFlags("store ls", storeUsage)
	g.register(fs)
	fs.BoolVar(&recursive, "r", false, "everything beneath")
	fs.BoolVar(&recursive, "recursive", false, "everything beneath")
	if err := parseOperands(fs, args, 1); err != nil {
		return skipHelp(err)
	}
	s, err := e.setup(&g)
	if err != nil {
		return err
	}

	if fs.NArg() == 0 {
		return listStores(e, s, &g)
	}
	ref, err := parseRemote(fs.Arg(0))
	if err != nil {
		return err
	}
	typedFolder := ref.dir
	ref.dir = true
	if recursive {
		return listTree(e, s, &g, ref)
	}
	return listObjects(e, s, &g, ref, typedFolder)
}

func listStores(e *env, s *session, g *globalFlags) error {
	var body struct {
		Stores []storeSummary `json:"stores"`
	}
	if err := s.client.Do(http.MethodGet, "/api/v1/store", nil, &body); err != nil {
		return storeError(err, "")
	}

	if g.json {
		return writeNDJSON(e.out.Stdout, body.Stores)
	}
	if len(body.Stores) == 0 {
		// Not an error: having no grant is a normal state, and the sentence
		// that follows is the one that gets somebody unstuck.
		e.out.Answer("No stores. An administrator grants access to a store on its " +
			"page under Admin → Storage.")
		return nil
	}
	if e.out.Rich() {
		printStoresRich(e, body.Stores)
		return nil
	}
	for _, st := range body.Stores {
		// The scope, spelled the way a path is typed: `reports/analytics/`. A
		// bare slug is the whole store, which is what every row was before a
		// grant could name a folder.
		where := remoteRef{store: st.Slug, path: strings.Trim(st.Folder, "/"), dir: true}.String()
		// The name is the first column already — it is what this command
		// takes — so the third is what the store is for, in the
		// administrator's words, first line only.
		about, _, _ := strings.Cut(st.Description, "\n")
		e.out.Answer("%-32s %-10s %s", e.out.Bold(where), st.Access, e.out.Dimmed(about))
		// A level is not the whole answer on a partitioned store: `full` on
		// one with no folder mapped writes nowhere, and saying so here is
		// what stops a person finding out from a refusal.
		if st.Partitioned && st.Access != "read" {
			if len(st.WritableFolders) == 0 {
				e.out.Answer("%-32s %s", "", e.out.Dimmed("partitioned, and no folder of it is mapped to an app — writes are refused"))
				continue
			}
			var in []string
			for _, f := range st.WritableFolders {
				in = append(in, remoteRef{store: st.Slug, path: f, dir: true}.String())
			}
			e.out.Answer("%-32s %s", "", e.out.Dimmed("partitioned — writes only in "+strings.Join(in, ", ")))
		}
	}
	return nil
}

// listObjects shows one level of one store: its folders, then its objects.
//
// **A level, not the whole store.** The argument is `store` or
// `store/folder/…`, which is the same spelling `toft store ls` prints for a grant
// scoped to a folder — so the form somebody reads off one command is the form
// the other takes. Splitting it here rather than sending it whole is what keeps
// the API's rule intact: the caller supplies a path, never a key, and the
// store's extent stays something a client can neither see nor address around.
//
// typedFolder is whether the path was written with its trailing slash. One
// without it that lists nothing may be naming an object, and that is said —
// exit 2, is_an_object — rather than printed as an empty folder (M1).
func listObjects(e *env, s *session, g *globalFlags, ref remoteRef, typedFolder bool) error {
	store, folder := ref.store, ref.path

	var objects []storeObject
	var folders []string
	seen := map[string]bool{}
	cursor := ""
	for {
		query := url.Values{}
		if folder != "" {
			query.Set("path", folder)
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		path := "/api/v1/store/" + url.PathEscape(store) + "/objects"
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
		var page storeObjectPage
		// The store, not what was typed: a 404 here is the *store* being
		// unreachable, and a folder inside one answers 403 with the API's own
		// sentence naming what the caller does hold.
		if err := s.client.Do(http.MethodGet, path, nil, &page); err != nil {
			return storeError(err, store)
		}
		objects = append(objects, page.Objects...)
		// Deduplicated across pages: a folder with more objects than one page
		// holds is reported on each of them, and listing it twice would read as
		// two folders.
		for _, prefix := range page.Prefixes {
			if !seen[prefix] {
				seen[prefix] = true
				folders = append(folders, prefix)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(objects) == 0 && len(folders) == 0 && !typedFolder && folder != "" {
		parent, name := "", folder
		if i := strings.LastIndex(folder, "/"); i >= 0 {
			parent, name = folder[:i], folder[i+1:]
		}
		found, err := objectInFolder(s, store, parent, name)
		if err != nil {
			return err
		}
		if found {
			up := remoteRef{store: store, path: parent, dir: true}
			obj := remoteRef{store: store, path: folder}
			return usageHinted("is_an_object",
				fmt.Sprintf("`toft store ls %s` lists the folder it is in; `toft store get %s .` fetches it", up, obj),
				"%s is an object, not a folder", obj)
		}
	}

	if g.json {
		// An object rather than the bare array this used to print, because
		// there are now two kinds of row and a folder is not an object with
		// fields missing. `.objects` is the array that was here before.
		return encodeListing(e, folders, objects)
	}
	if len(objects) == 0 && len(folders) == 0 {
		e.out.Answer("Nothing in %s yet.", ref)
		return nil
	}
	// Folders first, and with the size and date columns left empty rather than
	// filled with something invented: a prefix has neither. **Every row is the
	// whole toft:// path**, folders and files alike, so any line is something
	// the next command takes as it stands.
	if e.out.Rich() {
		var rows []storeRow
		for _, name := range folders {
			rows = append(rows, storeRow{path: remoteScheme + store + "/" + ref.join(name), dir: true})
		}
		for _, obj := range objects {
			rows = append(rows, storeRow{path: remoteScheme + store + "/" + ref.join(obj.Name),
				size: obj.Size, modified: obj.LastModified})
		}
		printStoreRows(e, rows)
		return nil
	}
	for _, name := range folders {
		e.out.Answer("%12s  %-16s  %s", "", "", e.out.Bold(listedName(remoteScheme+store+"/"+ref.join(name))))
	}
	for _, obj := range objects {
		e.out.Answer("%12s  %s  %s", ui.FormatBytes(obj.Size),
			e.out.Dimmed(obj.LastModified.Local().Format("2006-01-02 15:04 MST")), listedName(remoteScheme+store+"/"+ref.join(obj.Name)))
	}
	return nil
}

// listedName is an object or folder name as a listing prints it: every
// control character replaced with `?`. A name created through an app's storage
// mount can hold an ESC, which the API's own upload refuses and the mount does
// not, and no app's bytes drive the terminal (security review 2026-11-14,
// CS-5). `?` rather than nothing, so two names that differ only there still
// look different; the JSON document keeps the name exact.
func listedName(s string) string {
	control := func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) }
	if !strings.ContainsFunc(s, control) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if control(r) {
			return '?'
		}
		return r
	}, s)
}

// objectInFolder reports whether folder (no slashes at either end; empty is
// the store's top) holds an object called name, paging one level of it.
func objectInFolder(s *session, store, folder, name string) (bool, error) {
	cursor := ""
	for {
		query := url.Values{}
		if folder != "" {
			query.Set("path", folder+"/")
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		path := "/api/v1/store/" + url.PathEscape(store) + "/objects"
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
		var page storeObjectPage
		if err := s.client.Do(http.MethodGet, path, nil, &page); err != nil {
			return false, storeError(err, store)
		}
		for _, o := range page.Objects {
			if o.Name == name {
				return true, nil
			}
		}
		if page.NextCursor == "" {
			return false, nil
		}
		cursor = page.NextCursor
	}
}

// encodeListing is the one --json shape of a listing, one level or -r: an
// object with both arrays, each present and empty rather than null when there
// is nothing to put in it. Names are relative to what was listed.
func encodeListing(e *env, folders []string, objects []storeObject) error {
	if folders == nil {
		folders = []string{}
	}
	if objects == nil {
		objects = []storeObject{}
	}
	return json.NewEncoder(e.out.Stdout).Encode(map[string]any{"folders": folders, "objects": objects})
}

// --- put ---

func cmdStorePut(e *env, args []string) error {
	var g globalFlags
	var as string
	fs := newFlags("store put", storeUsage)
	g.register(fs)
	fs.StringVar(&as, "as", "", "the name to store it under")
	if err := parseOperands(fs, args, -1); err != nil {
		return skipHelp(err)
	}
	if fs.NArg() != 2 {
		return usageCodef("missing_argument", "toft store put <store> <file> [--as name]")
	}
	store, path := strings.TrimSuffix(strings.TrimPrefix(fs.Arg(0), remoteScheme), "/"), fs.Arg(1)
	if strings.Contains(store, "/") {
		// `put toft://s/folder/ file` reads as "into that folder", and is
		// taken that way rather than refused. --as names the object inside
		// that folder, not at the store's root: a user working in their own
		// folder of a shared store must not land in someone else's part of it
		// (BB-088).
		ref, err := parseRemote(store)
		if err != nil {
			return err
		}
		store = ref.store
		if as == "" {
			as = filepath.Base(path)
		}
		as = ref.join(strings.TrimPrefix(as, "/"))
	}

	s, err := e.setup(&g)
	if err != nil {
		return err
	}

	file, err := os.Open(path)
	if err != nil {
		return usageCodef("unreadable_file", "cannot read %s: %v", path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return usageCodef("unreadable_file", "cannot read %s: %v", path, err)
	}
	if info.IsDir() {
		return usageCodef("is_a_folder", "%s is a directory — `toft store cp -r %s toft://%s/` copies what is in it", path, path, store)
	}

	name := as
	if name == "" {
		name = filepath.Base(path)
	}

	// Both digests in one pass over the file. The install signs them into the
	// URL and the object store refuses a body that does not match, so a
	// truncated read here is a failed upload rather than a corrupt object
	// nobody notices until somebody reads it.
	sha, md5sum, err := digests(file)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding %s: %w", path, err)
	}

	var signed signedRequest
	req := map[string]any{"name": name, "size": info.Size(), "sha256": sha, "md5": md5sum}
	if err := s.client.Do(http.MethodPost,
		"/api/v1/store/"+url.PathEscape(store)+"/objects", req, &signed); err != nil {
		return storeError(err, store)
	}

	e.out.Say("Uploading %s to %s (%s)", name, store, ui.FormatBytes(info.Size()))
	// One byte past the size, no further: a file still being written is
	// caught by Go's transport as a body longer than its Content-Length
	// without reading it to its end through the meter (security review
	// 2026-11-14, CS-10).
	err = transfer(e, &signed, metered(e, "upload", io.LimitReader(file, info.Size()+1), info.Size()), info.Size())
	e.out.ClearLine()
	if err != nil {
		return err
	}

	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]any{
			"store": store, "name": name, "object": remoteScheme + store + "/" + name, "size": info.Size(), "sha256": sha,
		})
	}
	e.out.Answer("%s  %s", e.out.OK("uploaded"), remoteRef{store: store, path: name})
	return nil
}

// --- get ---

func cmdStoreGet(e *env, args []string) error {
	var g globalFlags
	var out string
	var force bool
	fs := newFlags("store get", storeUsage)
	g.register(fs)
	fs.StringVar(&out, "out", "", "where to write it")
	fs.StringVar(&out, "o", "", "where to write it")
	fs.BoolVar(&force, "force", false, "replace a local file that already exists")
	if err := parseOperands(fs, args, -1); err != nil {
		return skipHelp(err)
	}
	// Three spellings: `get <store> <name>`, the one path ls prints,
	// `get toft://<store>/<name>`, and that path with where to put it,
	// `get <store>/<name> <dest>` — cp's order. Two words are told apart by the
	// first: a store's name has no slash, so one with a path after its store is
	// the object, and the second word is the destination (drills 2026-10-28
	// #12, where the path was read as a store's name).
	var ref remoteRef
	switch fs.NArg() {
	case 2:
		if objectPathGiven(fs.Arg(0)) {
			r, err := parseRemote(fs.Arg(0))
			if err != nil {
				return err
			}
			if out != "" {
				return usageCodef("unexpected_argument", "%s is where to put %s, and so is -o %s — give the destination once", fs.Arg(1), r, out)
			}
			ref, out = r, fs.Arg(1)
			break
		}
		ref = remoteRef{store: strings.TrimSuffix(strings.TrimPrefix(fs.Arg(0), remoteScheme), "/"), path: fs.Arg(1)}
	case 1:
		r, err := parseRemote(fs.Arg(0))
		if err != nil {
			return err
		}
		ref = r
	default:
		return usageCodef("missing_argument", "toft store get <store>/<name> [dest] [--force], or get <store> <name> [-o dest]")
	}
	if ref.dir || ref.path == "" {
		ref.dir = true
		return folderRefusal(ref)
	}

	if out == "-" && g.json {
		return usageCodef("conflicting_flags", "-o - puts the object on stdout, where --json would put its document — choose one")
	}

	s, err := e.setup(&g)
	if err != nil {
		return err
	}

	if out == "-" {
		_, err := downloadObject(e, s, ref.store, ref.path, "-", false)
		return folderOrMissing(s, ref, err)
	}
	dest := out
	if dest == "" {
		dest = filepath.Base(ref.path)
	} else if strings.HasSuffix(dest, "/") || strings.HasSuffix(dest, string(filepath.Separator)) {
		// Written as a folder, so a folder whether or not it exists yet: the
		// download goes inside it under the object's own name.
		dest = filepath.Join(dest, filepath.Base(ref.path))
	} else if st, err := os.Stat(dest); err == nil && st.IsDir() {
		dest = filepath.Join(dest, filepath.Base(ref.path))
	}
	// Never silently over the top of something, -o included: a download that
	// clobbers a local file is the one mistake here that cannot be undone, so
	// replacing one is --force, the same flag cp and mv take.
	if err := refuseClobber(dest, force); err != nil {
		// A remote folder is is_a_folder whatever is here: the local file
		// was never going to be written, and naming it sent the reader to
		// the wrong side (BB-018, third 2026-11-03 list).
		if isObject, isFolder, lerr := lookupName(s, ref.store, ref.path); lerr == nil && isFolder && !isObject {
			return folderRefusal(ref)
		}
		return err
	}

	written, err := downloadObject(e, s, ref.store, ref.path, dest, true)
	e.out.ClearLine()
	if err != nil {
		return folderOrMissing(s, ref, err)
	}

	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]any{
			"store": ref.store, "name": ref.path, "object": remoteScheme + ref.store + "/" + ref.path, "path": dest, "size": written,
		})
	}
	e.out.Answer("%s  %s (%s)", e.out.OK("downloaded"), dest, ui.FormatBytes(written))
	return nil
}

// objectPathGiven says whether a store argument names a path inside its store
// — `reports/2026/q1.csv`, `toft://reports/q1.csv` — rather than the store
// alone (`reports`, `toft://reports/`).
func objectPathGiven(arg string) bool {
	_, rest, _ := strings.Cut(strings.TrimPrefix(arg, remoteScheme), "/")
	return strings.Trim(rest, "/") != ""
}

// --- rm ---

func cmdStoreRemove(e *env, args []string) error {
	var g globalFlags
	var recursive bool
	var jobs int
	fs := newFlags("store rm", storeUsage)
	g.register(fs)
	fs.BoolVar(&recursive, "r", false, "a folder and everything under it")
	fs.BoolVar(&recursive, "recursive", false, "a folder and everything under it")
	fs.IntVar(&jobs, "j", 4, "deletes at once")
	fs.IntVar(&jobs, "jobs", 4, "deletes at once")
	if err := parseOperands(fs, args, -1); err != nil {
		return skipHelp(err)
	}
	if err := refuseJobs(jobs); err != nil {
		return err
	}
	// Two spellings: `rm <store> <name>`, which is what this always took, and
	// `rm <store>/<name>`, which is what ls prints and cp takes.
	var ref remoteRef
	switch {
	case fs.NArg() == 2 && !recursive:
		ref = remoteRef{store: strings.TrimPrefix(fs.Arg(0), remoteScheme), path: strings.Trim(fs.Arg(1), "/"),
			dir: strings.HasSuffix(fs.Arg(1), "/")}
	case fs.NArg() == 1:
		r, err := parseRemote(fs.Arg(0))
		if err != nil {
			return err
		}
		ref = r
	default:
		return usageCodef("missing_argument", "toft store rm [-r] <store>/<name>")
	}

	s, err := e.setup(&g)
	if err != nil {
		return err
	}
	// **Everything is resolved before anything is asked**: the store, as
	// `toft task delete` resolves its task, and then the name. A confirmation
	// about something that is not there asks a question with no answer, and an
	// object store answers a DELETE of a missing key with the same 204 as a
	// real one — so without this, rm said "deleted" over nothing.
	if err := resolveStore(s, ref.store); err != nil {
		return err
	}
	if recursive {
		return removeTree(e, s, &g, ref, jobs)
	}
	if ref.path == "" {
		return usageCodef("is_a_store", "%s is a whole store — `toft store rm -r %s` deletes what is in it", ref, ref)
	}
	isObject, isFolder, err := lookupName(s, ref.store, ref.path)
	if err != nil {
		return err
	}
	switch {
	case ref.dir || (isFolder && !isObject):
		if !isFolder && !isObject {
			return noSuchStorePath(ref)
		}
		ref.dir = true
		return usageCodef("is_a_folder", "%s is a folder — pass -r to delete it and everything under it", ref)
	case !isObject:
		return noSuchStorePath(ref)
	}

	// Deleting an object is irreversible and the platform keeps no copy, so it
	// is confirmed. `confirm` refuses rather than hanging when nothing can
	// answer, which is what makes `--yes` the unattended path rather than a CI
	// job that waits an hour for a prompt nobody can see.
	if err := confirm(e, &g, fmt.Sprintf("delete %s (the store keeps no copy)", ref)); err != nil {
		return err
	}

	if err := deleteObject(e, s, ref.store, ref.path); err != nil {
		return err
	}

	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]any{"store": ref.store, "name": ref.path,
			"object": remoteScheme + ref.store + "/" + ref.path, "deleted": true})
	}
	e.out.Answer("%s  %s", e.out.OK("deleted"), ref)
	return nil
}

// lookupName answers whether a name is an object, a folder, or both — S3 lets
// `fold` and `fold/x.txt` coexist — from one level of its parent's listing.
//
// A listing, because nothing else answers without side effects: a signed GET
// would be audited as a read, and a DELETE is answered 204 whether or not the
// key existed. Paged until the name turns up or the parent ends.
func lookupName(s *session, store, name string) (isObject, isFolder bool, err error) {
	name = strings.Trim(name, "/")
	parent, base := gopath.Split(name)
	parent = strings.TrimSuffix(parent, "/")
	cursor := ""
	for {
		query := url.Values{}
		if parent != "" {
			query.Set("path", parent+"/")
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		path := "/api/v1/store/" + url.PathEscape(store) + "/objects"
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
		var page storeObjectPage
		if err := s.client.Do(http.MethodGet, path, nil, &page); err != nil {
			return false, false, storeError(err, store)
		}
		for _, o := range page.Objects {
			if o.Name == base {
				isObject = true
			}
		}
		for _, p := range page.Prefixes {
			if strings.TrimSuffix(p, "/") == base {
				isFolder = true
			}
		}
		if (isObject && isFolder) || page.NextCursor == "" {
			return isObject, isFolder, nil
		}
		cursor = page.NextCursor
	}
}

// folderRefusal is a folder named where an object was wanted — get, or cp
// and mv without -r. One code everywhere, is_a_folder, as rm has always said.
func folderRefusal(ref remoteRef) error {
	ref.dir = true
	return usageCodef("is_a_folder", "%s is a folder — `toft store cp -r %s .` copies what is in it", ref, ref)
}

// folderOrMissing turns a download's not_found into is_a_folder when the name
// is a folder written without its trailing slash (BB-017): `get s/dir` is
// a folder, not nothing. Any other error is returned as it was.
func folderOrMissing(s *session, ref remoteRef, err error) error {
	if refusalCode(err) != "not_found" || ref.path == "" {
		return err
	}
	if isObject, isFolder, lerr := lookupName(s, ref.store, ref.path); lerr == nil && isFolder && !isObject {
		return folderRefusal(ref)
	}
	return err
}

// noSuchStorePath is a name that resolves to nothing: exit 2, `not_found`, and the
// listing that would have shown what is there.
func noSuchStorePath(ref remoteRef) error {
	parent := ref
	parent.path, parent.dir = strings.TrimSuffix(gopath.Dir(ref.path), "."), true
	parent.path = strings.Trim(parent.path, "/")
	return usageCodef("not_found", "no such object or folder: %s — `toft store ls %s` lists what is there", ref, parent)
}

// --- The object store, directly ---

// transfer performs the signed request against the object store.
//
// The headers are sent exactly as they arrived, and nothing else is set: they
// are signed, so an extra one is as fatal as a missing one.
//
// An empty body is sent as http.NoBody whatever the caller passed: net/http
// reads a non-nil body with ContentLength 0 as a length it does not know and
// sends it chunked, which S3 refuses for a presigned PUT with a 501 — so an
// empty file could not be uploaded at all.
func transfer(e *env, signed *signedRequest, body io.Reader, size int64) error {
	if size == 0 {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(e.ctx, signed.Method, signed.URL, body)
	if err != nil {
		return fmt.Errorf("building the upload request: %w", err)
	}
	applySignedHeaders(req, signed)
	req.ContentLength = size

	resp, err := storeHTTP().Do(req)
	if err != nil {
		return unreachableStore(signed.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return objectStoreRefusal(resp)
	}
	return nil
}

// fetch is the body of a signed GET and its length, -1 when the store did not
// say.
func fetch(e *env, signed *signedRequest) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(e.ctx, signed.Method, signed.URL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("building the download request: %w", err)
	}
	applySignedHeaders(req, signed)

	resp, err := storeHTTP().Do(req)
	if err != nil {
		return nil, 0, unreachableStore(signed.URL, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, 0, objectStoreRefusal(resp)
	}
	return resp.Body, resp.ContentLength, nil
}

// metered is r drawing the byte meter as it is read, on a live run and when
// the size is known; otherwise r itself. The meter a deploy's upload has, for
// the single put or get a person sits through.
func metered(e *env, label string, r io.Reader, size int64) io.Reader {
	if !e.out.Live() || size <= 0 {
		return r
	}
	return &meterReader{r: r, out: e.out, label: label, total: size}
}

// meterReader redraws at most every meterEvery: a read is 32 KB, and a
// gigabyte read through a line repainted each time is thirty thousand
// repaints nobody can see.
type meterReader struct {
	r           io.Reader
	out         *ui.Out
	label       string
	sent, total int64
	drawn       time.Time
}

const meterEvery = 100 * time.Millisecond

func (m *meterReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	m.sent += int64(n)
	if now := time.Now(); now.Sub(m.drawn) >= meterEvery || m.sent >= m.total {
		m.drawn = now
		m.out.Meter(m.label, m.sent, m.total)
	}
	return n, err
}

func applySignedHeaders(req *http.Request, signed *signedRequest) {
	for name, value := range signed.Headers {
		// Host is not a header on a Go request — net/http takes it from the URL
		// — and setting it as one sends it twice.
		if strings.EqualFold(name, "Host") {
			continue
		}
		req.Header.Set(name, value)
	}
}

// storeHTTP is a client with no overall timeout: a large upload over a slow
// link is a large upload, not a hang. The context still cancels it on Ctrl-C.
func storeHTTP() *http.Client {
	return &http.Client{Transport: http.DefaultTransport}
}

// unreachableStore names the host, because this is the failure that gets
// misdiagnosed.
//
// The bytes go straight to the object store, so a network that reaches the
// install does not necessarily reach the store — a proxy-only corporate network
// is the ordinary case. "Upload failed" sends somebody to ask why Toft is
// broken; naming the host they cannot reach sends them to the right team.
func unreachableStore(rawURL string, err error) error {
	host := rawURL
	if u, perr := url.Parse(rawURL); perr == nil {
		host = u.Host
	}
	// **Never the URL itself.** A *url.Error prints `Put "<url>": …`, and this
	// URL is presigned: its query is a signature, a credential id and a session
	// token that write this key for the next hour. Printed, it landed in every
	// CI log that could not reach the bucket — the thing the platform masks
	// out of every job log (docs/invariants.md § Automation; docs/bugs
	// 2026-10-22-e2e-storage #2). The transport's own error and the host say
	// all a reader needs.
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = fmt.Errorf("%s %s: %w", strings.ToLower(uerr.Op), host, uerr.Err)
	}
	return &client.UnreachableError{
		URL: host,
		Err: fmt.Errorf("%w — the transfer goes directly to the object store rather than "+
			"through the install, so this is %s that could not be reached, not Toft", err, host),
	}
}

// objectStoreRefusal turns the object store's own answer into a sentence.
//
// Two of these are ordinary and worth naming: 412 is the `write` level refusing
// to replace an object that already exists, and 400 with a digest complaint is
// the file having changed underneath the upload.
func objectStoreRefusal(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusPreconditionFailed:
		// A refusal by access level, exit 3 — as `rm` at `write` is.
		return refusedCodef("already_exists", "that name already exists in this store, and your access is `write`, "+
			"which adds new objects and never replaces one — upload it under another name, "+
			"or ask an administrator for `full`")
	case http.StatusNotFound:
		return errObjectMissing
	case http.StatusForbidden:
		return fmt.Errorf("the store refused the signed request. If this took a while to " +
			"start, the URL may have expired — try again")
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		// The store is pacing this caller — the host data plane holds four
		// transfers per person at once. A later try succeeds, so it is
		// retryable, and a batch backs off and retries it itself (M2).
		return &failedError{code: "store_busy", retryable: true,
			msg:  fmt.Sprintf("the store answered %d: %s", resp.StatusCode, summarizeXML(body)),
			hint: "retry in a moment, or pass a lower -j: the store paces how many transfers one person has open at once"}
	case http.StatusBadRequest:
		if bytes.Contains(bytes.ToLower(body), []byte("digest")) || bytes.Contains(body, []byte("Md5")) {
			return fmt.Errorf("the store rejected the file's checksum — the file changed " +
				"while it was being uploaded")
		}
	}
	return fmt.Errorf("the store answered %d: %s", resp.StatusCode, summarizeXML(body))
}

// errObjectMissing is the store's 404 for a signed request — a bucket's
// NoSuchKey and the host data plane's not-found alike. A caller that knows the
// name turns it into noSuchName; one that does not still gets exit 2.
var errObjectMissing = usageCodef("not_found", "no such object in the store")

// storeError maps the *install's* refusals onto this client's exit codes.
//
// 404 is the interesting one and it is deliberately ambiguous on the server:
// "no such store" and "you have no grant on it" are the same answer, so that
// the route cannot be used to enumerate an install's stores. The message says
// both rather than picking one.
func storeError(err error, store string) error {
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	// Exit 2 with the install's code, both of them: a store name that does not
	// resolve is the name rule (nothing was attempted), and an install with no
	// uploads is configuration.
	if apiErr.NotFound() {
		code := orDefault(apiErr.Code, "not_found")
		// The install says which absence it is; an older one does not, and
		// then only the list call (no store named) can tell.
		if store == "" || code == "store_uploads_off" {
			return usageCodef(code, "this install does not offer store uploads")
		}
		return usageCodef(code, "no store called %q that you can reach — it may not exist, or you may "+
			"have no access to it. `toft store ls` lists the ones you do", store)
	}
	// A name or path the install would not take — `../../etc/passwd`, a
	// trailing slash on an object — is the argument being wrong: exit 2, with
	// the install's own sentence and code.
	if apiErr.Status == http.StatusBadRequest {
		return usageCodef(orDefault(apiErr.Code, "bad_request"), "%s", apiErr.Message)
	}
	// A write beside a partitioned resource's mapped folders is a refusal of
	// where, not of who: kind refused, as already_exists and
	// insufficient_access are, rather than the 403's auth (drills 2026-10-30).
	// A level too low for the verb — put at read, rm at write — arrives as a
	// 403 the envelope codes only "forbidden", without a hint. It is the same
	// refusal mv makes up front, so it carries mv's code and a next step
	// (BB-015).
	if apiErr.Status == http.StatusForbidden && (apiErr.Code == "" || apiErr.Code == "forbidden") {
		hint := apiErr.Hint
		if hint == "" {
			hint = "ask an administrator for a higher level on this store; `toft store ls` shows your level on each"
		}
		return &authError{msg: apiErr.Error(), code: "insufficient_access", refused: true, hint: hint}
	}
	// The same refusal from an install that codes it itself, with its own
	// hint: still kind refused, as mv's up-front check says it — it fell
	// through to the 403's auth, so put and rm said auth and mv refused for
	// one rule (BB-106, 2026-11-04 black-box list).
	if apiErr.Code == "insufficient_access" {
		return &authError{msg: apiErr.Error(), code: apiErr.Code, refused: true, from: apiErr}
	}
	if apiErr.Code == "outside_mapped_folders" {
		return &authError{msg: apiErr.Message, code: apiErr.Code, refused: true, from: apiErr}
	}
	// An install older than the 403 answered a partitioned store's write
	// beside its folders with 409, the only 409 these routes have. It is a
	// refusal, exit 3, as the 403 that replaced it is.
	if apiErr.Status == http.StatusConflict {
		return authf("%s", apiErr.Error())
	}
	return err
}

// resolveStore refuses a store this credential cannot reach, by the same rule and
// in the same words as the install's 404 on it.
func resolveStore(s *session, store string) error {
	var body struct {
		Stores []storeSummary `json:"stores"`
	}
	if err := s.client.Do(http.MethodGet, "/api/v1/store", nil, &body); err != nil {
		return storeError(err, "")
	}
	for _, st := range body.Stores {
		if st.Slug == store {
			return nil
		}
	}
	return usageCodef("not_found", "no store called %q that you can reach — it may not exist, or you may "+
		"have no access to it. `toft store ls` lists the ones you do", store)
}

// --- Small helpers ---

// digests reads the file once and returns both, hex-encoded.
//
// Two, because they do different jobs: the SHA-256 is what AWS stores beside
// the object so it can be verified later, and the MD5 is what every
// S3-compatible server actually refuses a mismatched body on.
func digests(r io.Reader) (sha, md5sum string, err error) {
	s := sha256.New()
	m := md5.New()
	if _, err := io.Copy(io.MultiWriter(s, m), r); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(s.Sum(nil)), hex.EncodeToString(m.Sum(nil)), nil
}

// escapeObjectName escapes each segment but keeps the slashes, because the
// route's wildcard is a path and the name's own separators are part of it.
func escapeObjectName(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// summarizeXML pulls the <Message> out of an S3 error body, which is otherwise
// forty lines of namespace declarations around one sentence.
func summarizeXML(body []byte) string {
	text := string(body)
	if start := strings.Index(text, "<Message>"); start >= 0 {
		rest := text[start+len("<Message>"):]
		if end := strings.Index(rest, "</Message>"); end >= 0 {
			return rest[:end]
		}
	}
	text = strings.TrimSpace(text)
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}

// printStoresRich is `toft store ls` with no argument, drawn for a person: the
// grants as a table, and a partitioned store's caveat dimmed under its row.
func printStoresRich(e *env, stores []storeSummary) {
	t := e.out.NewTable("store", "access", "about").Flex(2)
	var notes []string
	for _, st := range stores {
		where := remoteRef{store: st.Slug, path: strings.Trim(st.Folder, "/"), dir: true}.String()
		about, _, _ := strings.Cut(st.Description, "\n")
		access := st.Access
		if access == "read" {
			access = e.out.Dimmed(access)
		} else {
			access = e.out.OK(access)
		}
		t.Row(e.out.Bold(where), access, e.out.Dimmed(about))
		if st.Partitioned && st.Access != "read" {
			if len(st.WritableFolders) == 0 {
				notes = append(notes, where+" is partitioned, and no folder of it is mapped to an app — writes are refused")
				continue
			}
			var in []string
			for _, f := range st.WritableFolders {
				in = append(in, remoteRef{store: st.Slug, path: f, dir: true}.String())
			}
			notes = append(notes, where+" is partitioned — writes only in "+strings.Join(in, ", "))
		}
	}
	t.Print()
	if len(notes) > 0 {
		e.out.Answer("")
	}
	for _, n := range notes {
		e.out.Answer("%s %s", e.out.Caution("▲"), e.out.Faint(e.out.Wrap(n, 2)))
	}
}

// storeRow is one line of a listing, a folder or an object.
type storeRow struct {
	path     string
	dir      bool
	size     int64
	modified time.Time
}

// printStoreRows is a listing drawn for a person: the size right-aligned so
// the magnitudes line up, the age as a glance, and folders in the accent with
// no size or date — a prefix has neither, and nothing is invented for it. The
// path stays whole, the column a person copies from.
func printStoreRows(e *env, rows []storeRow) {
	sizes := make([]string, len(rows))
	width := len("size") // so the header sits over the right edge too
	for i, r := range rows {
		if !r.dir {
			sizes[i] = ui.FormatBytes(r.size)
			width = max(width, len(sizes[i]))
		}
	}
	t := e.out.NewTable("size", "modified", "path").Flex(2)
	for i, r := range rows {
		r.path = listedName(r.path)
		if r.dir {
			t.Row(strings.Repeat(" ", max(width-1, 0))+e.out.Dimmed("—"), e.out.Dimmed("—"),
				e.out.Bold(e.out.Accent(r.path)))
			continue
		}
		t.Row(fmt.Sprintf("%*s", width, sizes[i]), e.out.Dimmed(ui.Ago(r.modified)), r.path)
	}
	t.Print()
}
