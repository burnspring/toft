package builder

import "testing"

func TestAppKind(t *testing.T) {
	for fw, want := range map[string]string{
		"flask": AppKindServing, "nextjs": AppKindServing, "shiny": AppKindServing,
		"marimo": AppKindServing, "plumber": AppKindServing, "": AppKindServing,
		"static": AppKindRendered, "notebook": AppKindRendered,
		"rmarkdown": AppKindRendered, "quarto": AppKindRendered,
		"batch-python": AppKindBatch, "batch-r": AppKindBatch,
	} {
		if got := AppKind(fw); got != want {
			t.Errorf("AppKind(%q) = %q, want %q", fw, got, want)
		}
	}
}
