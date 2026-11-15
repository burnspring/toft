//go:build unix

package runner

import "syscall"

func setAppUmask() { syscall.Umask(0o007) }
