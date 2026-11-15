//go:build !unix

package runner

import (
	"os"
	"syscall"
)

// ExitAs ends the runner with its app's exit code.
func ExitAs(e AppExit) { os.Exit(e.Code) }

// signalName is the signal by its own description where SIGxxx names are not
// to hand.
func signalName(sig syscall.Signal) string { return sig.String() }
