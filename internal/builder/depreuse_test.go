package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLock(t *testing.T, dir, lock string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Every input the install reads moves the key; nothing else does.
func TestNodeDepsKeyIsEveryInputTheInstallReads(t *testing.T) {
	dir := t.TempDir()
	if _, ok := nodeDepsKey(dir, "22", []string{"ci"}, nil); ok {
		t.Fatal("a tree with no lockfile got a key: npm install resolves afresh and must never be reused")
	}
	writeLock(t, dir, `{"a":1}`)
	base, ok := nodeDepsKey(dir, "22", []string{"ci"}, map[string]string{"NODE_ENV": "production"})
	if !ok {
		t.Fatal("no key for a tree with a lockfile")
	}
	if again, _ := nodeDepsKey(dir, "22", []string{"ci"}, map[string]string{"NODE_ENV": "production"}); again != base {
		t.Error("the same inputs gave a different key")
	}
	differs := map[string]func() string{
		"node version": func() string {
			k, _ := nodeDepsKey(dir, "24", []string{"ci"}, map[string]string{"NODE_ENV": "production"})
			return k
		},
		"arguments": func() string {
			k, _ := nodeDepsKey(dir, "22", []string{"ci", "--ignore-scripts"}, map[string]string{"NODE_ENV": "production"})
			return k
		},
		"environment": func() string {
			k, _ := nodeDepsKey(dir, "22", []string{"ci"}, map[string]string{"NODE_ENV": "development"})
			return k
		},
		".npmrc": func() string {
			os.WriteFile(filepath.Join(dir, ".npmrc"), []byte("registry=https://example.test/"), 0o644)
			defer os.Remove(filepath.Join(dir, ".npmrc"))
			k, _ := nodeDepsKey(dir, "22", []string{"ci"}, map[string]string{"NODE_ENV": "production"})
			return k
		},
		"lockfile": func() string {
			writeLock(t, dir, `{"a":2}`)
			defer writeLock(t, dir, `{"a":1}`)
			k, _ := nodeDepsKey(dir, "22", []string{"ci"}, map[string]string{"NODE_ENV": "production"})
			return k
		},
	}
	for what, key := range differs {
		if key() == base {
			t.Errorf("changing the %s did not change the key", what)
		}
	}
}

// A kept tree comes back byte for byte, links included, and the directory is
// trimmed to depsKeep.
func TestKeptNodeModulesAreReusedByTheNextBuildWithTheSameKey(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: t.TempDir()}}
	var logged []string
	lg := buildLog{step: func(s string) { logged = append(logged, s) }, output: func(string) {}}

	first := t.TempDir()
	mods := filepath.Join(first, "node_modules")
	os.MkdirAll(filepath.Join(mods, "left-pad", ".bin"), 0o755)
	os.WriteFile(filepath.Join(mods, "left-pad", "index.js"), []byte("module.exports=1"), 0o644)
	os.Symlink("../index.js", filepath.Join(mods, "left-pad", ".bin", "pad"))

	if b.reuseDeps("d1", first, "k1", "node_modules", lg) {
		t.Fatal("reused a tree nothing had kept")
	}
	b.keepDeps("d1", first, "k1", "node_modules", lg)

	second := t.TempDir()
	if !b.reuseDeps("d2", second, "k1", "node_modules", lg) {
		t.Fatal("the next build with the same key did not reuse the kept tree")
	}
	got, err := os.ReadFile(filepath.Join(second, "node_modules", "left-pad", "index.js"))
	if err != nil || string(got) != "module.exports=1" {
		t.Errorf("reused file = %q, %v", got, err)
	}
	if link, err := os.Readlink(filepath.Join(second, "node_modules", "left-pad", ".bin", "pad")); err != nil || link != "../index.js" {
		t.Errorf("reused link = %q, %v: a .bin link must stay a relative link", link, err)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "reused") {
		t.Errorf("the build log does not say the tree was reused: %v", logged)
	}
	if b.reuseDeps("d3", t.TempDir(), "other", "node_modules", lg) {
		t.Error("a different key reused the kept tree")
	}

	root := filepath.Join(b.opts.CacheDir, depsDirName)
	for i := 0; i < depsKeep+2; i++ {
		os.MkdirAll(filepath.Join(root, "old"+string(rune('a'+i))), 0o700)
		old := time.Now().Add(-time.Duration(i+1) * time.Hour)
		os.Chtimes(filepath.Join(root, "old"+string(rune('a'+i))), old, old)
	}
	trimDeps(root, depsKeep)
	entries, _ := os.ReadDir(root)
	if len(entries) != depsKeep {
		t.Errorf("%d trees kept after the trim, want %d", len(entries), depsKeep)
	}
	if !dirExists(filepath.Join(root, "k1")) {
		t.Error("the trim removed the most recently used tree")
	}
}
