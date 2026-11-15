package builder

import (
	"testing"

	"toft/internal/database"
)

// What a render schedule may re-run: the three document frameworks, and
// nothing that executes per viewer or runs a server.
func TestRerenderableIsTheDocumentFrameworks(t *testing.T) {
	for fw, want := range map[Framework]bool{
		FrameworkNotebook:    true,
		FrameworkRMarkdown:   true,
		FrameworkQuarto:      true,
		FrameworkMarimo:      false,
		FrameworkShiny:       false,
		FrameworkStreamlit:   false,
		FrameworkStatic:      false,
		FrameworkUnsupported: false,
	} {
		if got := Rerenderable(string(fw)); got != want {
			t.Errorf("Rerenderable(%q) = %v, want %v", fw, got, want)
		}
	}
}

// A stopped document is not re-rendered on its schedule — the flip would write
// `running` over a person's Stop and mail the recipients — while a sleeping one
// is (code review 2026-11-11, C-2).
func TestARenderScheduleDeclinesAStoppedApp(t *testing.T) {
	for status, want := range map[string]bool{
		database.AppStatusStopped: false, database.AppStatusSleeping: true,
		database.AppStatusRunning: true, database.AppStatusCrashed: true,
	} {
		if got := renderFiresFor(status); got != want {
			t.Errorf("renderFiresFor(%s) = %v, want %v", status, got, want)
		}
	}
}
