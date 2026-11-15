//go:build !unix

package builder

import "os/exec"

// ownGroup and endGroup are the unix process-group kill; elsewhere only
// WaitDelay bounds an orphan's hold on the output.
func ownGroup(*exec.Cmd) {}

func endGroup(*exec.Cmd) {}

// asAccount: no app accounts exist off unix (runtime.Isolation), so there is
// never a credential to set.
func asAccount(*exec.Cmd, *accountCreds) {}
