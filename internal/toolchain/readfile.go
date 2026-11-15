package toolchain

import (
	"fmt"
	"io"
	"os"
)

// MaxManifestBytes caps a manifest the platform reads out of an app's tree. A
// requirements.txt or a DESCRIPTION is kilobytes; a link to /dev/zero is not.
const MaxManifestBytes = 16 << 20

// ReadRegularFile reads path when it is a regular file, at most max bytes,
// without blocking on what it opens.
//
// **For a file in a tree an app's account can rewrite.** os.ReadFile there is
// two ways to stall the platform: a FIFO blocks the open until a writer
// appears — holding a build worker, or the dependency sweep, forever — and a
// link to an endless device grows the process until it is killed (security
// review 2026-11-10, E-6). Opened non-blocking and checked on the descriptor,
// neither can happen; anything but a regular file is an error.
func ReadRegularFile(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|OpenNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadRegular(f, path, max)
}

// ReadRegular is ReadRegularFile for a file already opened (non-blocking).
func ReadRegular(f *os.File, name string, max int64) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "read", Path: name, Err: fmt.Errorf("not a regular file")}
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, &os.PathError{Op: "read", Path: name, Err: fmt.Errorf("larger than %d bytes", max)}
	}
	return data, nil
}
