package advisory

import (
	"archive/zip"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"toft/internal/safefetch"
)

// The feed: bulk snapshots, downloaded and matched locally.
//
// OSV offers a query API, and using it would mean POSTing the name and version
// of every package on every internal app to a third party, continuously. For a
// product whose customers put it on-premises specifically so their internal
// tools stay internal, that is the wrong default, and it is a fact about the
// feature that no amount of copy would make comfortable. So the platform
// downloads the advisories and does the matching itself, and **nothing about
// this install's apps is sent anywhere**.
//
// That sentence belongs in the product copy, because it is the reason a
// security team can enable the feature at all — and it is the sentence the
// API-query design would have made untrue.

const (
	// DefaultEndpoint is where OSV publishes its per-ecosystem snapshots.
	DefaultEndpoint = "https://osv-vulnerabilities.storage.googleapis.com"

	// MaxSnapshotBytes bounds one ecosystem's archive. npm's is the largest by
	// an order of magnitude and is comfortably inside this today; the limit
	// exists so that a feed that starts serving something else cannot fill
	// this box's disk, not because the current figure is close.
	MaxSnapshotBytes = 512 << 20

	// MaxAdvisoryBytes bounds one document inside the archive. A single
	// advisory is a few kilobytes; anything approaching this is not one.
	MaxAdvisoryBytes = 4 << 20

	// fetchTimeout is generous because the payload is tens of megabytes over
	// whatever link an on-premise install has.
	fetchTimeout = 10 * time.Minute
)

// osvEcosystemPath is the path segment OSV publishes each ecosystem under.
// Its spelling is the feed's, not this platform's — "PyPI" there, "pypi" here
// — and this map is the one place the two are reconciled.
//
// It carries every **synced** ecosystem, not every covered one: CRAN's corpus
// is fetched so that its fourteen advisories can be matched, while its silence
// is still not a verdict. See advisory.Synced.
var osvEcosystemPath = map[string]string{
	EcosystemNPM:  "npm",
	EcosystemPyPI: "PyPI",
	EcosystemGo:   "Go",
	EcosystemCRAN: "CRAN",
}

// SnapshotURL is where one ecosystem's archive lives under a given endpoint.
func SnapshotURL(endpoint, ecosystem string) (string, error) {
	path, ok := osvEcosystemPath[ecosystem]
	if !ok {
		return "", fmt.Errorf("no snapshot is published for %q", ecosystem)
	}
	return strings.TrimSuffix(strings.TrimSpace(endpoint), "/") + "/" + path + "/all.zip", nil
}

// Snapshot is one ecosystem's corpus as fetched.
type Snapshot struct {
	Ecosystem string
	// NotModified is a successful fetch with nothing to parse: the stored
	// corpus is still current, and only its age moves.
	NotModified bool
	ETag        string
	Records     []Record
	// Skipped counts documents that parsed but named no ecosystem this
	// platform installs. Reported rather than silent, because a sudden change
	// in it is how a feed reshaping itself becomes visible.
	Skipped int
	// Unreadable counts documents that would not parse at all. Same reason.
	Unreadable int
	// At is the snapshot's own age — the archive's Last-Modified, falling back
	// to now. **The age is the snapshot's, never the fetch's**, so that an
	// unchanged archive served for a month reads as a month old.
	At time.Time
}

// Fetcher downloads snapshots under the platform's SSRF rules.
type Fetcher struct {
	client *safefetch.Client
	// headers is the optional credential for a private mirror, in the
	// `key=value,key=value` form. Never logged and never written to audit.
	headers map[string]string
	// scratch is where the archive is spooled — the disk the release trees
	// are on, never a TMPDIR that is a small tmpfs on some hosts, where a
	// snapshot of hundreds of megabytes fails every sync and leaves the corpus
	// quietly stale. Empty means TMPDIR, which only tests use.
	scratch string
}

// NewFetcher builds a fetcher for one configured endpoint. When the endpoint
// names a host on this network — an operator's own mirror — that one host is
// reachable and nothing else is.
func NewFetcher(endpoint, credential string) *Fetcher {
	return &Fetcher{
		client:  safefetch.New(endpoint, fetchTimeout),
		headers: parseHeaders(credential),
	}
}

// NewFetcherWithClient is NewFetcher with the guarded client supplied, for
// tests that stand a server on loopback.
func NewFetcherWithClient(client *safefetch.Client, credential string) *Fetcher {
	return &Fetcher{client: client, headers: parseHeaders(credential)}
}

