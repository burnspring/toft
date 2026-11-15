package toft

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The 2026-11-04 black-box list's third round, Go's side.

// BB-107: with viewer identity on, a Viewer prints the person Who named — it
// printed "the app cannot see who" beside it — and before anything is resolved
// it claims nothing about whether the app can see who.
func TestAViewerPrintsThePersonOnceIdentityResolved(t *testing.T) {
	key := "test-role-key"
	t.Setenv(EnvRoleKey, key)
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 200, map[string]any{"id": "u1", "name": "Ada Lovelace", "role": "editor"})
	})
	h := http.Header{}
	h.Set(ActForHeader, "blob-107")
	h.Set(RoleHeader, mintForTest(key, RoleEditor, time.Now().Add(time.Minute)))
	v, err := ViewerFor(h)
	if err != nil {
		t.Fatal(err)
	}
	before := v.String()
	if strings.Contains(before, "cannot see who") || !strings.Contains(before, "a signed-in person") {
		t.Errorf("before Who: %q", before)
	}
	if me, err := Who(context.Background(), h); err != nil || me == nil {
		t.Fatalf("Who = %v, %v", me, err)
	}
	if got := v.String(); got != "<toft.Viewer (Ada Lovelace, editor)>" {
		t.Errorf("after Who: %q", got)
	}
}

// BB-112: UpdateN with attempts below 1 is bad_request before any request, as
// Python and Node refuse it; 0 ran one attempt and wrote.
func TestUpdateNRefusesAttemptsBelowOne(t *testing.T) {
	var calls atomic.Int32
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(t, w, 200, map[string]any{"doc": map[string]any{}, "version": 1})
	})
	for _, n := range []int{0, -1} {
		_, err := Data.UpdateN(context.Background(), func(d map[string]any) (map[string]any, error) {
			return d, nil
		}, n)
		hintedRefusal(t, "UpdateN attempts", err, "bad_request")
	}
	if calls.Load() != 0 {
		t.Errorf("%d requests made", calls.Load())
	}
}

// BB-113: a redirect on an ordinary call is coded unexpected_redirect with
// R's hint, and keeps the redirect's status; it was uncoded.
func TestARedirectIsCodedAndHinted(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://sign-in.example/", http.StatusFound)
	})
	_, err := Data.Get(context.Background())
	hintedRefusal(t, "Data.Get on a 302", err, "unexpected_redirect")
	if e, _ := AsError(err); e == nil || e.Status != http.StatusFound || !strings.Contains(e.Hint(), "TOFT_API_URL") {
		t.Errorf("err = %+v", e)
	}
}
