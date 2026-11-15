package builder

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Dated snapshots for an R lockfile's old pins (ADR §123).
//
// P3M serves a binary for a package version only from a snapshot in which that
// version was current. `latest` still has an old version — in its archive, as
// source — so a lockfile pinning Rcpp 1.1.1 after 1.1.2 shipped compiled it:
// 1.4 minutes on trytoft, and three such pins made a seven-minute deploy. The
// same version from a snapshot dated while it was current is an unpack.
//
// So before the restore, the build asks which pins the configured repository has
// moved past, asks P3M when each one was current, and hands renv the fewest
// dated snapshots that cover them all, after the configured repository. renv
// takes a version from the first repository whose index carries it as current,
// which is now a dated one with a binary in it. Everything already current comes
// from the configured repository exactly as before.
//
// **Only for a P3M repository, and never fatal.** A mirror the operator runs is
// theirs to date or not; and a P3M that cannot be asked leaves the restore as it
// was, compiling what it compiled. The answer is advice to renv, not a
// constraint: a snapshot that turns out to carry no binary for this
// distribution is one renv passes over.

// p3mHosts are the hosts the planner will talk to. The repository URL is the
// operator's, and the planner makes requests from the platform's own process,
// so it asks only Posit's public service — never wherever the setting points.
var p3mHosts = map[string]bool{"p3m.dev": true, "packagemanager.posit.co": true}

// snapshotLimit is how many dated snapshots one restore is given. Each is one
// more index renv downloads; a lockfile whose pins need more than this has the
// rest compiled, as every pin was before.
const snapshotLimit = 6

// snapshotPlanTimeout bounds the whole of the planning, every request included.
var snapshotPlanTimeout = 20 * time.Second

// p3mRepo splits a P3M binary repository URL —
// https://p3m.dev/cran/__linux__/trixie/latest — into the service's origin and
// the prefix a dated snapshot is appended to. ok is false for anything else.
func p3mRepo(repos string) (origin, prefix string, ok bool) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(repos), "/"))
	if err != nil || u.Scheme != "https" || !p3mHosts[u.Host] || strings.Contains(repos, ";") {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "cran" || parts[1] != "__linux__" || parts[2] == "" {
		return "", "", false
	}
	origin = u.Scheme + "://" + u.Host
	return origin, origin + "/cran/__linux__/" + parts[2] + "/", true
}

// snapshotEntry is one change in a package's P3M history: from Date on, Version
// was the current one.
type snapshotEntry struct {
	Date    string
	Version string
}

// window is when a pinned version was current: from its first snapshot up to,
// and not including, the next change. An empty Until means it still is.
type window struct {
	Package, From, Until string
}

// currentWindow finds when version was current, from a history in any order.
func currentWindow(pkg, version string, history []snapshotEntry) (window, bool) {
	sorted := sortedByDate(history)
	for i, e := range sorted {
		if e.Version != version {
			continue
		}
		w := window{Package: pkg, From: e.Date}
		if i+1 < len(sorted) {
			w.Until = sorted[i+1].Date
		}
		return w, true
	}
	return window{}, false
}

