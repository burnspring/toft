package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The admin guide's checks reference lists every check by hand, with a column
// — *What it reads* — that the registry does not hold, so it is checked rather
// than generated (open-gaps PRD D12). Its ids, their order and the counts in
// its prose must be the registry's. The page is in the toft-docs checkout;
// scripts/check-reference-pages.sh points TOFT_DOCS_DIR at it, and without it
// this skips, so the repository's tests never depend on a sibling checkout.
func TestTheChecksReferencePageMatchesTheRegistry(t *testing.T) {
	dir := os.Getenv("TOFT_DOCS_DIR")
	if dir == "" {
		t.Skip("TOFT_DOCS_DIR is unset; scripts/check-reference-pages.sh sets it")
	}
	b, err := os.ReadFile(filepath.Join(dir, "src/content/docs/admin/reference/checks-reference.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range checksPageProblems(string(b), checks) {
		t.Error(p)
	}
}

var (
	checksPageRow   = regexp.MustCompile("^\\| `([a-z0-9_]+)` \\|")
	checksPageCount = regexp.MustCompile(`All (\d+) system checks|There are \*\*(\d+)\*\*`)
	checksPageCats  = regexp.MustCompile(`\*\*\d+\*\*, in (\w+) categories`)
)

var spelledCount = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
	"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12}

// checksPageProblems is every way page differs from reg, each with what to
// write: a missing row comes back formatted, its prose cell left for a person.
func checksPageProblems(page string, reg []checkDef) []string {
	// The rows are the table headed "| Id |" — the page has others, such as
	// the statuses, whose first cells are backticked too.
	var listed []string
	inTable := false
	for _, line := range strings.Split(page, "\n") {
		switch {
		case strings.HasPrefix(line, "| Id |"):
			inTable = true
		case !strings.HasPrefix(line, "|"):
			inTable = false
		case inTable:
			if m := checksPageRow.FindStringSubmatch(line); m != nil {
				listed = append(listed, m[1])
			}
		}
	}
	var problems []string
	onPage := map[string]bool{}
	for _, id := range listed {
		onPage[id] = true
	}
	inReg := map[string]bool{}
	cats := map[string]bool{}
	for _, c := range reg {
		inReg[c.id] = true
		cats[c.category] = true
		if !onPage[c.id] {
			problems = append(problems, fmt.Sprintf("missing from the page; add, after its registry neighbour:\n| `%s` | %s | %s | — |", c.id, c.name, c.category))
		}
	}
	for _, id := range listed {
		if !inReg[id] {
			problems = append(problems, fmt.Sprintf("`%s` is on the page and not in the registry; delete its row", id))
		}
	}
	if len(problems) == 0 {
		for i, c := range reg {
			if listed[i] != c.id {
				problems = append(problems, fmt.Sprintf("row %d is `%s`; the report runs `%s` there — the page is in report order", i+1, listed[i], c.id))
				break
			}
		}
	}
	for _, m := range checksPageCount.FindAllStringSubmatch(page, -1) {
		n, _ := strconv.Atoi(m[1] + m[2])
		if n != len(reg) {
			problems = append(problems, fmt.Sprintf("%q: the registry has %d", m[0], len(reg)))
		}
	}
	if m := checksPageCats.FindStringSubmatch(page); m != nil && spelledCount[m[1]] != len(cats) {
		problems = append(problems, fmt.Sprintf("%q: the registry has %d categories", m[0], len(cats)))
	}
	return problems
}

func TestTheChecksPageCheckerCatchesAMissingRowAndAStaleCount(t *testing.T) {
	reg := []checkDef{
		{id: "database", name: "Database", category: catPlatform},
		{id: "agent", name: "Shared-mode agent", category: catExecution},
	}
	page := "description: All 1 system checks in report order\n\nThere are **1**, in two categories.\n\n" +
		"| Status | Means |\n|---|---|\n| `skip` | Not applicable. |\n\n" +
		"| Id | Name | Category | What it reads |\n|---|---|---|---|\n| `database` | Database | Platform | The schema. |\n"
	got := strings.Join(checksPageProblems(page, reg), "\n")
	for _, want := range []string{
		"| `agent` | Shared-mode agent | Execution | — |",
		`"All 1 system checks": the registry has 2`,
		`"There are **1**": the registry has 2`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("problems lack %q:\n%s", want, got)
		}
	}

	fixed := strings.ReplaceAll(page, "1", "2") + "| `agent` | Shared-mode agent | Execution | The agent. |\n"
	if p := checksPageProblems(fixed, reg); len(p) != 0 {
		t.Errorf("a page that matches still fails: %v", p)
	}
	swapped := strings.ReplaceAll(page, "1", "2")
	swapped = strings.Replace(swapped, "| `database`", "| `agent` | Shared-mode agent | Execution | The agent. |\n| `database`", 1)
	if p := checksPageProblems(swapped, reg); len(p) != 1 || !strings.Contains(p[0], "report order") {
		t.Errorf("rows out of order = %v, want one report-order problem", p)
	}
}
