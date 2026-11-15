package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxUnpackBytes bounds what one artifact may expand to. A release tree that
// legitimately exceeds this is a signal to reconsider what is being deployed;
// an artifact that does so unexpectedly is a decompression bomb, and without a
// bound the unpack fills the disk out from under every other app on the host.
const maxUnpackBytes = 8 << 30 // 8 GiB

// packLimit is maxUnpackBytes on the packing side. A variable only so a test
// can lower it; nothing else writes it.
var packLimit int64 = maxUnpackBytes

// humanBytes names a limit the way a person reads one.
func humanBytes(n int64) string {
	if n >= 1<<30 && n%(1<<30) == 0 {
		return fmt.Sprintf("%d GiB", n>>30)
	}
	return fmt.Sprintf("%d bytes", n)
}

// pack writes dir as a gzipped tar to w. Symlinks are stored as symlinks
// rather than followed: a Python venv is full of them, and following them
// would both explode the size and turn a link to something outside the tree
// into a silent copy of it.
// Progress is what a pack reports as it goes: how many entries and bytes it
// has written, and the path it is on. A publish that stalls was invisible --
// the build log stopped at "Publishing release..." with nothing to say where
// (2026-10-22, a Next.js release on trytoft.dev) -- so the caller hears
// about it every few thousand entries, and a deadline names the last path.
type Progress func(entries int, bytes int64, path string)

type progressKey struct{}

// WithProgress attaches a progress callback to a Put's context.
func WithProgress(ctx context.Context, fn Progress) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func progressOf(ctx context.Context) Progress {
	if fn, ok := ctx.Value(progressKey{}).(Progress); ok && fn != nil {
		return fn
	}
	return func(int, int64, string) {}
}

// pack writes dir as a gzipped tar. It honours ctx between entries, so a
// deadline stops it rather than leaving a deploy in "building" for good.
//
// **The tree is read through an os.Root.** In the shared runtime this process
// is root and the release has already been handed to the app's account, which
// can rename and symlink entries while the walk runs: a path-based walk follows
// a directory swapped for a link to /etc or another app's release, and a
// Lstat-then-Open on each file is the same race one level down. Every directory
// read, link read and file open here resolves inside dir and refuses a link
// that leaves it, and a file's header is built from the descriptor that was
// opened, so what is packed is exactly what was checked.
func pack(ctx context.Context, dir string, w io.Writer) error {
	progress := progressOf(ctx)
	var entries int
	var written int64
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("packing stopped after %d entries (%d bytes), at %s: %w",
				entries, written, rel, err)
		}
		entries++
		progress(entries, written, rel)

		// Sockets, devices and fifos cannot be meaningfully restored and a
		// stray one must not fail an otherwise good build.
		typ := d.Type()
		if typ&fs.ModeType != 0 && !d.IsDir() && typ&fs.ModeSymlink == 0 {
			return nil
		}

		var fi fs.FileInfo
		var src *os.File
		link := ""
		switch {
		case typ&fs.ModeSymlink != 0:
			if link, err = root.Readlink(rel); err != nil {
				return err
			}
			if fi, err = root.Lstat(rel); err != nil {
				return err
			}
		case d.IsDir():
			if fi, err = d.Info(); err != nil {
				return err
			}
		default:
			// Opened without following a link, then judged by the descriptor:
			// an entry swapped since the listing is refused here or found not
			// to be a regular file, never read through.
			if src, err = root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0); err != nil {
				return err
			}
			defer src.Close()
			if fi, err = src.Stat(); err != nil {
				return err
			}
			if !fi.Mode().IsRegular() {
				return nil
			}
		}

		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		// Tar paths are always slash-separated regardless of the host.
		hdr.Name = filepath.ToSlash(rel)
		// Ownership is the *restoring* machine's business — the release is
		// chowned to the app account at deploy. Recording the builder's uid
		// would either be wrong or leak the platform account's identity.
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if src == nil {
			return nil
		}
		// Refused at build time rather than published: both unpackers stop at
		// maxUnpackBytes, so a bigger tree deployed and served from disk, then
		// could never be restored — every eviction, rollback and isolated start
		// an integrity error on a healthy artifact (AS-10).
		if written+fi.Size() > packLimit {
			return fmt.Errorf("the release is larger than the %s limit a release may unpack to "+
				"(at %s, %d bytes in): shrink what the build leaves in the release directory",
				humanBytes(packLimit), rel, written)
		}
		// No more than the header promised: a file growing under the pack
		// would otherwise fail it with "write too long".
		n, err := io.CopyN(tw, src, fi.Size())
		written += n
		return err
	})
	if err != nil {
		tw.Close()
		gz.Close()
		return err
	}

	if err := tw.Close(); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
}

