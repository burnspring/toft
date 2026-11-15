package artifact

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

// An app's delete removes its runs' outputs, and only its own: the rows that
// held their keys cascade away with the app (AS-3).
func TestDeleteAppRunsRemovesOnlyThatAppsRunPrefix(t *testing.T) {
	s3srv := httptest.NewServer(newFakeS3())
	defer s3srv.Close()
	azsrv := httptest.NewServer(newFakeAzBlob())
	defer azsrv.Close()
	local, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	underneath, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for name, store := range map[string]Store{
		"s3":     newTestS3Store(t, s3srv, "install-a"),
		"azblob": newTestAzBlobStore(t, azsrv, "install-a"),
		"local":  local,
		// Seen through the telemetry decorator, as every …Of helper is.
		"wrapped": instrumented{Store: underneath, backend: "local"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			src := writeTree(t, map[string]string{"out": "x"})
			mine := []string{RunOutputKey("app-1", "r1", "output"), RunOutputKey("app-1", "r2", "output")}
			theirs := RunOutputKey("app-2", "r1", "output")
			release := ReleaseKey("app-1", "d1")
			for _, k := range append(append([]string{}, mine...), theirs, release) {
				if _, err := store.Put(ctx, k, src); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := DeleteAppRuns(ctx, store, "app-1"); err != nil {
				t.Fatalf("DeleteAppRuns: %v", err)
			}
			for _, k := range mine {
				if _, err := store.Stat(ctx, k); !errors.Is(err, ErrNotFound) {
					t.Errorf("%s survived: %v", k, err)
				}
			}
			for _, k := range []string{theirs, release} {
				if _, err := store.Stat(ctx, k); err != nil {
					t.Errorf("%s was removed: %v", k, err)
				}
			}
		})
	}
}

func TestDeleteAppRunsRefusesAnEmptyApp(t *testing.T) {
	local, _ := NewLocalStore(t.TempDir())
	for _, id := range []string{"", "a/b", `..\x`} {
		if _, err := DeleteAppRuns(context.Background(), local, id); err == nil {
			t.Errorf("app id %q was accepted", id)
		}
	}
	if n, err := DeleteAppRuns(context.Background(), nil, "app-1"); n != 0 || err != nil {
		t.Errorf("a nil store = %d, %v", n, err)
	}
}
