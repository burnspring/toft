package api

import (
	"encoding/csv"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
)

// packages.csv carries strings an app's lockfile controls, so a version that
// is a formula reaches the spreadsheet as text, as the viewer CSV's does.
func TestPackagesCSVNeutralisesFormulae(t *testing.T) {
	w := httptest.NewRecorder()
	writePackageCSV(w, database.PackageOccurrences{
		Incomparable: []database.PackageOccurrence{{
			Slug: "app", Name: "@evil", Package: "+pkg", Version: `=HYPERLINK("http://evil/")`,
			Path: "\t-2+3", Ecosystem: "npm",
		}},
	})
	rows, err := csv.NewReader(w.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	r := rows[1]
	for i, want := range map[int]string{1: "'@evil", 4: "'+pkg", 5: `'=HYPERLINK("http://evil/")`, 8: "'\t-2+3"} {
		if r[i] != want {
			t.Errorf("column %d = %q, want %q", i, r[i], want)
		}
	}
}
