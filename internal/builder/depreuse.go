package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
	"toft/internal/deps"
)

// Reusing an unchanged dependency tree (ADR §123).
//
// A redeploy that changed only the app's code used to install every dependency
// again: `npm ci` empties node_modules and extracts every package from the npm
// cache, which for a Next.js app is tens of thousands of files and most of the
// build. So a Node build that installed from a lockfile keeps a copy of the
// node_modules it produced, under a key that is every input the install read,
// and the next build whose inputs are the same copies that back instead of
// installing.
//
// **Node only, and that is measured rather than assumed.** Python's uv installs
// out of its own cache in a couple of seconds, and a venv is not relocatable —
// its console scripts name the old release's interpreter by absolute path. R's
// renv cache already makes an unchanged lock a copy out of the cache. Go's build
// cache is the same answer for Go.
//
// **Where the copy lives is the app account's own cache directory**, beside its
// npm and renv caches, and the app's running process can write it. That gives
// it nothing it did not have: the same account writes this app's next build
// tree while that build runs. It is the argument of ADR §29 — the app's account
// is the one identity that gains nothing by building — and the reason the
// copies are never shared between accounts. An install with no accounts keeps
// them under CACHE_DIR/deps, as it keeps every other cache.

// depsKeep is how many dependency trees one directory keeps: enough for a
// branch and its revert, or a handful of apps on an install with no accounts.
const depsKeep = 4

// depsDirName is the directory the copies live in, under an account's own cache
// directory or CACHE_DIR.
const depsDirName = "deps"

// nodeDepsKey is every input `npm ci` reads, hashed: the interpreter it ran
// under, its arguments, the lockfile, the project's .npmrc and the whole build
// environment (a registry, a token, NODE_ENV). ok is false when the install is
// not reproducible from those inputs — no lockfile means `npm install`, which
// resolves afresh — and nothing is reused or kept.
func nodeDepsKey(releaseDir, nodeVersion string, args []string, env map[string]string) (string, bool) {
	lock, err := readInRelease(releaseDir, deps.NPMLockfile(releaseDir))
	if err != nil {
		return "", false
	}
	h := sha256.New()
	fmt.Fprintf(h, "node-deps/1\x00%s\x00", nodeVersion)
	for _, a := range args {
		fmt.Fprintf(h, "%s\x00", a)
	}
	fmt.Fprintf(h, "lock\x00%d\x00", len(lock))
	h.Write(lock)
	if npmrc, err := readInRelease(releaseDir, ".npmrc"); err == nil {
		fmt.Fprintf(h, "npmrc\x00%d\x00", len(npmrc))
		h.Write(npmrc)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "env\x00%s=%s\x00", k, env[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:32], true
}

// depsRoot is where the dependency copies for a build in releaseDir live: the
// building account's own directory, or CACHE_DIR's. Empty means nowhere, and
// nothing is reused.
func (b *Builder) depsRoot(releaseDir string) string {
	if id := b.identityFor(releaseDir); id != nil {
		return filepath.Join(filepath.Dir(id.home), depsDirName)
	}
	if b.opts.CacheDir == "" {
		return ""
	}
	return filepath.Join(b.opts.CacheDir, depsDirName)
}

// reuseDeps copies a kept tree named sub into releaseDir, when one exists under
// key. true means the install can be skipped. A copy that fails is cleaned up
// and reported false, so the caller installs as it would have.
func (b *Builder) reuseDeps(deployID, releaseDir, key, sub string, lg buildLog) bool {
	root := b.depsRoot(releaseDir)
	if root == "" || key == "" {
		return false
	}
	kept := filepath.Join(root, key, sub)
	if !dirExists(kept) {
		return false
	}
	began := time.Now()
	dest := filepath.Join(releaseDir, sub)
	if err := b.runCommandFor(depsCopyTimeout, deployID, releaseDir, "cp",
		[]string{"-a", "--reflink=auto", kept, dest}, nil, lg.output); err != nil {
		lg.output(fmt.Sprintf("    (the kept %s could not be copied, so it is installed instead: %v)", sub, err))
		_ = b.asBuild(releaseDir, func() error { return os.RemoveAll(dest) })
		return false
	}
	// Touched, so the eviction keeps the trees in use.
	_ = b.asBuild(releaseDir, func() error {
		now := time.Now()
		return os.Chtimes(filepath.Join(root, key), now, now)
	})
	lg.stepf("    Dependencies are unchanged since an earlier build, so its %s was reused (%s).",
		sub, time.Since(began).Round(100*time.Millisecond))
	return true
}

// keepDeps copies releaseDir's freshly installed sub under key, then trims the
// directory to depsKeep. Never fatal: a build whose copy could not be kept is a
// build the next one installs again, which is where every build used to be.
func (b *Builder) keepDeps(deployID, releaseDir, key, sub string, lg buildLog) {
	root := b.depsRoot(releaseDir)
	if root == "" || key == "" || !dirExists(filepath.Join(releaseDir, sub)) {
		return
	}
	final := filepath.Join(root, key)
	if dirExists(final) {
		return
	}
	partial := filepath.Join(root, ".partial-"+key+"-"+deployID)
	if err := b.asBuild(releaseDir, func() error { return os.MkdirAll(partial, 0o700) }); err != nil {
		lg.output(fmt.Sprintf("    (the installed %s could not be kept for the next build: %v)", sub, err))
		return
	}
	if err := b.runCommandFor(depsCopyTimeout, deployID, releaseDir, "cp",
		[]string{"-a", "--reflink=auto", filepath.Join(releaseDir, sub), filepath.Join(partial, sub)}, nil, lg.output); err != nil {
		lg.output(fmt.Sprintf("    (the installed %s could not be kept for the next build: %v)", sub, err))
		_ = b.asBuild(releaseDir, func() error { return os.RemoveAll(partial) })
		return
	}
	_ = b.asBuild(releaseDir, func() error {
		// A rename, so a concurrent build never copies a half-kept tree.
		if err := os.Rename(partial, final); err != nil {
			return os.RemoveAll(partial)
		}
		trimDeps(root, depsKeep)
		return nil
	})
}

// trimDeps removes all but the keep most recently used trees under root, and
// any partial copy an interrupted build left behind for more than an hour.
func trimDeps(root string, keep int) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type tree struct {
		name string
		used time.Time
	}
	var trees []tree
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() {
			continue
		}
		if len(e.Name()) > 9 && e.Name()[:9] == ".partial-" {
			if time.Since(info.ModTime()) > time.Hour {
				os.RemoveAll(filepath.Join(root, e.Name()))
			}
			continue
		}
		trees = append(trees, tree{e.Name(), info.ModTime()})
	}
	sort.Slice(trees, func(i, j int) bool { return trees[i].used.After(trees[j].used) })
	for _, t := range trees[min(keep, len(trees)):] {
		os.RemoveAll(filepath.Join(root, t.name))
	}
}

// depsCopyTimeout bounds one copy in or out of the kept trees.
const depsCopyTimeout = 10 * time.Minute

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
