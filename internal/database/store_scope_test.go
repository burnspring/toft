package database

import "testing"

// The scope is the containment root the host data plane opens, so it is the
// folder that granted the winning level — and of several at that level, the
// deepest, the narrowest boundary that still authorizes the request. A wider
// one would let a link inside the granted folder reach what only the wider
// grant covers, at the narrower grant's level.
func TestTheScopeIsTheDeepestFolderAtTheWinningLevel(t *testing.T) {
	s := StoreScopes{Scopes: []StoreScope{
		{Folder: "", Access: "read"},
		{Folder: "teams", Access: "write"},
		{Folder: "teams/a", Access: "write"},
		{Folder: "teams/b", Access: "full"},
	}}
	for _, c := range []struct{ key, level, folder string }{
		{"top.csv", "read", ""},
		{"teams/x.csv", "write", "teams"},
		{"teams/a/x.csv", "write", "teams/a"},
		{"teams/b/x.csv", "full", "teams/b"},
		{"teamsx/y", "read", ""},
	} {
		level, folder := s.ScopeForKey(c.key)
		if level != c.level || folder != c.folder {
			t.Errorf("ScopeForKey(%q) = %q, %q; want %q, %q", c.key, level, folder, c.level, c.folder)
		}
	}
	if level, folder := s.ScopeForPath("teams/a/"); level != "write" || folder != "teams/a" {
		t.Errorf("ScopeForPath(teams/a/) = %q, %q", level, folder)
	}
}
