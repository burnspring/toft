package builder

import (
	"log"
	"os"
	"path/filepath"
)

// trashName is the directory under APPS_DIR that a deleted tree is moved into
// on its way out. Nothing lists APPS_DIR's top level -- every path is built
// from an app id -- so a name no id can take is enough to keep it apart.
const trashName = ".trash"

// toTrash takes dir out of the tree at once and removes it in the background.
//
// **A delete answers in the time a rename takes, not the time a tree takes.**
// A release holds a virtualenv or a node_modules, tens of thousands of files,
// and on a network filesystem (EFS) every one of them is a round trip: removing
// an app inline took minutes, so the request timed out in every client while
// the platform went on deleting. A rename within the same filesystem is one
// metadata operation, after which the path is free and nothing names the bytes
// but the trash; SweepTrash finishes what a restart interrupted.
//
// A rename that fails -- the tree on another device, the trash not creatable --
// falls back to removing it here, so the caller's rule still holds: no error
// means the path is gone.
func toTrash(appsDir, dir string) error {
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil
	}
	trash := filepath.Join(appsDir, trashName)
	if err := os.MkdirAll(trash, 0o755); err == nil {
		if holder, err := os.MkdirTemp(trash, filepath.Base(dir)+"-"); err == nil {
			if err := os.Rename(dir, filepath.Join(holder, "tree")); err == nil {
				go removeTrash(holder)
				return nil
			}
			_ = os.Remove(holder)
		}
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SweepTrash removes whatever a previous process moved to the trash and did
// not finish removing. It returns at once; the work is in the background.
func SweepTrash(appsDir string) {
	entries, err := os.ReadDir(filepath.Join(appsDir, trashName))
	if err != nil {
		return
	}
	for _, e := range entries {
		go removeTrash(filepath.Join(appsDir, trashName, e.Name()))
	}
}

// removeTrash is a variable so a test can wait for it.
var removeTrash = func(path string) {
	if err := os.RemoveAll(path); err != nil {
		log.Printf("trash: removing %s: %v (SweepTrash retries at the next start)", path, err)
	}
}
