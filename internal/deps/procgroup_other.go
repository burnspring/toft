//go:build !unix

package deps

import "os/exec"

// ownGroup is the unix process-group kill; elsewhere only WaitDelay bounds an
// orphan's hold on the pipes.
func ownGroup(*exec.Cmd) {}
