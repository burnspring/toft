package builder

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A Posit Connect bundle states its dependencies in `manifest.json`, and this
// is the one place that reads it.
//
// **Why this exists at all.** Everything downstream of a build asks the same
// question of an R release — `internal/deps` for the inventory, the R branch of
// installDependencies for the restore, the runner's repair check for whether a
// library is owed — and all three ask it by looking for `renv.lock`. A bundle
// written by `rsconnect::writeManifest()` has no such file, so all three
// answered "this app declares nothing": the build installed zero packages, said
// so, started the release anyway, and the app died at
// `there is no package called 'shiny'` with a manifest naming shiny sitting
// beside it.
//
// **Why a translation rather than a second restore path.** The manifest is not
// a lockfile renv can read, but it holds the same facts: a flat, fully-resolved
// set of packages with exact versions, each with the repository or the commit
// it came from. So it is converted into an `renv.lock` in the release and
// nothing else changes — one restore, one inventory reader, one repair check,
// and a bundle becomes indistinguishable from a repository that committed its
// lock. A second install path would have been a second place for the caps, the
// repository override and the no-escaping-links rule to drift from.
//
// The lock is written **into the release, never back into the app's source**,
// and only when the author shipped none: a committed `renv.lock` is the
// author's own statement and always wins.

// connectManifest is the part of a Connect bundle's manifest this reads. Every
// other key — `files` and its checksums, `metadata`, `locale`, `users` — is
// ignored rather than rejected, on the same terms as `deps.renvLock`: the file
// is rsconnect's and its shape is rsconnect's to extend.
type connectManifest struct {
	Version     int                       `json:"version"`
	Platform    string                    `json:"platform"`
	Environment connectEnvironment        `json:"environment"`
	Packages    map[string]connectPackage `json:"packages"`
	Metadata    connectMetadata           `json:"metadata"`
}

// connectMetadata is what rsconnect records about which file is the content.
// A bundle of an R Markdown project names its document in `primary_rmd`, and a
// Quarto one in `entrypoint` — the publisher's own answer to "which of these
// documents is the app", which is a question a folder holding a report and the
// email template it renders cannot answer by itself.
type connectMetadata struct {
	AppMode    string `json:"appmode"`
	PrimaryRmd string `json:"primary_rmd"`
	Entrypoint string `json:"entrypoint"`
}

// connectPrimaryDocument is the document a Connect bundle names as its content,
// or "" — only when the manifest is one this platform adopts (readConnectManifest)
// and the file it names is at the release root, where detection looks.
func connectPrimaryDocument(dir string) string {
	m := readConnectManifest(dir)
	if m == nil {
		return ""
	}
	for _, name := range []string{m.Metadata.PrimaryRmd, m.Metadata.Entrypoint} {
		name = strings.TrimSpace(name)
		if name == "" || name != filepath.Base(name) {
			continue
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".rmd", ".qmd":
		default:
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			return name
		}
	}
	return ""
}

// connectDeclaresShiny reports whether a Connect bundle says it is a Shiny
// application — `appmode: shiny`, which rsconnect writes for an app.R or a
// ui.R/server.R pair. It is the bundle's answer to the one question the files
// cannot settle alone: an app that keeps a README.Rmd or a help tab's .Rmd at
// its root is one document candidate to detectRDocument, and would be rendered
// as a report rather than run.
func connectDeclaresShiny(dir string) bool {
	m := readConnectManifest(dir)
	return m != nil && strings.TrimSpace(m.Metadata.AppMode) == "shiny"
}

type connectEnvironment struct {
	// A pointer, because *absent* is the fact this reads. rsconnect-python
	// writes `environment.python` and no `packages` at all, so a bundle with
	// neither this nor an R-shaped `platform` is somebody else's manifest.
	R *struct {
		Requires string `json:"requires"`
	} `json:"r"`
	// Present only in a Python bundle, and decisive when it is: a Python
	// `platform` is `3.12.4`, which is the same shape as an R one, so the
	// version alone cannot tell the two apart.
	Python *json.RawMessage `json:"python"`
}

