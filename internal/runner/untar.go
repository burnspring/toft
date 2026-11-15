package runner

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxUnpackBytes bounds what one release may expand to inside a workload.
// Without it a decompression bomb fills the container's ephemeral disk.
const maxUnpackBytes = 8 << 30

// untarGz unpacks a release tarball into dest.
//
// This deliberately mirrors internal/artifact's unpacker rather than importing
// it: the runner ships as a standalone static binary inside an image that has
// no platform code, and the two are held together by the round-trip test
// rather than by a shared package. Both check every path against the
// destination before writing — a tar built from a cloned repo is
// attacker-influenced input, and an entry named "../../etc/cron.d/x" is how an
// unpacker gets used to write outside the tree it was pointed at.
func untarGz(r io.Reader, dest string) error {
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	// Resolved once, so that "is this inside the release directory" is a
	// comparison between two resolved paths. Without it a dest that itself sits
	// under a symlink (/tmp -> /private/tmp) makes every later check fail.
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}

	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("release is not a gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var written int64
	type dirMode struct {
		path string
		mode os.FileMode
	}
	var dirs []dirMode

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading release: %w", err)
		}

		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return err
		}
		// Nothing is ever written *through* a symlink this archive planted.
		// That, not the existence of the link, is how an unpacker gets used to
		// write outside the tree it was pointed at. Checked before anything is
		// created, so a refused entry leaves nothing behind.
		if err := contained(root, filepath.Dir(target)); err != nil {
			return fmt.Errorf("release entry %q: %w", hdr.Name, err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			// The entry's own name too: it may already exist as a symlink out of
			// the tree, and the deferred Chmod below would follow it.
			if err := contained(root, target); err != nil {
				return fmt.Errorf("release entry %q: %w", hdr.Name, err)
			}
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			// Applied last: a 0500 directory written now would stop its own
			// children being created.
			dirs = append(dirs, dirMode{target, hdr.FileInfo().Mode().Perm()})

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			// O_NOFOLLOW covers the last component, which the parent check
			// cannot: an archive listing a symlink and then a regular file at
			// the same name would otherwise write through the link it just
			// created.
			f, err := os.OpenFile(target,
				os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW,
				hdr.FileInfo().Mode().Perm())
			if err != nil {
				return err
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxUnpackBytes-written+1))
			f.Close()
			if err != nil {
				return fmt.Errorf("writing %s: %w", hdr.Name, err)
			}
			written += n
			if written > maxUnpackBytes {
				return fmt.Errorf("release expands beyond %d bytes", int64(maxUnpackBytes))
			}

		case tar.TypeSymlink:
			if hdr.Linkname == "" {
				return fmt.Errorf("release contains a symlink with no target")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := notADirectory(target); err != nil {
				return fmt.Errorf("release entry %q: %w", hdr.Name, err)
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}

		case tar.TypeLink:
			source, err := safeJoin(root, hdr.Linkname)
			if err != nil {
				return err
			}
			// A hardlink's source is resolved by the kernel, so "etc/passwd"
			// after planting "etc -> /etc" would pull an outside inode into the
			// tree — and O_NOFOLLOW cannot see it afterwards, because a hardlink
			// is not a symlink. safeJoin only checks the name lexically.
			if err := contained(root, source); err != nil {
				return fmt.Errorf("release entry %q: %w", hdr.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := notADirectory(target); err != nil {
				return fmt.Errorf("release entry %q: %w", hdr.Name, err)
			}
			os.Remove(target)
			if err := os.Link(source, target); err != nil {
				return err
			}
		}
	}

	for i := len(dirs) - 1; i >= 0; i-- {
		if err := chmodDir(dirs[i].path, dirs[i].mode); err != nil {
			return fmt.Errorf("release directory: %w", err)
		}
	}
	return nil
}

// notADirectory refuses a link entry whose name is an existing directory. A
// directory entry queues its name for the closing chmod; a later link of the
// same name would remove the empty directory and plant a link there, and the
// chmod would then follow it to wherever it pointed (AS-12).
func notADirectory(target string) error {
	if info, err := os.Lstat(target); err == nil && info.IsDir() {
		return fmt.Errorf("a link replaces the directory %s", filepath.Base(target))
	}
	return nil
}

// chmodDir applies a directory's mode through a descriptor opened without
// following a link, so a name that is no longer a directory is refused rather
// than chmodded through.
func chmodDir(p string, mode os.FileMode) error {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(mode)
}

func safeJoin(root, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("release contains an entry with no name")
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("release contains an absolute path: %q", name)
	}
	target := filepath.Join(root, filepath.FromSlash(name))
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("release entry escapes the release directory: %q", name)
	}
	return target, nil
}

// contained reports whether p, with every symlink on the way to it resolved, is
// really inside root.
//
// It replaces a rule that refused any symlink pointing out of the tree, and the
// relaxation is deliberate: **a Python virtual environment cannot be shipped
// without one**. `uv venv` writes `venv/bin/python -> /usr/bin/python3`, an
// absolute link to the base interpreter, so the old rule refused every Python
// release the moment a runner tried to unpack it — which is what would have made
// "a job is an isolated workload in both execution modes" false in practice
// rather than merely untested.
//
// Allowing the link is safe because a symlink is inert. What an unpacker can be
// used for is *writing* outside the tree it was pointed at, and that takes two
// entries: a link, and then something written through it. safeJoin refuses a
// name that escapes lexically ("../../etc/cron.d/x"); this refuses the other
// half, which filepath.Join cannot see because it resolves "foo/passwd" without
// knowing "foo" is a link to /etc.
//
// The resolution is the kernel's, not a ledger of which links looked like they
// escaped, because two shapes defeat such a ledger: a chain where every hop
// stays inside the tree but the last one leaves ("a -> b", "b -> /etc"), and a
// hardlink, which pulls an outside inode *in* and which O_NOFOLLOW cannot then
// refuse because a hardlink is not a symlink.
//
// Kept byte-for-byte in step with internal/artifact's copy, which the round-trip
// test holds it to. The runner ships as a standalone binary in an image with no
// platform code, so the two are duplicated rather than shared.
func contained(root, p string) error {
	// Resolve the deepest component that already exists. Anything below it does
	// not exist yet and will be created by MkdirAll as a real directory, which
	// cannot be a link.
	probe := p
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("%q has no existing ancestor", p)
		}
		probe = parent
	}
	resolved, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return err
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return fmt.Errorf("resolves through a symlink to %q, outside the release directory", resolved)
	}
	return nil
}