func sortedByDate(h []snapshotEntry) []snapshotEntry {
	out := append([]snapshotEntry(nil), h...)
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// lastDayOf is the latest snapshot date inside w: the day before the next
// change, by which P3M has had the longest to build the binaries.
func lastDayOf(w window) string {
	if w.Until == "" {
		return w.From
	}
	until, err := time.Parse(time.DateOnly, w.Until)
	if err != nil {
		return w.From
	}
	day := until.AddDate(0, 0, -1).Format(time.DateOnly)
	if day < w.From {
		return w.From
	}
	return day
}

// coverDates picks the fewest dates such that every window holds one — the
// classic interval cover, by earliest last day — up to limit.
func coverDates(ws []window, limit int) []string {
	sort.Slice(ws, func(i, j int) bool { return lastDayOf(ws[i]) < lastDayOf(ws[j]) })
	var dates []string
	last := ""
	for _, w := range ws {
		if last != "" && last >= w.From && (w.Until == "" || last < w.Until) {
			continue
		}
		if len(dates) == limit {
			break
		}
		last = lastDayOf(w)
		dates = append(dates, last)
	}
	return dates
}

// lockPins is every package the lockfile installs from a repository, by name.
func lockPins(releaseDir string) map[string]string {
	data, err := readInRelease(releaseDir, "renv.lock")
	if err != nil {
		return nil
	}
	var lock struct {
		Packages map[string]struct {
			Package, Version, Source string
		} `json:"Packages"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return nil
	}
	pins := make(map[string]string, len(lock.Packages))
	for key, p := range lock.Packages {
		name := p.Package
		if name == "" {
			name = key
		}
		if p.Version != "" && (p.Source == "Repository" || p.Source == "") {
			pins[name] = p.Version
		}
	}
	return pins
}

// parsePackagesIndex reads a CRAN-style PACKAGES file into name → version.
func parsePackagesIndex(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	name := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Package: "):
			name = strings.TrimSpace(strings.TrimPrefix(line, "Package: "))
		case strings.HasPrefix(line, "Version: ") && name != "":
			out[name] = strings.TrimSpace(strings.TrimPrefix(line, "Version: "))
			name = ""
		}
	}
	return out
}

// p3mClient talks to one P3M service. Its fields are what a test replaces.
type p3mClient struct {
	http   *http.Client
	origin string
}

func (c p3mClient) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s answered %s", u, resp.Status)
	}
	return resp, nil
}

// index is the repository's current versions.
func (c p3mClient) index(ctx context.Context, repos string) (map[string]string, error) {
	resp, err := c.get(ctx, strings.TrimRight(repos, "/")+"/src/contrib/PACKAGES.gz")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, err
	}
	return parsePackagesIndex(gz), nil
}

// history is every change P3M recorded for one package.
func (c p3mClient) history(ctx context.Context, pkg string) ([]snapshotEntry, error) {
	var out []snapshotEntry
	for page := 1; page <= 10; page++ {
		u := fmt.Sprintf("%s/__api__/repos/cran/packages/%s/history?_limit=200&_page=%d",
			c.origin, url.PathEscape(pkg), page)
		resp, err := c.get(ctx, u)
		if err != nil {
			return nil, err
		}
		var body struct {
			Pages     int `json:"pages"`
			Snapshots []struct {
				Date   string `json:"date"`
				Latest struct {
					Version string `json:"version"`
				} `json:"latest_version"`
			} `json:"snapshots"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, s := range body.Snapshots {
			out = append(out, snapshotEntry{Date: s.Date, Version: s.Latest.Version})
		}
		if page >= body.Pages {
			break
		}
	}
	return out, nil
}

// rRestoreRepos is the repository list the restore of releaseDir is given:
// repos itself, or repos followed by dated P3M snapshots for the pins it has
// moved past, in renv's `name=url;name=url` form. Anything that goes wrong
// returns repos unchanged.
func rRestoreRepos(releaseDir, repos string, say func(string)) string {
	origin, prefix, ok := p3mRepo(repos)
	if !ok {
		return repos
	}
	return planRestoreRepos(releaseDir, repos, prefix,
		p3mClient{http: &http.Client{Timeout: 10 * time.Second}, origin: origin}, say)
}

func planRestoreRepos(releaseDir, repos, prefix string, c p3mClient, say func(string)) string {
	pins := lockPins(releaseDir)
	if len(pins) == 0 {
		return repos
	}
	ctx, cancel := context.WithTimeout(context.Background(), snapshotPlanTimeout)
	defer cancel()

	current, err := c.index(ctx, repos)
	if err != nil {
		say(fmt.Sprintf("    (could not read the R repository's index to look for older binaries: %v)", err))
		return repos
	}
	var behind []string
	for pkg, v := range pins {
		if cur, ok := current[pkg]; ok && cur != v {
			behind = append(behind, pkg)
		}
	}
	if len(behind) == 0 {
		return repos
	}
	sort.Strings(behind)

	var (
		mu      sync.Mutex
		windows []window
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 8)
	)
	for _, pkg := range behind {
		wg.Add(1)
		go func(pkg string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			h, err := c.history(ctx, pkg)
			if err != nil {
				return
			}
			if w, ok := currentWindow(pkg, pins[pkg], h); ok {
				mu.Lock()
				windows = append(windows, w)
				mu.Unlock()
			}
		}(pkg)
	}
	wg.Wait()
	if len(windows) == 0 {
		return repos
	}

	dates := coverDates(windows, snapshotLimit)
	parts := []string{"CRAN=" + repos}
	for _, d := range dates {
		parts = append(parts, "P3M-"+d+"="+prefix+d)
	}
	say(fmt.Sprintf("    %d of the lockfile's packages are older than the repository's current versions; "+
		"adding P3M snapshot(s) %s so they install as binaries rather than compiling.",
		len(behind), strings.Join(dates, ", ")))
	return strings.Join(parts, ";")
}