type connectPackage struct {
	Source     string `json:"Source"`
	Repository string `json:"Repository"`
	// The version lives here rather than beside Source, because rsconnect
	// records the package's whole DESCRIPTION verbatim under this key.
	Description connectDescription `json:"description"`
}

// connectDescription names the DESCRIPTION fields this needs and no others.
//
// Named rather than `map[string]string`, and that is not tidiness: a real
// DESCRIPTION carries `Config/…` fields whose values are not always strings,
// and a map typed to string fails the whole decode on one of them — which would
// turn a bundle that could have deployed into one the platform refuses to read.
type connectDescription struct {
	Package string `json:"Package"`
	Version string `json:"Version"`
	// The Remote* family is what renv restores a non-repository package from,
	// and rsconnect records it under exactly these names. RemoteSha is the
	// reason a GitHub record is reproducible at all.
	RemoteType     string `json:"RemoteType"`
	RemoteHost     string `json:"RemoteHost"`
	RemoteUsername string `json:"RemoteUsername"`
	RemoteRepo     string `json:"RemoteRepo"`
	RemoteRef      string `json:"RemoteRef"`
	RemoteSha      string `json:"RemoteSha"`
	RemoteSubdir   string `json:"RemoteSubdir"`
}

// renvLockFile is the lockfile this writes. Only the keys renv needs to restore
// are emitted: no `Hash`, because the manifest records none and inventing one
// would be a claim about bytes nobody has seen.
type renvLockFile struct {
	R        renvLockR                  `json:"R"`
	Packages map[string]renvLockPackage `json:"Packages"`
}

type renvLockR struct {
	Version      string           `json:"Version"`
	Repositories []renvRepository `json:"Repositories"`
}

type renvRepository struct {
	Name string `json:"Name"`
	URL  string `json:"URL"`
}

type renvLockPackage struct {
	Package        string `json:"Package"`
	Version        string `json:"Version"`
	Source         string `json:"Source"`
	Repository     string `json:"Repository,omitempty"`
	RemoteType     string `json:"RemoteType,omitempty"`
	RemoteHost     string `json:"RemoteHost,omitempty"`
	RemoteUsername string `json:"RemoteUsername,omitempty"`
	RemoteRepo     string `json:"RemoteRepo,omitempty"`
	RemoteRef      string `json:"RemoteRef,omitempty"`
	RemoteSha      string `json:"RemoteSha,omitempty"`
	RemoteSubdir   string `json:"RemoteSubdir,omitempty"`
}

// connectReport is what the translation has to say for itself on the build log.
// A count alone would not survive being checked: an author whose app still
// fails at `library(x)` needs to read which packages the platform could not
// carry across and why, on the deploy that decided it.
type connectReport struct {
	RVersion  string
	Converted int
	// Remote names the packages restored from a commit rather than from the
	// operator's repository. Reported because it is the one class whose install
	// reaches a host `TOFT_R_REPOS` does not name — which is exactly what a
	// committed renv.lock already does with a GitHub record, so it is stated
	// rather than refused.
	Remote []string
	// Skipped names the packages that were left out, each with the reason. A
	// manifest can record a package installed from a local tarball or from a
	// path on the author's machine, and there is nothing to fetch for those.
	Skipped []string
	// Unpinned names the packages whose recorded version the operator's
	// repository does not have, and which are therefore restored at whatever
	// version it does. Its own bucket rather than Skipped, because the package
	// is still installed and the copy has to say the opposite thing.
	Unpinned []string
}