// unpack reads a gzipped tar from r into dest, which must already exist.
//
// Every entry's path is checked against dest before anything is written. A tar
// is attacker-influenced input — it is built from a repo Toft cloned —
// and an entry named "../../etc/cron.d/x" or a symlink to "/" is how an
// unpacker gets used to write outside the tree it was pointed at.
func unpack(r io.Reader, dest string) error {
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
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
		return fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var written int64

	// Directory modes are applied last: a 0500 directory written early would
	// stop its own children being created.
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
			return fmt.Errorf("%w: %v", ErrIntegrity, err)
		}

		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return err
		}
		// Nothing is ever written *through* a symlink this artifact planted.
		// That, not the existence of the link, is how an unpacker gets used to
		// write outside the tree it was pointed at. Checked before anything is
		// created, so a refused entry leaves nothing behind.
		if err := contained(root, filepath.Dir(target)); err != nil {
			return fmt.Errorf("%w: entry %q: %v", ErrIntegrity, hdr.Name, err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			// The entry's own name too: it may already exist as a symlink out of
			// the tree, and the deferred Chmod below would follow it.
			if err := contained(root, target); err != nil {
				return fmt.Errorf("%w: entry %q: %v", ErrIntegrity, hdr.Name, err)
			}
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{target, hdr.FileInfo().Mode().Perm()})

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			// O_NOFOLLOW covers the last component, which the parent check
			// cannot: an artifact listing a symlink and then a regular file at
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
				// A short read here is the stream ending mid-entry: a
				// truncated transfer, not a filesystem problem. Reporting it
				// as anything but an integrity failure would let a partial
				// tree look like a successful restore of a smaller app.
				return fmt.Errorf("%w: reading %s: %v", ErrIntegrity, hdr.Name, err)
			}
			written += n
			if written > maxUnpackBytes {
				return fmt.Errorf("%w: artifact expands beyond %d bytes", ErrIntegrity, int64(maxUnpackBytes))
			}

		case tar.TypeSymlink:
			// Restored whatever it points at, including outside the tree. A
			// Python venv cannot be shipped otherwise — `uv venv` writes
			// `venv/bin/python -> /usr/bin/python3` — and a symlink on its own
			// is inert. What is refused is *writing through* one, above.
			if hdr.Linkname == "" {
				return fmt.Errorf("%w: symlink with an empty target", ErrIntegrity)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			if err := notADirectory(target); err != nil {
				return fmt.Errorf("%w: entry %q: %v", ErrIntegrity, hdr.Name, err)
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
				return fmt.Errorf("%w: entry %q: %v", ErrIntegrity, hdr.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			if err := notADirectory(target); err != nil {
				return fmt.Errorf("%w: entry %q: %v", ErrIntegrity, hdr.Name, err)
			}
			os.Remove(target)
			if err := os.Link(source, target); err != nil {
				return err
			}

		default:
			// Devices, fifos and sockets are not packed and are not restored.
			continue
		}
	}

	for i := len(dirs) - 1; i >= 0; i-- {
		if err := chmodDir(dirs[i].path, dirs[i].mode); err != nil {
			if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
				return fmt.Errorf("%w: %v", ErrIntegrity, err)
			}
			return err
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

// safeJoin resolves name against root and refuses anything that escapes it.
func safeJoin(root, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: entry with an empty name", ErrIntegrity)
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: absolute path in artifact: %q", ErrIntegrity, name)
	}
	target := filepath.Join(root, filepath.FromSlash(name))
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: path escapes the release directory: %q", ErrIntegrity, name)
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
// release on the way back out of the store — which is what made an isolated
// Python workload impossible in practice.
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
// Kept byte-for-byte in step with internal/runner's copy, which the round-trip
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
