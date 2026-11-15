package toft

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// toft.App.URL: the shared join vector (sdk/testdata/app-url-vector.json, read
// by all four SDKs) and Go's half of the fetch rules — asked of
// /api/v1/app/url, reused briefly and then asked again, the last answer used
// when the platform cannot be reached, a refusal returned.

func TestTheSharedAppURLVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "app-url-vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Cases []struct {
			Name      string `json:"name"`
			PublicURL string `json:"public_url"`
			BasePath  string `json:"base_path"`
			Path      string `json:"path"`
			Want      string `json:"want"`
			Error     string `json:"error"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatal(err)
	}
	if len(vector.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range vector.Cases {
		got, err := joinAppURL(c.PublicURL, c.BasePath, c.Path)
		if c.Error != "" {
			var te *Error
			if !errors.As(err, &te) || te.Code() != c.Error || te.Hint() == "" {
				t.Errorf("%s: got %q, %v; want a hinted %s", c.Name, got, err, c.Error)
			}
			continue
		}
		if err != nil || got != c.Want {
			t.Errorf("%s: got %q, %v; want %q", c.Name, got, err, c.Want)
		}
	}
}

func TestAppURLIsAskedFreshAndOutlivesAnOutage(t *testing.T) {
	var asked atomic.Int32
	public := "https://apps.example.com/apps/reporter"
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/app/url" {
			t.Errorf("asked %s", r.URL.Path)
		}
		asked.Add(1)
		writeJSON(t, w, 200, map[string]string{"public_url": public, "base_path": "/apps/reporter"})
	})
	clock := time.Unix(1_000_000, 0)
	urlClock = func() time.Time { return clock }
	t.Cleanup(func() { urlClock = time.Now })
	ctx := context.Background()

	if got, err := App.URL(ctx, "/reports/42"); err != nil || got != "https://apps.example.com/apps/reporter/reports/42" {
		t.Fatalf("URL = %q, %v", got, err)
	}
	_, _ = App.URL(ctx, "")
	if asked.Load() != 1 {
		t.Errorf("asked %d times inside the window, want 1", asked.Load())
	}

	public = "https://applications.example.org/apps/reporter"
	clock = clock.Add(URLTTL + time.Second)
	if got, _ := App.URL(ctx, "/a"); got != "https://applications.example.org/apps/reporter/a" {
		t.Errorf("after the window: %q, want the new address", got)
	}

	// The platform goes away: the last answer stands.
	clock = clock.Add(URLTTL + time.Second)
	SetTransport("http://127.0.0.1:1", "test-token")
	if got, err := App.URL(ctx, "/a"); err != nil || got != "https://applications.example.org/apps/reporter/a" {
		t.Errorf("unreachable with a known address: %q, %v", got, err)
	}
}

func TestAppURLWithNoAddressIsThePlatformsRefusal(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 503, map[string]any{"error": "no address", "code": "no_public_address",
			"retryable": false, "hint": "set APPS_BASE_URL"})
	})
	_, err := App.URL(context.Background(), "")
	var te *Error
	if !errors.As(err, &te) || te.Code() != "no_public_address" {
		t.Errorf("err = %v, want no_public_address", err)
	}
}
