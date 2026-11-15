package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

// A bare sentence gains a code from its status, a retryable from its status,
// and no hint — a generic code has no act worth naming.
func TestErrorEnvelope_BareSentenceIsCompleted(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusNotFound, map[string]string{"error": "deployment not found"})
	body := decodeBody(t, rec)
	if body["error"] != "deployment not found" || body["code"] != "not_found" || body["retryable"] != false {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["hint"]; ok {
		t.Fatalf("a generic code should carry no hint: %v", body)
	}
}

// The call site's own keys win: a code it named, a retryable it decided.
func TestErrorEnvelope_CallSiteKeysWin(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusConflict, map[string]any{
		"error": "busy", "code": "no_release", "retryable": true, "hint": "mine"})
	body := decodeBody(t, rec)
	if body["code"] != "no_release" || body["retryable"] != true || body["hint"] != "mine" {
		t.Fatalf("body = %v", body)
	}
}

// A named code with a hint gets the hint; a code that means "wait" is
// retryable at a status that otherwise is not.
func TestErrorEnvelope_KnownCodes(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusUnauthorized, map[string]string{"error": "expired", "code": "key_expired"})
	body := decodeBody(t, rec)
	if body["retryable"] != false || body["hint"] != errorHints["key_expired"] {
		t.Fatalf("body = %v", body)
	}

	rec = httptest.NewRecorder()
	writeJSON(rec, http.StatusConflict, map[string]any{"error": "full", "code": "queue_full", "in_flight": 3})
	body = decodeBody(t, rec)
	if body["retryable"] != true || body["in_flight"] != float64(3) {
		t.Fatalf("body = %v", body)
	}
}

func TestErrorEnvelope_RetryableByStatus(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusBadRequest:          false,
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusNotImplemented:      false,
		http.StatusServiceUnavailable:  true,
	} {
		rec := httptest.NewRecorder()
		writeJSON(rec, status, map[string]string{"error": "x"})
		if got := decodeBody(t, rec)["retryable"]; got != want {
			t.Errorf("%d: retryable = %v, want %v", status, got, want)
		}
	}
}

// Only error bodies are touched: a success, a list, a struct, and an error
// status whose body is not keyed `error` all pass through as written.
func TestErrorEnvelope_LeavesOtherBodiesAlone(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]string{"error": "not really"})
	if _, ok := decodeBody(t, rec)["code"]; ok {
		t.Fatal("a 200 was completed")
	}
	rec = httptest.NewRecorder()
	writeJSON(rec, http.StatusBadRequest, map[string]any{"detail": "scim"})
	if body := decodeBody(t, rec); len(body) != 1 {
		t.Fatalf("a body with no error key was completed: %v", body)
	}
	rec = httptest.NewRecorder()
	writeJSON(rec, http.StatusConflict, struct {
		Error string `json:"error"`
	}{"s"})
	if body := decodeBody(t, rec); len(body) != 1 {
		t.Fatalf("a struct was completed: %v", body)
	}
}

// The call site's map is not mutated — a handler may reuse it.
func TestErrorEnvelope_DoesNotMutate(t *testing.T) {
	m := map[string]any{"error": "x"}
	writeJSON(httptest.NewRecorder(), http.StatusBadRequest, m)
	if len(m) != 1 {
		t.Fatalf("map mutated: %v", m)
	}
}

// The rsconnect subtree answers in Connect's shape: no string code beside the
// sentence, from a handler or from a helper it shares with the rest.
func TestErrorEnvelope_ConnectSubtreeIsVerbatim(t *testing.T) {
	h := verbatimErrors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/connect/v1/content/x", nil))
	if body := decodeBody(t, rec); len(body) != 1 {
		t.Fatalf("connect error completed: %v", body)
	}
}

// refusalBody decodes an error body whose envelope carries a bool beside the
// strings — `retryable` — reading every string field as before. A test that
// decoded a refusal into map[string]string stopped decoding at all when the
// envelope arrived, which is the same break an outside client meets.
type refusalBody map[string]string

func (b *refusalBody) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*b = refusalBody{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			(*b)[k] = s
		}
	}
	return nil
}
