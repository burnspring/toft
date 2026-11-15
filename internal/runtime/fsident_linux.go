//go:build linux

package runtime

import (
	"fmt"
	goruntime "runtime"
	"syscall"
	"unsafe"
)

// WithAccountFS runs fn on an OS thread whose filesystem identity is the
// account's: its uid and primary gid for every permission check, and its own
// group list — the per-app group that gates apps/<id> among them.
//
// It is how the platform, running as root, writes into a tree it has handed to
// an account without walking that tree as root. Every path fn opens is resolved
// with the account's permissions, so a link the account planted reaches only
// what the account could already reach, and what fn creates is the account's
// own. The kernel drops the filesystem capabilities from the thread the moment
// its fsuid leaves 0 (capabilities(7)), so DAC_OVERRIDE does not follow it.
//
// Three properties hold it together, and each is load-bearing:
//
//   - **Thread-local calls only.** setfsuid, setfsgid and the raw setgroups
//     syscall change the calling thread alone. syscall.Setgroups would not do —
//     since Go 1.16 it applies to every thread in the process.
//   - **The thread never goes back.** The goroutine locks its thread and exits
//     without unlocking it, and the Go runtime then destroys the thread rather
//     than return it to the pool. There is no restore to get wrong.
//   - **fn must do its file work on this goroutine.** A goroutine fn starts runs
//     on some other thread, as root. Everything handed to this today is plain
//     sequential file I/O.
//
// A nil account runs fn as the process itself, which is what an install that
// separates nothing asks for.
func WithAccountFS(a *AppAccount, fn func() error) error {
	if a == nil {
		return fn()
	}
	done := make(chan error, 1)
	go func() {
		goruntime.LockOSThread()
		// Deliberately never unlocked: see above.
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("writing as %s: %v", a.Username, r)
			}
		}()
		groups := []uint32{uint32(a.GID)}
		if a.GroupGID != 0 {
			groups = append(groups, uint32(a.GroupGID))
		}
		if _, _, errno := syscall.RawSyscall(syscall.SYS_SETGROUPS,
			uintptr(len(groups)), uintptr(unsafe.Pointer(&groups[0])), 0); errno != 0 {
			done <- fmt.Errorf("taking %s's groups for a write: %w", a.Username, errno)
			return
		}
		syscall.RawSyscall(syscall.SYS_SETFSGID, uintptr(a.GID), 0, 0)
		syscall.RawSyscall(syscall.SYS_SETFSUID, uintptr(a.UID), 0, 0)
		// setfsuid reports the previous value and never an error, so the only
		// way to know it took is to ask again with an id it will refuse.
		minusOne := -1
		if cur, _, _ := syscall.RawSyscall(syscall.SYS_SETFSUID, uintptr(minusOne), 0, 0); int(cur) != a.UID {
			done <- fmt.Errorf("could not take %s's identity for a write (fsuid is %d)", a.Username, int(cur))
			return
		}
		done <- fn()
	}()
	return <-done
}
