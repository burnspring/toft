package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// The credentials file is a single-writer design, and this is the single
// writer.
//
// Its own file because it is the only part of the package with a retry loop in
// it, and a retry loop reads badly next to code that is otherwise a
// straight-line read-modify-write.
//
// `O_EXCL` rather than flock or LockFileEx: it needs no new dependency in a
// module whose only non-stdlib weight today is the UI's, and it behaves the same
// on Windows, which userConfigDir already supports. It is adequate for a file
// that two processes touch by coincidence, which is what `toft login` in two
// terminals is — it is not a general mutual-exclusion primitive and is not used
// as one.
const (
	// lockPoll is how often a waiter looks again. Writing this file takes
	// milliseconds, so the wait is almost always one poll or none.
	lockPoll = 20 * time.Millisecond
	// lockWait bounds the wait, because a CLI that blocks forever on a lock is
	// worse than one that says which file to delete.
	lockWait = 5 * time.Second
	// lockStale is when a lock stops being evidence of a live writer.
	//
	// A credentials file is written in milliseconds; a lock older than a minute
	// is a process that was killed. Left unbroken it would lock somebody out of
	// their own credentials, which is a worse failure than the race it guards
	// against. Deliberately a constant: the only person who would tune it is one
	// debugging something else.
	lockStale = 60 * time.Second
)

// lock takes the credentials lock, waiting a bounded time for it.
//
// It reports whether it had to break a stale lock, so the caller can say so —
// silently stepping over another process's lock is the kind of thing that should
// appear in the output the one time it matters.
func lock(path string) (release func(), brokeStale bool, err error) {
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, brokeStale, nil
		}
		if !os.IsExist(err) {
			return nil, brokeStale, fmt.Errorf("taking the credentials lock %s: %w", path, err)
		}
		if breakIfStale(path) {
			brokeStale = true
			continue
		}
		if time.Now().After(deadline) {
			return nil, brokeStale, fmt.Errorf(
				"another toft is writing the credentials file and did not finish — "+
					"if nothing else is running, delete %s", path)
		}
		time.Sleep(lockPoll)
	}
}

// breakIfStale removes a lock left behind by a process that is no longer there.
func breakIfStale(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		// Gone between the failed create and here: the holder released it, so
		// the next attempt is the one that succeeds.
		return os.IsNotExist(err)
	}
	if time.Since(info.ModTime()) < lockStale {
		return false
	}
	return os.Remove(path) == nil
}

// writePrivate replaces a file's contents atomically, and durably.
//
// The rename is what makes an interrupted write unable to leave somebody with a
// truncated credentials file and no way back in. The fsync before it is what
// makes that true against a lost power rail rather than only against a killed
// process: without it the rename can land while the bytes it points at have not.
//
// The temporary name carries the pid, so two writers that somehow both got here
// cannot rename each other's partial file into place. Created 0600 from the
// start rather than chmod'ed afterwards — the gap between the two is a window
// where the token is world-readable.
func writePrivate(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes the rename itself durable.
//
// Not on Windows, where a directory is not something you open and sync, and
// where the rename's durability is the filesystem's business.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