// readConnectManifest returns the R manifest a release carries, or nil.
//
// **`manifest.json` is not a name this platform owns.** A progressive web app
// ships one, and so does half of `web/`; reading either as a dependency
// declaration would be worse than reading neither. So the file has to look like
// what it claims: a `packages` object with something in it, *and* an R signal —
// `environment.r`, which rsconnect writes for every R bundle, or a `platform`
// that is an R version. A file that is not JSON at all is not a Connect
// manifest either, and is passed over in silence rather than failing a deploy
// over somebody else's file.
func readConnectManifest(dir string) *connectManifest {
	data, err := readInRelease(dir, "manifest.json")
	if err != nil {
		return nil
	}
	var m connectManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	if len(m.Packages) == 0 {
		return nil
	}
	if m.Environment.Python != nil {
		return nil
	}
	if m.Environment.R == nil && !looksLikeRVersion(m.Platform) {
		return nil
	}
	return &m
}

// looksLikeRVersion reports whether s is the shape rsconnect writes into
// `platform` for an R bundle — `4.5.2`, and never a word. Deliberately not a
// version comparison: this decides whose file this is, not which R to use.
func looksLikeRVersion(s string) bool {
	if s == "" {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// adoptConnectManifest writes an `renv.lock` derived from a Connect bundle's
// manifest, and reports what it did. A nil report means there was nothing to
// do — no manifest, somebody else's manifest, or a lock the author committed.
//
// The author's lock always wins, and it is checked first rather than merged
// with: a bundle that carries both was snapshotted by renv *and* by rsconnect,
// and renv's file is the one renv wrote.
func adoptConnectManifest(dir, repos string) (*connectReport, error) {
	if hasRenvLock(dir) {
		return nil, nil
	}
	m := readConnectManifest(dir)
	if m == nil {
		return nil, nil
	}

	lock := renvLockFile{
		R:        renvLockR{Version: m.Platform, Repositories: manifestRepositories(m)},
		Packages: make(map[string]renvLockPackage, len(m.Packages)),
	}
	report := &connectReport{RVersion: m.Platform}

	for key, p := range m.Packages {
		name := p.Description.Package
		if name == "" {
			name = key
		}
		if p.Description.Version == "" {
			// Same judgement `deps.resolveCRAN` makes about an unversioned
			// entry: renv cannot ask a repository for "whatever version", so a
			// record with no version is a record that would fail the restore
			// and take every other package with it.
			report.Skipped = append(report.Skipped, name+" (no version recorded)")
			continue
		}
		rec, remote, why := connectRecord(name, p)
		if why != "" {
			report.Skipped = append(report.Skipped, name+" ("+why+")")
			continue
		}
		if remote {
			report.Remote = append(report.Remote, name)
		}
		lock.Packages[name] = rec
		report.Converted++
	}

	reconcileVersions(&lock, repos, report)

	sort.Strings(report.Remote)
	sort.Strings(report.Skipped)
	sort.Strings(report.Unpinned)

	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("building renv.lock from manifest.json: %w", err)
	}
	if err := createInRelease(dir, "renv.lock", append(data, '\n')); err != nil {
		return nil, fmt.Errorf("writing the renv.lock built from manifest.json: %w", err)
	}
	return report, nil
}

// createInRelease writes a new file of the release through an os.Root, and
// only a new one: O_EXCL refuses a name that already exists *including as a
// link*, dangling or not, so a committed `renv.lock -> /somewhere` is never
// written through. This runs before the handover, as root, on every git deploy
// with a manifest.json (security review 2026-11-10, E-4).
func createInRelease(dir, name string, data []byte) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// connectRecord turns one manifest entry into one lock record. The second
// return says whether it is restored from a commit rather than a repository;
// the third is the reason it could not be turned into anything, and is empty
// when the first two are meaningful.
func connectRecord(name string, p connectPackage) (renvLockPackage, bool, string) {
	rec := renvLockPackage{Package: name, Version: p.Description.Version}

	// RemoteType first, not Source. It is what renv itself keys on, and it is
	// the field that carries the commit — a manifest that records
	// `Source: "github"` without one has nothing to fetch, and one that records
	// the Remote family under a Source this reader has never heard of still has
	// everything it needs.
	switch strings.ToLower(p.Description.RemoteType) {
	case "github", "gitlab", "bitbucket":
		d := p.Description
		if d.RemoteUsername == "" || d.RemoteRepo == "" || d.RemoteSha == "" {
			return rec, false, "recorded as " + strings.ToLower(d.RemoteType) +
				" without the username, repository and commit renv needs"
		}
		// renv's own spelling of the source, which is not the lowercase one
		// rsconnect records: renv reads `Source` to pick the downloader.
		rec.Source = map[string]string{
			"github": "GitHub", "gitlab": "GitLab", "bitbucket": "Bitbucket",
		}[strings.ToLower(d.RemoteType)]
		rec.RemoteType = strings.ToLower(d.RemoteType)
		rec.RemoteHost = d.RemoteHost
		rec.RemoteUsername = d.RemoteUsername
		rec.RemoteRepo = d.RemoteRepo
		rec.RemoteRef = d.RemoteRef
		rec.RemoteSha = d.RemoteSha
		rec.RemoteSubdir = d.RemoteSubdir
		return rec, true, ""
	}

	switch strings.ToLower(p.Source) {
	case "cran", "repository", "":
		// `Repository: "CRAN"` is the *name* renv writes, never the URL —
		// which is what lets TOFT_R_REPOS decide where CRAN is. The URL
		// the manifest recorded is the author's machine's answer to that
		// question and is deliberately dropped here; it survives in the
		// lockfile's repository list, where renv treats it as a default rather
		// than as an instruction.
		rec.Source = "Repository"
		rec.Repository = "CRAN"
		return rec, false, ""
	case "bioconductor":
		rec.Source = "Bioconductor"
		return rec, false, ""
	}
	return rec, false, "recorded as " + p.Source + ", which renv cannot restore from"
}

// manifestRepositories collects the distinct repository URLs the manifest
// names, in a stable order.
//
// Written into the lock for fidelity rather than for effect: an install with
// TOFT_R_REPOS set overrides all of them, which is the rule *the packages'
// source is the operator's and their versions are the lock's*. They matter on
// an install that has deliberately left it empty.
func manifestRepositories(m *connectManifest) []renvRepository {
	seen := map[string]bool{}
	var urls []string
	for _, p := range m.Packages {
		if p.Repository == "" || seen[p.Repository] {
			continue
		}
		seen[p.Repository] = true
		urls = append(urls, p.Repository)
	}
	sort.Strings(urls)
	repos := make([]renvRepository, 0, len(urls))
	for i, u := range urls {
		// The first is CRAN by name, because that is the name every record
		// written above points at. Any others are along for fidelity and are
		// named after their position rather than guessed at.
		name := "CRAN"
		if i > 0 {
			name = fmt.Sprintf("repo%d", i+1)
		}
		repos = append(repos, renvRepository{Name: name, URL: u})
	}
	return repos
}

// The bounds on the availability check. Sixteen at a time answers a 192-package
// manifest in about a second and a half against a repository on the internet,
// which is inside the noise of a deploy; one at a time would be three minutes
// and would make this the slowest step in the phase.
const (
	reconcileConcurrency    = 16
	reconcileRequestTimeout = 20 * time.Second
)

// report writes the translation onto the build log.
//
// On the log the author already reads, and in the phase that decided it: this
// is the only place a bundle is told that its dependency declaration was
// understood, and the only place it is told which parts of it were not.
func (r *connectReport) report(lg func(string)) {
	lg("==> No renv.lock, but this is a Posit Connect bundle")
	version := r.RVersion
	if version == "" {
		version = "an unrecorded version"
	} else {
		version = "R " + version
	}
	lg(fmt.Sprintf("    manifest.json names %d package(s) for %s — translated into renv.lock",
		r.Converted, version))
	if len(r.Remote) > 0 {
		lg(fmt.Sprintf("    %d of them are pinned to a commit and are fetched from there, not from "+
			"the configured repository: %s", len(r.Remote), strings.Join(r.Remote, ", ")))
	}
	if len(r.Unpinned) > 0 {
		lg(fmt.Sprintf("    %d of them name a version the configured repository does not have, so "+
			"they are installed at the version it does: %s",
			len(r.Unpinned), strings.Join(r.Unpinned, ", ")))
		lg("    Usually the bundle was built against packages newer than the configured " +
			"repository has — a dated snapshot holds only what was published by its date — or on " +
			"a host that installs R packages from its own distribution, whose version numbers " +
			"are not CRAN's.")
	}
	for _, s := range r.Skipped {
		lg("    Warning: " + s + " — left out, and the app will fail if it uses it")
	}
}

// reconcileVersions drops a pinned version the operator's repository cannot
// supply, so that renv resolves the package instead of dying on it.
//
// **The failure this closes is silent in renv and misleading afterwards.** A
// Connect host running r2u installs its R packages from Debian, and rsconnect
// records the *Debian* version into the manifest: `Rcpp 1.1.1-1`, a string CRAN
// has never published (it has 1.1.1 and 1.1.1-1.1, and no mirror has the one in
// between). renv marks the download ✖, prints "Successfully downloaded 62
// packages" for the other 62, and fails sixty lines later at
// `there is no package called 'Rcpp'` while installing something else entirely.
// Nothing in that sequence names the manifest, and the version in it is not
// wrong for the machine that wrote it.
//
// **A version, not a package.** The record is dropped rather than the package:
// renv then installs it as a dependency at the repository's current version,
// which is what it already does for every package the manifest never mentioned.
// Losing a pin is a real cost and is why this is reported rather than done
// quietly — but the alternative on offer is not the pinned version, it is no
// deploy.
//
// **Only a definite answer counts.** A refusal, a timeout or anything that is
// not a clean 404 leaves the pin alone: dropping pins because a proxy hiccuped
// would turn a network blip into a silently different dependency tree, which is
// far worse than the failure this fixes. An install with no repository
// configured checks nothing at all.
func reconcileVersions(lock *renvLockFile, repos string, report *connectReport) {
	repos = strings.TrimRight(strings.TrimSpace(repos), "/")
	if repos == "" || len(lock.Packages) == 0 {
		return
	}

	type result struct {
		name, version string
		missing       bool
	}
	client := &http.Client{Timeout: reconcileRequestTimeout}
	jobs := make(chan renvLockPackage)
	out := make(chan result)

	var wg sync.WaitGroup
	for i := 0; i < reconcileConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rec := range jobs {
				out <- result{rec.Package, rec.Version,
					!repositoryHas(client, repos, rec.Package, rec.Version)}
			}
		}()
	}
	go func() {
		for _, rec := range lock.Packages {
			// A commit-pinned record is fetched from its host and has no
			// version in this repository to look for.
			if rec.Source != "Repository" || rec.Version == "" {
				continue
			}
			jobs <- rec
		}
		close(jobs)
		wg.Wait()
		close(out)
	}()

	for r := range out {
		if !r.missing {
			continue
		}
		delete(lock.Packages, r.name)
		report.Converted--
		report.Unpinned = append(report.Unpinned, r.name+" "+r.version)
	}
}

// repositoryHas reports whether the repository can serve one exact version.
//
// Both paths, because a repository serves the *current* version of a package
// from src/contrib and every earlier one from Archive, and a Connect manifest
// is old enough that almost everything in it is in the second.
func repositoryHas(client *http.Client, repos, name, version string) bool {
	file := name + "_" + version + ".tar.gz"
	for _, u := range []string{
		repos + "/src/contrib/" + file,
		repos + "/src/contrib/Archive/" + name + "/" + file,
	} {
		req, err := http.NewRequest(http.MethodHead, u, nil)
		if err != nil {
			return true // not a verdict; see reconcileVersions
		}
		// P3M decides source-or-binary by reading the R version out of this,
		// and answers 404 for a version it does not hold either way. Sent so
		// that what is asked here is what the restore will ask.
		req.Header.Set("User-Agent", "Toft (R package availability check)")
		resp, err := client.Do(req)
		if err != nil {
			return true
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			continue
		}
		return true
	}
	return false
}
