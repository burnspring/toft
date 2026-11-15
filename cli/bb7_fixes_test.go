package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"toft/cli/internal/client"
)

// BB-086: a flag where a subcommand goes is a subcommand left out, not a
// subcommand named "--json"; BB-083: an unknown subcommand's hint names the
// command's own --help, never "toft store url --help".
func TestAFlagWhereASubcommandGoesIsAMissingOne(t *testing.T) {
	bb3Env(t)
	for _, args := range [][]string{{"env", "--json"}, {"deployments", "--app", "x", "--json"}} {
		stdout, _ := captureStd(t)
		run(args)
		doc := lastDoc(t, stdout())
		if doc["code"] != "missing_argument" || strings.Contains(doc["error"].(string), `"--`) {
			t.Errorf("%v: %v, want missing_argument not naming a flag as a subcommand", args, doc)
		}
	}
	stdout, _ := captureStd(t)
	run([]string{"store", "url", "toft://x/a.txt", "--json"})
	doc := lastDoc(t, stdout())
	if doc["code"] != "unknown_command" || doc["hint"] != "`toft store --help` lists its subcommands" {
		t.Errorf("store url: %v", doc)
	}
}

// BB-082: an install's hintless not_found on a run gains this client's hint.
func TestNoSuchNameCarriesAHint(t *testing.T) {
	err := noSuchName(&client.Error{Status: http.StatusNotFound, Code: "not_found", Message: "run not found"}, "toft job runs")
	var u *usageError
	if !errors.As(err, &u) || u.hint != "`toft job runs` names them" {
		t.Errorf("noSuchName = %#v", err)
	}
}
