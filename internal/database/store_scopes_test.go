package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

func scopes(pairs ...string) database.StoreScopes {
	out := database.StoreScopes{}
	for i := 0; i < len(pairs); i += 2 {
		out.Scopes = append(out.Scopes, database.StoreScope{Folder: pairs[i], Access: pairs[i+1]})
	}
	return out
}

// A folder is matched on a separator, never as a bare string prefix.
//
// **This is the one test in the feature whose failure is a containment breach
// rather than a wrong answer.** A grant on `report` that reached every key under
// `reports/` would sign URLs for a directory nobody was granted, and nothing
// would error — the caller would simply be handed an object. It is the same
// reasoning, and the same trailing slash, that kube.StoreMountOptions applies to
// a mount's prefix; the two have to agree or a person is signed a URL for
// something no mount of theirs would ever show them.
func TestLevelForKeyMatchesOnSeparators(t *testing.T) {
	s := scopes("report", runtime.AccessFull)

	for _, key := range []string{
		"reports/2026/q1.csv", // the sibling directory
		"report",              // an object beside the folder, not inside it
		"reportx",
		"reports",
	} {
		if got := s.LevelForKey(key); got != "" {
			t.Errorf("LevelForKey(%q) = %q, want none — a grant on %q reaches inside it and nothing else",
				key, got, "report")
		}
	}
	if got := s.LevelForKey("report/2026/q1.csv"); got != runtime.AccessFull {
		t.Errorf("LevelForKey inside the granted folder = %q, want %q", got, runtime.AccessFull)
	}
}

// A folder covers itself as a path and does not cover itself as a key.
//
// The distinction is load-bearing and easy to lose in a refactor: `analytics` is
// the folder somebody was granted, so listing it is exactly what the grant is
// for — while an *object* whose whole key is `analytics` sits beside that folder
// and was never granted. Folding the two together hands somebody an object on
// the strength of a name collision.
func TestLevelForPathCoversTheFolderItself(t *testing.T) {
	s := scopes("analytics", runtime.AccessRead)

	if got := s.LevelForPath("analytics"); got != runtime.AccessRead {
		t.Errorf("LevelForPath(%q) = %q, want %q — a grant on a folder lists that folder",
			"analytics", got, runtime.AccessRead)
	}
	if got := s.LevelForPath("analytics/"); got != runtime.AccessRead {
		t.Errorf("a trailing slash is the same path: got %q", got)
	}
	if got := s.LevelForKey("analytics"); got != "" {
		t.Errorf("LevelForKey(%q) = %q, want none — that object is beside the folder", "analytics", got)
	}
}

// Union-of-max, per key, and a narrow grant never subtracts.
//
// The reason union-of-max exists at all is that adding a narrow grant must never
// be a silent revocation of a wide one. With folders that failure has a new
// shape — most-specific-wins — and it is silent in exactly the same way: nothing
// errors, somebody simply stops being able to delete what they could delete
// yesterday, and the row that caused it looks like a row that granted them more.
func TestLevelForKeyIsUnionOfMax(t *testing.T) {
	wide := scopes("", runtime.AccessFull)
	narrowed := scopes("", runtime.AccessFull, "reports", runtime.AccessRead)

	for _, key := range []string{"reports/2026.csv", "elsewhere/x", "top.txt"} {
		if wide.LevelForKey(key) != runtime.AccessFull {
			t.Fatalf("precondition: root grant should be full on %q", key)
		}
		if got := narrowed.LevelForKey(key); got != runtime.AccessFull {
			t.Errorf("LevelForKey(%q) after adding a narrow read grant = %q, want %q — "+
				"adding a grant may never take reach away", key, got, runtime.AccessFull)
		}
	}

	// And the other direction: the narrow grant does add, where it covers.
	up := scopes("", runtime.AccessRead, "reports", runtime.AccessFull)
	if got := up.LevelForKey("reports/2026.csv"); got != runtime.AccessFull {
		t.Errorf("inside the folder = %q, want %q", got, runtime.AccessFull)
	}
	if got := up.LevelForKey("elsewhere/x"); got != runtime.AccessRead {
		t.Errorf("outside the folder = %q, want %q", got, runtime.AccessRead)
	}
}

// The folders a caller reaches, minimally, and only when they do not reach the
// root — which is what the store's synthetic root listing is drawn from.
func TestGrantedFolders(t *testing.T) {
	// Covered folders collapse into the one that covers them: a caller granted
	// `a` and `a/b` has one entry point, not two.
	s := scopes("a", runtime.AccessRead, "a/b", runtime.AccessFull, "c", runtime.AccessRead)
	got := s.GrantedFolders()
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("GrantedFolders() = %v, want [a c]", got)
	}

	// A caller who reaches the root has no synthetic listing: the real one is
	// the answer, and every row of it is theirs to open.
	if f := scopes("", runtime.AccessRead, "a", runtime.AccessFull).GrantedFolders(); f != nil {
		t.Errorf("GrantedFolders() with a root grant = %v, want none", f)
	}
}

// Top is the highest level held anywhere, and authorizes nothing by itself.
func TestTopIsTheHighestScope(t *testing.T) {
	s := scopes("a", runtime.AccessRead, "b", runtime.AccessWrite)
	if got := s.Top(); got != runtime.AccessWrite {
		t.Errorf("Top() = %q, want %q", got, runtime.AccessWrite)
	}
	if got := s.LevelForKey("c/x"); got != "" {
		t.Errorf("LevelForKey outside every scope = %q, want none — Top is a summary, not a grant", got)
	}
}
