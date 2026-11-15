package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// BB-052 and BB-060, fifth 2026-11-03 black-box list: a size refusal's hint
// names the limit, and a destination refusal names the integration sent to.
func TestNotifyRefusalsNameTheLimitAndTheIntegration(t *testing.T) {
	read := func(rec *httptest.ResponseRecorder) map[string]string {
		var b map[string]string
		json.Unmarshal(rec.Body.Bytes(), &b)
		return b
	}
	rec := httptest.NewRecorder()
	writeDestinationRefusal(rec, "Email", "channel", destinationFields["channel"])
	b := read(rec)
	if !strings.Contains(b["error"], "Email") || strings.Contains(b["error"], "Slack") {
		t.Errorf("destination refusal = %q, want it to name Email and not Slack", b["error"])
	}
	if h := notifyFilesHint(25 << 20); !strings.Contains(h, "26214400") {
		t.Errorf("files hint = %q, want the limit", h)
	}
	rec = httptest.NewRecorder()
	writeNotifyTooLarge(rec, "too long", "shorten the text to 3000 characters")
	if b := read(rec); b["code"] != "too_large" || !strings.Contains(b["hint"], "3000") {
		t.Errorf("too_large = %v", b)
	}
}
