package toft

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// WriteOutput asks the platform for an output URL at the moment it writes, so
// a run longer than the bootstrap URL's life can still write; it falls back to
// TOFT_OUTPUT_URI when the platform does not answer. Every PUT carries Azure
// Blob's x-ms-blob-type, which S3 ignores (AS-2, AS-4).
func TestWriteOutputMintsItsURLAtWriteTimeAndFallsBack(t *testing.T) {
	var mu sync.Mutex
	puts := map[string]string{}
	mint := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == outputURLPath:
			if !mint || r.Header.Get("Authorization") != "Bearer run-token" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"url": "http://" + r.Host + "/fresh"})
		case r.Method == http.MethodPut:
			puts[r.URL.Path] = r.Header.Get("x-ms-blob-type")
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()

	run := &Run{OutputURI: srv.URL + "/stale", base: srv.URL, token: "run-token", client: srv.Client()}
	if err := run.WriteOutput(context.Background(), []byte("x")); err != nil {
		t.Fatalf("WriteOutput: %v", err)
	}
	mint = false
	if err := run.WriteOutput(context.Background(), []byte("x")); err != nil {
		t.Fatalf("WriteOutput (fallback): %v", err)
	}
	if puts["/fresh"] != "BlockBlob" {
		t.Errorf("the minted URL was not used, or without the blob type: %v", puts)
	}
	if puts["/stale"] != "BlockBlob" {
		t.Errorf("TOFT_OUTPUT_URI was not the fallback, or without the blob type: %v", puts)
	}
}
