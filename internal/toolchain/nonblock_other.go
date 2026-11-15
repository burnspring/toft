//go:build !unix

package toolchain

// OpenNonblock is unix's O_NONBLOCK; elsewhere there are no FIFOs to open.
const OpenNonblock = 0
