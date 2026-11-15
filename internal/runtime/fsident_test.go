package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// nobodyAccount is an account every Linux host has, for the tests that need a
// real second identity. They run only as root: sudo go test ./internal/runtime/.
func nobodyAccount(t *testing.T) *AppAccount {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: an account's identity can only be taken by root")
	}
	return &AppAccount{Username: "nobody", UID: 65534, GID: 65534}
}

// The platform writes into a tree it has handed to an account as that account:
// what it creates is the account's, and a link the account planted reaches only
// what the account could reach.
func TestWritingAsAnAccountIsContainedByItsPermissions(t *testing.T) {
	a := nobodyAccount(t)
	base := traversableTempDir(t)
	tree := filepath.Join(base, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(tree, a.UID, a.GID); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(base, "root-only")
	if err := os.WriteFile(secret, []byte("root's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(tree, "planted")); err != nil {
		t.Fatal(err)
	}

	err := WithAccountFS(a, func() error {
		return os.WriteFile(filepath.Join(tree, "written"), []byte("x"), 0o644)
	})
	if err != nil {
		t.Fatalf("writing into the account's own tree: %v", err)
	}
	info, err := os.Lstat(filepath.Join(tree, "written"))
	if err != nil {
		t.Fatal(err)
	}
	if st := info.Sys().(*syscall.Stat_t); int(st.Uid) != a.UID {
		t.Errorf("the file is owned by %d, want the account's %d", st.Uid, a.UID)
	}

	err = WithAccountFS(a, func() error {
		return os.WriteFile(filepath.Join(tree, "planted"), []byte("overwritten"), 0o644)
	})
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("writing through a planted link = %v, want permission denied", err)
	}
	if got, _ := os.ReadFile(secret); string(got) != "root's" {
		t.Errorf("the link's target was changed: %q", got)
	}
	var readErr error
	_ = WithAccountFS(a, func() error { _, readErr = os.ReadFile(filepath.Join(tree, "planted")); return nil })
	if !errors.Is(readErr, os.ErrPermission) {
		t.Errorf("reading through a planted link = %v, want permission denied", readErr)
	}

	// The thread that did it is gone; the process is still root.
	if err := os.WriteFile(filepath.Join(base, "after"), []byte("x"), 0o600); err != nil {
		t.Fatalf("the caller lost root: %v", err)
	}
}

func TestWritingAsNoAccountIsThePlatform(t *testing.T) {
	ran := false
	if err := WithAccountFS(nil, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
}

// traversableTempDir is a t.TempDir another account can reach: the test's own
// parent directory is created 0700.
func traversableTempDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	for _, d := range []string{filepath.Dir(base), base} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return base
}
