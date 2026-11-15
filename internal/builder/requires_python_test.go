package builder

import (
	"errors"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/toolchain"
)

// With no pin, requires-python moves the build off the install default only
// when it excludes the default, onto the newest version it allows (ADR §64).
func TestRequiresPythonMovesOnlyAnExcludedDefault(t *testing.T) {
	set := []toolchain.Version{
		{Version: "3.11.9"}, {Version: "3.12.7"}, {Version: "3.13.5", Default: true},
	}
	resolve := func(pyproject string) (*resolvedRuntime, error) {
		dir := t.TempDir()
		if pyproject != "" {
			write(t, dir, "pyproject.toml", pyproject)
		}
		r := &resolvedRuntime{Language: toolchain.Python, Chosen: set[2], BuildWith: set[2], Where: "this build host"}
		return r, r.followRequiresPython(dir, set)
	}

	r, err := resolve("[project]\nrequires-python = \">=3.10,<3.13\"\n")
	if err != nil || r.Chosen.Version != "3.12.7" || r.RequiresPython != ">=3.10,<3.13" || r.Excluded != "3.13.5" {
		t.Errorf("an excluded default: %+v, %v", r, err)
	}
	r, _ = resolve("[project]\nrequires-python = \"==3.11.*\"\n")
	if r.Chosen.Version != "3.11.9" {
		t.Errorf("==3.11.* chose %s", r.Chosen.Version)
	}

	// A lower bound the default satisfies moves nothing — not to the newest.
	for _, py := range []string{
		"[project]\nrequires-python = \">=3.8\"\n",
		"[project]\nname = \"x\"\n",
		"", // no pyproject.toml
		"[project]\nrequires-python = \">=3.14rc1\"\n", // unreadable: no guess
	} {
		r, err := resolve(py)
		if err != nil || r.Chosen.Version != "3.13.5" || r.RequiresPython != "" {
			t.Errorf("%q moved the default: %+v, %v", py, r, err)
		}
	}

	// Nothing satisfies it: refused, carrying the default for a repair.
	_, err = resolve("[project]\nrequires-python = \">=3.14\"\n")
	var unsatisfied *RequiresPythonError
	if !errors.As(err, &unsatisfied) || unsatisfied.Default.Chosen.Version != "3.13.5" {
		t.Fatalf("unsatisfiable = %v", err)
	}
	for _, want := range []string{`">=3.14"`, "3.13", "[runtime] python", ManifestFile} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not carry %q", err, want)
		}
	}
}

// The runtime's warning joins the manifest's rather than replacing it.
func TestDeployWarningsAccumulate(t *testing.T) {
	d := &database.Deployment{ID: "d1"}
	b := &Builder{}
	b.addDeployWarnings(d, []string{"first"})
	b.addDeployWarnings(d, nil)
	b.addDeployWarnings(d, []string{"second"})
	if got := DecodeWarnings(d.Warnings); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("warnings = %v", got)
	}
}
