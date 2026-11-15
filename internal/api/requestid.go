package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// ctxKeyRequestID carries the id this request is known by. Resolved once at the
// top of the chain so the response header and the audit row cannot disagree
// about it — two places deriving it independently is how they would.
type ctxKeyRequestID struct{}

// maxRequestID bounds an inbound id. `audit_log.request_id` is TEXT and would
// store whatever arrived, and the value is written back onto the response, so
// the bound lives here rather than at either of the two places that would
// otherwise each have to remember it.
const maxRequestID = 128

// requestIDMiddleware resolves the request id and puts it on the response.
//
// The id is not new — `requestMetaMiddleware` has always written one into every
// audit row. What was missing is that it was disclosed nowhere: a person
// reporting an error had nothing to quote, and an operator holding an audit row
// had nothing to match it against. Echoing it closes that loop for every audited
// mutation. What makes an *unaudited* call attributable is the access log, which
// is now built (ADR §46) and is handed this id through `setAccessRequestID`
// below — so a 429 or a 500 an audit row will never see can still be found by
// the id the caller was given.
//
// **An untrusted caller may not choose it.** The inbound header is honoured only
// through a trusted proxy — ADR §31 D6's rule for `traceparent` and
// `realIPMiddleware`'s for `X-Forwarded-For`, here for the same reason. An id
// the caller picks is one they can aim at somebody else's audit row, or fill
// with whatever text an administrator will later read out of that column.
//
// Mounted above the rate limiters and the origin check, so a 429 and a rejected
// origin carry an id too: those are precisely the responses somebody reports.
// Platform router only — the apps gateway is the proxy path, and an app's
// response headers are the app's.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := ""
		if viaTrustedProxy(r) {
			rid = sanitizeRequestID(r.Header.Get("X-Request-ID"))
		}
		if rid == "" {
			rid = uuid.New().String()
		}
		w.Header().Set("X-Request-ID", rid)
		// The access log is outermost and will never see the context built
		// below, so it is told through its slot instead.
		setAccessRequestID(r.Context(), rid)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID{}, rid)))
	})
}

// requestIDFrom returns the id requestIDMiddleware resolved, or "" where it is
// not installed.
func requestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return v
}

// sanitizeRequestID accepts a bounded run of printable ASCII and returns ""
// — meaning "mint one instead" — for anything else.
//
// Go replaces CR and LF on the way out, so this is not what stops header
// splitting. It stops a correlation id from being somewhere to put a paragraph.
func sanitizeRequestID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxRequestID {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return ""
		}
	}
	return s
}
