package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNotifyRefusalsCarryAHint: too_large and destination_not_allowed name
// the next step in hint, as every coded refusal does (third 2026-11-03
// black-box list).
func TestNotifyRefusalsCarryAHint(t *testing.T) {
	read := func(rec *httptest.ResponseRecorder) map[string]any {
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	rec := httptest.NewRecorder()
	if !tooLarge(rec, &http.MaxBytesError{Limit: 10}) {
		t.Fatal("tooLarge did not answer a body cap")
	}
	if b := read(rec); b["code"] != "too_large" || b["hint"] == "" || b["hint"] == nil {
		t.Fatalf("too_large without hint: %v", b)
	}
	if tooLarge(httptest.NewRecorder(), errors.New("other")) {
		t.Fatal("tooLarge answered an error that is not the cap")
	}
	rec = httptest.NewRecorder()
	writeDestinationRefusal(rec, "Email", "channel", "the channel")
	if b := read(rec); b["code"] != "destination_not_allowed" || b["hint"] == nil {
		t.Fatalf("destination_not_allowed without hint: %v", b)
	} else if msg, _ := b["error"].(string); !strings.HasPrefix(msg, `a send to Email does not take "channel"`) {
		// BB-073: "a Email send" read wrong.
		t.Errorf("error = %q", msg)
	}
	if appDataTooLargeHint == "" {
		t.Fatal("app data too_large has no hint")
	}
}
