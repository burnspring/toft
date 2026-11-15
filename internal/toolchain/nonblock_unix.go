//go:build unix

package toolchain

import "syscall"

// OpenNonblock makes an open of a FIFO return at once rather than wait for a
// writer. On a regular file it changes nothing.
const OpenNonblock = syscall.O_NONBLOCK
