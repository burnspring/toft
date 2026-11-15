package api

import (
	"strings"
	"testing"

	"toft/internal/runtime"
)

// The Logs tab masks what the copy masks, on an app's lines only: the
// platform's own voice composes nothing Toft minted.
func TestTheLogsTabMasksWhatToftMinted(t *testing.T) {
	lines := maskAppLines([]runtime.LogLine{
		{Source: runtime.LogSourceApp, Line: "TOFT_APP_TOKEN=tofta_abcdefgh12345678"},
		{Source: runtime.LogSourcePlatform, Line: "==> Starting"},
	})
	if strings.Contains(lines[0].Line, "abcdefgh12345678") {
		t.Errorf("an app token reached the tab: %q", lines[0].Line)
	}
	if lines[1].Line != "==> Starting" {
		t.Errorf("a platform line changed: %q", lines[1].Line)
	}
}
