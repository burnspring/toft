package hoststore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// gatedFolderMode and gatedFileMode are what the data plane writes: the gate's
// group may edit and delete, nobody else may look. Set explicitly after every
// create, because the platform's own umask would otherwise take the group's
// write bit off.
const (
	gatedFolderMode = 0o770 | os.ModeSetgid
	gatedFileMode   = 0o660
)

// errNotFolder means a path component the request must pass through is a file.
var errNotFolder = errors.New("a file is in the way of that folder")

// errLinkedScope means a component of the scope folder is a symbolic link. The
// scope is the containment boundary, so it is never one: an app on a whole
// resource could otherwise point a granted folder at one the grant does not
// cover.
// errLinkedFolder is a folder on the way to a write that is a link, or became
// one: the data plane writes only through real folders below the scope.
var errLinkedFolder = errors.New("a folder on that path is a symbolic link, which the platform will not follow")

var errLinkedScope = errors.New("the granted folder is a symbolic link, which the platform will not follow")

// openScope opens the containment root: the resource directory, then each
// component of the scope through the root before it, refusing any that is a
// link. **os.OpenRoot on the joined path would follow a link in it**, which is
// the escape this exists to close. With create, missing components are made
// as gated folders taking gid — a grant on a folder nothing has written to yet
// is still somewhere a person may upload.
func openScope(resourceDir, scope string, create bool, gid int) (*os.Root, error) {
	cur, err := os.OpenRoot(resourceDir)
	if err != nil {
		return nil, err
	}
	if scope == "" {
		return cur, nil
	}
	for _, comp := range strings.Split(scope, "/") {
		next, err := enter(cur, comp, create, gid)
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// betweenCheckAndOpen runs between enter's type check and its open. Nil outside
// the test that swaps the component in exactly that gap.
var betweenCheckAndOpen func(cur *os.Root, comp string)

func enter(cur *os.Root, comp string, create bool, gid int) (*os.Root, error) {
	info, err := cur.Lstat(comp)
	if errors.Is(err, fs.ErrNotExist) && create {
		// Made by a concurrent request in between is as good as made here; the
		// Lstat below judges what is there either way.
		if err := makeFolder(cur, comp, gid); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		info, err = cur.Lstat(comp)
	}
	switch {
	case err != nil:
		return nil, err
	case info.Mode()&fs.ModeSymlink != 0:
		return nil, errLinkedScope
	case !info.IsDir():
		return nil, errNotFolder
	}
	if betweenCheckAndOpen != nil {
		betweenCheckAndOpen(cur, comp)
	}
	// The check and the open are two calls, and OpenRoot follows a link that
	// stays inside cur: an app with rename rights here (a whole resource) can
	// swap the folder for a link to a sibling in the gap. So the directory
	// opened must be the very one the Lstat judged.
	next, err := cur.OpenRoot(comp)
	if err != nil {
		return nil, err
	}
	opened, err := next.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		next.Close()
		return nil, errLinkedScope
	}
	return next, nil
}

// descend opens the folder rel below the scope root as a root of its own,
// one component at a time, each opened without following a link and checked
// to be the very directory its Lstat judged — the descent openScope makes to
// the scope, continued below it. **Every host-store verb addresses names
// through the folder this returns**, never by a path below the scope:
// os.Root follows a link that stays inside the root, so `X/sub -> ../Y`
// showed and deleted Y's files under X, and a component swapped between a
// check and the next by-name call put an upload in Y (AS-13). With create,
// missing folders are made gated, and existed reports whether the last one
// already was there. The caller closes the root returned.
func descend(root *os.Root, rel string, create bool, gid int) (folder *os.Root, existed bool, err error) {
	cur, err := root.OpenRoot(".")
	if err != nil {
		return nil, false, err
	}
	existed = true
	if rel == "" || rel == "." {
		return cur, true, nil
	}
	for _, comp := range strings.Split(rel, "/") {
		if comp == "" || comp == "." || comp == ".." {
			cur.Close()
			return nil, false, fmt.Errorf("%w: %q", errLinkedFolder, rel)
		}
		_, lerr := cur.Lstat(comp)
		existed = lerr == nil
		next, err := enter(cur, comp, create, gid)
		cur.Close()
		if errors.Is(err, errLinkedScope) {
			err = fmt.Errorf("%w: %s", errLinkedFolder, comp)
		}
		if err != nil {
			return nil, false, err
		}
		cur = next
	}
	return cur, existed, nil
}

// makeFolder creates one gated folder: the gate's group, setgid, 2770.
//
// **Gated through the descriptor, never by name.** Between the mkdir and a
// chown by name an app on the gate could swap the new name for a link to a
// sibling folder, and the sibling was re-grouped to this gate (security
// review 2026-11-10, C-2). Opened O_NOFOLLOW|O_DIRECTORY, a swapped name
// fails the open, and fchown/fchmod reach only the directory just made.
func makeFolder(root *os.Root, name string, gid int) error {
	if err := root.Mkdir(name, 0o770); err != nil {
		return err
	}
	d, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("%w: %s", errLinkedFolder, name)
	}
	defer d.Close()
	if gid > 0 {
		if err := d.Chown(-1, gid); err != nil {
			return err
		}
	}
	return d.Chmod(gatedFolderMode)
}
