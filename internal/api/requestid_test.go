package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// requestIDSeenBy runs one request through requestIDMiddleware and reports both
// halves of the contract: what the handler was told, and what the caller was.
func requestIDSeenBy(t *testing.T, trusted bool, inbound string) (inCtx, onResponse string) {
	t.Helper()
	h := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inCtx = requestIDFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if inbound != "" {
		req.Header.Set("X-Request-ID", inbound)
	}
	if trusted {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyViaProxy{}, true))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return inCtx, rec.Header().Get("X-Request-ID")
}

// The point of the change: the id is disclosed. Without this an audit row's
// request_id matches nothing anybody outside the database has ever seen.
func TestRequestIDIsEchoedAndMatchesTheContext(t *testing.T) {
	inCtx, onResp := requestIDSeenBy(t, false, "")
	if inCtx == "" {
		t.Fatal("no request id in context")
	}
	if onResp != inCtx {
		t.Errorf("response header %q does not match the id handlers see %q", onResp, inCtx)
	}
}

// An id a caller can choose is one they can aim at somebody else's audit row.
// Same rule as X-Forwarded-For in realIPMiddleware and traceparent in ADR §31 D6.
func TestRequestIDFromUntrustedPeerIsIgnored(t *testing.T) {
	inCtx, onResp := requestIDSeenBy(t, false, "chosen-by-the-caller")
	if inCtx == "chosen-by-the-caller" {
		t.Error("an untrusted caller chose the request id")
	}
	if onResp != inCtx {
		t.Errorf("response header %q does not match context %q", onResp, inCtx)
	}
}

// A proxy we believe about the client address is one we believe about the id it
// assigned, so a trace spanning the proxy and the platform stays one trace.
func TestRequestIDFromTrustedProxyIsHonoured(t *testing.T) {
	inCtx, onResp := requestIDSeenBy(t, true, "from-the-proxy")
	if inCtx != "from-the-proxy" {
		t.Errorf("trusted proxy's id was dropped: got %q", inCtx)
	}
	if onResp != "from-the-proxy" {
		t.Errorf("trusted proxy's id was not echoed: got %q", onResp)
	}
}

// Even a trusted proxy does not get to put arbitrary text in a column an
// administrator reads, or on the response.
func TestRequestIDIsSanitizedEvenFromATrustedProxy(t *testing.T) {
	cases := map[string]string{
		"too long":  strings.Repeat("a", maxRequestID+1),
		"newline":   "abc\ndef",
		"space":     "abc def",
		"control":   "abc\x00def",
		"non-ascii": "abcé",
		"empty":     "   ",
	}
	for name, inbound := range cases {
		t.Run(name, func(t *testing.T) {
			inCtx, _ := requestIDSeenBy(t, true, inbound)
			if inCtx == strings.TrimSpace(inbound) {
				t.Errorf("%q was accepted as a request id", inbound)
			}
			if inCtx == "" {
				t.Error("a rejected inbound id must be replaced, not dropped")
			}
		})
	}
}

// The address an audit row records is an address, whichever way the request
// arrived. A direct connection carries an ephemeral source port on RemoteAddr
// and a proxied one does not, and a column that holds both shapes cannot be
// searched for the thing it is named after.
func TestRequestMetaRecordsAddressWithoutThePort(t *testing.T) {
	cases := map[string]struct{ remoteAddr, want string }{
		"direct":        {"198.51.100.4:51234", "198.51.100.4"},
		"ipv6 direct":   {"[2001:db8::1]:51234", "2001:db8::1"},
		"proxy-derived": {"198.51.100.4", "198.51.100.4"},
		"ipv6 proxied":  {"2001:db8::1", "2001:db8::1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got string
			h := requestMetaMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = requestMeta(r.Context()).IP
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != tc.want {
				t.Errorf("recorded %q, want %q", got, tc.want)
			}
		})
	}
}

// The audit row and the response header name the same request. These are two
// middlewares and nothing but this test holds them together.
func TestRequestMetaUsesTheEchoedID(t *testing.T) {
	var got string
	h := requestIDMiddleware(requestMetaMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = requestMeta(r.Context()).RequestID
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got == "" {
		t.Fatal("no request id on the audit metadata")
	}
	if echoed := rec.Header().Get("X-Request-ID"); echoed != got {
		t.Errorf("audit row would say %q, the caller was told %q", got, echoed)
	}
}