// parseHeaders reads the `key=value,key=value` form OTEL_EXPORTER_OTLP_HEADERS
// uses, which is the spelling this product already asks administrators for.
func parseHeaders(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		if k = strings.TrimSpace(k); k != "" {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// Fetch downloads and parses one ecosystem's snapshot.
//
// The archive is streamed to a temporary file rather than held in memory: a
// zip reader needs random access, and an install with several ecosystems
// configured should not hold three hundred megabytes of compressed JSON in the
// heap to find out that none of it changed.
func (f *Fetcher) Fetch(ctx context.Context, endpoint, ecosystem, etag string) (Snapshot, error) {
	url, err := SnapshotURL(endpoint, ecosystem)
	if err != nil {
		return Snapshot{}, err
	}
	if err := f.client.CheckConfigured(url); err != nil {
		return Snapshot{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Snapshot{}, err
	}
	req.Header.Set("User-Agent", "Toft")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	for k, v := range f.headers {
		req.Header.Set(k, v)
	}

	resp, err := f.client.For(url).Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return Snapshot{Ecosystem: ecosystem, NotModified: true, ETag: etag}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("the advisory feed returned %s for %s", resp.Status, ecosystem)
	}

	tmp, err := os.CreateTemp(f.scratch, "toft-advisories-*.zip")
	if err != nil {
		return Snapshot{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if _, err := safefetch.CopyLimited(tmp, resp.Body, MaxSnapshotBytes, "the advisory snapshot"); err != nil {
		return Snapshot{}, err
	}

	snap, err := ReadSnapshot(tmp.Name(), ecosystem, sourceName(endpoint))
	if err != nil {
		return Snapshot{}, err
	}
	snap.ETag = resp.Header.Get("ETag")
	snap.At = lastModified(resp.Header.Get("Last-Modified"))
	return snap, nil
}

// affects reports whether a record declares a range in ecosystem — the only
// ranges ReplaceSnapshot writes for it.
func affects(rec Record, ecosystem string) bool {
	for _, a := range rec.Affected {
		if a.Ecosystem == ecosystem {
			return true
		}
	}
	return false
}

// lastByID keeps one record per advisory id, the last one read, in first-seen
// order. Two rows with one conflict key in one INSERT fail the whole swap, and
// two in different chunks double the advisory's ranges, which have no unique
// key (security review 2026-11-14, DG-6) — so the dedupe is here, before both.
func lastByID(records []Record) []Record {
	at := make(map[string]int, len(records))
	out := records[:0]
	for _, r := range records {
		if i, ok := at[r.ID]; ok {
			out[i] = r
			continue
		}
		at[r.ID] = len(out)
		out = append(out, r)
	}
	return out
}

// ReadSnapshot parses an archive already on disk. It is the whole of what an
// air-gapped import shares with a sync — the only thing that differed was
// transport, so everything downstream is identical by construction rather than
// by two code paths agreeing.
func ReadSnapshot(path, ecosystem, source string) (Snapshot, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("the advisory snapshot is not a readable archive: %w", err)
	}
	defer zr.Close()

	snap := Snapshot{Ecosystem: ecosystem, At: time.Now().UTC()}
	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name), ".json") {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			snap.Unreadable++
			continue
		}
		body, err := safefetch.ReadLimited(rc, MaxAdvisoryBytes, "an advisory document")
		rc.Close()
		if err != nil {
			snap.Unreadable++
			continue
		}
		rec, ok, err := ParseOSV(body, source)
		switch {
		case err != nil:
			snap.Unreadable++
		case !ok, !affects(rec, ecosystem):
			// Parsed fine, but names no ecosystem this platform installs — or
			// not this snapshot's: a record that only another ecosystem's
			// ranges would be written for writes nothing here, and counting
			// it made a PyPI archive imported as npm read as a full corpus.
			snap.Skipped++
		default:
			snap.Records = append(snap.Records, rec)
		}
	}
	if len(snap.Records) == 0 && snap.Unreadable > 0 {
		// A corpus that produced nothing but failures is not an empty corpus,
		// and swapping it in would replace a good one with nothing. Better to
		// fail the sync and keep serving what is already stored.
		return Snapshot{}, fmt.Errorf("no advisory in the %s snapshot could be read (%d failed)",
			ecosystem, snap.Unreadable)
	}
	if len(snap.Records) == 0 {
		// No covered ecosystem has zero advisories, so an archive with none
		// for this one — empty, another ecosystem's, or all skipped — is the
		// wrong archive, not an empty corpus. Swapping it in deleted every
		// range and recorded a fresh success that every later poll's 304
		// kept (security review 2026-11-14, DG-3).
		return Snapshot{}, fmt.Errorf("the %s snapshot holds no advisory for %s (%d skipped): "+
			"it is empty or another ecosystem's archive", ecosystem, ecosystem, snap.Skipped)
	}
	if snap.Unreadable > 0 {
		log.Printf("advisory: %d document(s) in the %s snapshot could not be read", snap.Unreadable, ecosystem)
	}
	snap.Records = lastByID(snap.Records)
	return snap, nil
}

// sourceName is what gets recorded on each advisory as where it came from. The
// host is enough to tell an OSV sync from an operator's mirror, and it is not
// a credential.
func sourceName(endpoint string) string {
	e := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	if i := strings.IndexByte(e, '/'); i >= 0 {
		e = e[:i]
	}
	if e == "" {
		return "osv"
	}
	return e
}

func lastModified(header string) time.Time {
	if t, err := http.ParseTime(strings.TrimSpace(header)); err == nil {
		return t.UTC()
	}
	return time.Now().UTC()
}
