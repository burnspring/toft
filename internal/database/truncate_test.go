package database

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A transport error echoing non-ASCII text, cut byte-wise mid-rune, made
// Postgres refuse the UPDATE that records a send's outcome, so the send sat
// queued (DB2-1). Both cutters keep the result valid for a text column.
func TestTheCuttersKeepTextValidForPostgres(t *testing.T) {
	s := "x" + strings.Repeat("é", 2000) // the 2000-byte cut lands mid-rune
	for name, got := range map[string]string{
		"truncate": truncate(s, 2000),
		"clip":     clip(s, 2000),
		"nul":      truncate("a\x00b", 10),
	} {
		if !utf8.ValidString(got) || strings.IndexByte(got, 0) >= 0 {
			t.Errorf("%s produced text a text column refuses", name)
		}
	}
}
