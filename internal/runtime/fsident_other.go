//go:build !linux

package runtime

import "errors"

// WithAccountFS is Linux's; nowhere else does an install run apps as accounts
// of their own, so there is no account to write as.
func WithAccountFS(a *AppAccount, fn func() error) error {
	if a == nil {
		return fn()
	}
	return errors.New("writing as another account needs Linux")
}
