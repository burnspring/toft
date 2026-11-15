package database_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// **A `--` inside a string literal hides that statement's terminating
// semicolon from goose.**
//
// The statement then runs on into the next one, which Postgres executes anyway
// and with the full text intact — so this is invisible until the affected
// statement is the *last* in a `+goose Up` or `+goose Down` block, at which
// point there is nothing to run on into and goose fails the whole migration with
// `unexpected unfinished SQL query`. That failure happens at **startup**, on the
// install that pulls the release.
//
// Established by bisection against a real database on 2026-09-02: the same
// COMMENT parses with a statement after it and fails without. It is worth a
// guard because nothing else in the tree catches it — the schema snapshot
// regenerates against a database, the API suite runs against one already at
// head, and every other test in this package uses a fixture.
//
// It reads the text rather than importing goose's parser, which is not exported:
// what is being checked is a property of the file, and a test that could only
// fail by running the same parser would be checking that goose agrees with
// itself.
func TestNoCommentMarkerInsideAStringLiteral(t *testing.T) {
	// **Two lines predate the rule and are not to be copied.** Both are a
	// COMMENT with a statement after it, so both are being merged into their
	// neighbour today and both work. Rewriting them would rewrite applied
	// migrations, which changes nothing on an install that has already run
	// them; they are named here so that the guard is a rule going forward and
	// not a claim that the file is clean.
	grandfathered := map[string]int{
		"00037_schema.sql":                 1094,
		"00041_permitted_login_groups.sql": 53,
	}

	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("reading migrations: %v", err)
	}

	checked := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		checked++
		body, err := os.ReadFile(filepath.Join("migrations", entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		for n, line := range strings.Split(string(body), "\n") {
			if commentMarkerInsideAString(line) < 0 {
				continue
			}
			if grandfathered[entry.Name()] == n+1 {
				continue
			}
			t.Errorf("%s:%d has `--` inside a string literal. goose will miss this "+
				"statement's semicolon and merge it into the next one — and fail the "+
				"migration outright if it is the last statement in its block. Use a "+
				"semicolon, a colon or parentheses instead:\n    %s",
				entry.Name(), n+1, strings.TrimSpace(line))
		}
	}
	if checked == 0 {
		t.Fatal("no migrations were read — this guard checked nothing")
	}
}

// commentMarkerInsideAString returns the offset of a `--` that falls inside a
// single-quoted string, or -1.
//
// Doubled quotes (`”`) are SQL's escape, and the state machine falling out and
// straight back in is the same thing they mean. A `--` *outside* a string ends
// the scan: the rest of the line is a comment and cannot open a literal.
func commentMarkerInsideAString(line string) int {
	inString := false
	for i := 0; i < len(line)-1; i++ {
		if line[i] == '\'' {
			inString = !inString
			continue
		}
		if line[i] == '-' && line[i+1] == '-' {
			if inString {
				return i
			}
			return -1
		}
	}
	return -1
}
