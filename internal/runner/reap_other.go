//go:build !linux

package runner

// becomeSubreaper is Linux's alone; elsewhere orphans go to init as usual.
func becomeSubreaper() {}

// waitExited cannot observe an exit without reaping it here; the runner waits
// on the app's output as it always did.
func waitExited(int) bool { return false }
