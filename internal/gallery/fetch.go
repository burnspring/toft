package gallery

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"toft/internal/safefetch"
)

// The bounds on anything fetched from the catalog's world. A document this
// platform did not write is read through a limited reader, never io.ReadAll.
const (
	MaxCatalogBytes = 1 << 20   // 1 MB
	MaxZipBytes     = 100 << 20 // 100 MB, matching the human upload route
	fetchTimeout    = 15 * time.Second
)

// CheckSourceURL is the hygiene every URL *inside* the catalog must pass
// before this server will fetch it.
//
// This is not the trust model — the trust model is TLS and the fact that the
// Toft team owns the repository (ADR §28 D3). This is input handling, and
// it lives in internal/safefetch because it is not the gallery's question: an
// advisory feed endpoint and a mirror an operator typed are server-side input
// in exactly the same way, and two guards that could disagree about what "this
// network" means is a bug nobody finds until it matters.
func CheckSourceURL(raw string) error { return safefetch.CheckURL(raw) }

// Fetcher fetches the catalog and the files it names.
type Fetcher struct {
	client *safefetch.Client
}

// NewFetcher builds a fetcher. catalogURL is the *configured* URL; when it
// names a host on this network — an internal mirror — that one host is
// reachable and nothing else is.
func NewFetcher(catalogURL string) *Fetcher {
	return &Fetcher{client: safefetch.New(catalogURL, fetchTimeout)}
}

// Result is one catalog fetch.
type Result struct {
	NotModified bool
	Body        []byte
	ETag        string
}

// FetchCatalog reads the catalog document, conditionally when an ETag is
// known. A 304 is a successful fetch with nothing to parse — the cached
// document is still current, and only its age moves.
func (f *Fetcher) FetchCatalog(ctx context.Context, rawURL, etag string) (Result, error) {
	if err := f.client.CheckConfigured(rawURL); err != nil {
		return Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Toft")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := f.client.For(rawURL).Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return Result{NotModified: true, ETag: etag}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("catalog returned %s", resp.Status)
	}

	body, err := safefetch.ReadLimited(resp.Body, MaxCatalogBytes, "catalog")
	if err != nil {
		return Result{}, err
	}
	return Result{Body: body, ETag: resp.Header.Get("ETag")}, nil
}

// FetchTo streams a file the catalog names into w, refusing anything past max.
// Used for example ZIPs, which are written to a temporary file rather than
// held in memory.
func (f *Fetcher) FetchTo(ctx context.Context, rawURL string, max int64, w io.Writer) (int64, error) {
	if err := safefetch.CheckURL(rawURL); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Toft")

	resp, err := f.client.For(rawURL).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download returned %s", resp.Status)
	}
	if resp.ContentLength > max {
		return 0, fmt.Errorf("file is %d bytes; the limit is %d", resp.ContentLength, max)
	}
	return safefetch.CopyLimited(w, resp.Body, max, "file")
}
