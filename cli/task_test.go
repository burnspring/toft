package main

import (
	"errors"
	"strings"
	"testing"
)

// Every spelling of the old command is refused with the replacement named,
// and as a usage error with a code a script can branch on.
func TestTaskIsRefusedWithTheReplacement(t *testing.T) {
	for _, args := range [][]string{nil, {"list"}, {"run", "digest", "--app", "demo"}, {"--help"}} {
		err := cmdTask(&env{}, args)
		var ue *usageError
		if !errors.As(err, &ue) || ue.code != "removed_command" {
			t.Fatalf("toft task %v: %v, want a removed_command usage error", args, err)
		}
		if !strings.Contains(err.Error(), "[jobs.<name>]") {
			t.Errorf("toft task %v: %q does not name the replacement", args, err)
		}
	}
}
