package telemetry

import (
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// forbidden is every key D7 says may never leave this process, plus a fuzzed
// tail. Written out rather than generated so that adding one is a deliberate
// act with a name attached — this list is half the specification.
var forbidden = []attribute.KeyValue{
	attribute.String("user.email", "someone@example.com"),
	attribute.String("user.name", "Someone"),
	attribute.String("session.id", "sess_abc123"),
	attribute.String("http.request.header.authorization", "Bearer abcdef123456"),
	attribute.String("http.request.header.cookie", "toft_session=abc"),
	attribute.String("url.query", "?token=abc123"),
	attribute.String("url.full", "https://toft.example.com/apps/x?token=abc"),
	attribute.String("toft.app.slug", "quarterly-report"),
	attribute.String("toft.env.DATABASE_URL", "postgres://u:p@db/toft"),
	attribute.String("toft.git.url", "https://github.com/acme/secret-repo"),
	attribute.String("db.query.text", "SELECT * FROM users"),
	attribute.String("toft.act_for", "signed-blob"),
	attribute.String("slack.channel", "#finance"),
	attribute.String("client.address", "10.1.2.3"),
	attribute.String("toft.api_key", "toft_livekey1234567890"),
}

// TestRedactionDropsUnknownAttributes is the property, stated as a property:
// no exported span ever carried a key outside the allow-list.
//
// The alternative — "the call sites are careful" — is testable only as a code
// review, and a code review is not a test. This is why redaction is a stage on
// the export path rather than a convention at the call site (ADR §31 D7).
func TestRedactionDropsUnknownAttributes(t *testing.T) {
	attrs := append([]attribute.KeyValue{}, forbidden...)
	// A fuzzed tail, so a key nobody thought of is covered by the same rule.
	for _, k := range []string{"a", "zz.zz", "toft.something.new", "custom.metric.label", "x-internal"} {
		attrs = append(attrs, attribute.String(k, "value"))
	}
	// ...and one of each allowed kind, which must survive.
	attrs = append(attrs,
		attribute.String("http.route", "/api/v1/apps/{appID}"),
		attribute.String("toft.app.id", "8b0f6a5e-0000-0000-0000-000000000000"),
		attribute.String("toft.outcome", OutcomeSucceeded),
	)

	stub := tracetest.SpanStub{Name: "redact-me", Attributes: attrs}
	out := redact(stub.Snapshot())

	got := map[attribute.Key]bool{}
	for _, kv := range out.Attributes() {
		got[kv.Key] = true
		if !allowedSpanAttributes[kv.Key] && kv.Key != enduserIDKey {
			t.Errorf("attribute %q survived redaction and is not on the allow-list", kv.Key)
		}
	}
	for _, want := range []attribute.Key{"http.route", "toft.app.id", "toft.outcome"} {
		if !got[want] {
			t.Errorf("allow-listed attribute %q was dropped", want)
		}
	}
}

// TestRedactionCoversEventAttributes matters because RecordError puts the
// exception's type and message on an *event*, not on the span — a filter that
// only walked span attributes would look correct and leak anyway.
func TestRedactionCoversEventAttributes(t *testing.T) {
	stub := tracetest.SpanStub{
		Name: "with-events",
		Events: []sdktrace.Event{{
			Name: "exception",
			Attributes: []attribute.KeyValue{
				attribute.String("exception.type", "*errors.errorString"),
				attribute.String("exception.message", "connect to postgres://toft:s3cr3tpassword@db:5432/toft failed"),
				attribute.String("user.email", "someone@example.com"),
			},
		}},
	}
	out := redact(stub.Snapshot())

	events := out.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	for _, kv := range events[0].Attributes {
		if kv.Key == "user.email" {
			t.Error("an event attribute outside the allow-list survived")
		}
		if kv.Key == "exception.message" && strings.Contains(kv.Value.AsString(), "s3cr3tpassword") {
			t.Errorf("the connection password rode out on an exception message: %q", kv.Value.AsString())
		}
	}
}

// TestExceptionMessageScrubbed seeds the scrubber with the values a real
// install actually holds.
//
// Seeding from live config rather than from a list written in the test is the
// point of Options.Secrets: a secret added to config.Config later and passed in
// is scrubbed for free, and one that is *not* passed in shows up here as a
// visible gap rather than as a silent leak.
func TestExceptionMessageScrubbed(t *testing.T) {
	const jwtSecret = "0123456789abcdef0123456789abcdef"
	const encryptionKey = "fedcba9876543210fedcba9876543210"
	newScrubber([]string{jwtSecret, encryptionKey, "short"})
	t.Cleanup(func() { newScrubber(nil) })

	cases := []struct {
		name, in, mustNotContain string
	}{
		{"app key", "key toft_9f8e7d6c5b4a39281706 was refused", "toft_9f8e7d6c5b4a39281706"},
		{"personal key", "toftu_aabbccddeeff00112233 is locked", "toftu_aabbccddeeff00112233"},
		{"app token", "tofta_0a1b2c3d4e5f60718293 expired", "tofta_0a1b2c3d4e5f60718293"},
		{"deploy key", "X-API-Key: toftd_5e4d3c2b1a0918273645 refused", "toftd_5e4d3c2b1a0918273645"},
		{"exchanged key", "key toftx_0f1e2d3c4b5a69788796a5b4 revoked", "toftx_0f1e2d3c4b5a69788796a5b4"},
		{"bearer", "upstream said 401 for Authorization: Bearer eyJhbGciOiJIUzI1NiJ9abc", "Bearer eyJ"},
		{"connection url", "dial postgres://toft:hunter2@db:5432/toft: refused", "hunter2"},
		{"jwt secret", "signing failed with key " + jwtSecret, jwtSecret},
		{"encryption key", "decrypt: " + encryptionKey + " is wrong length", encryptionKey},
		{"private key", "parse: -----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----", "MIIabc"},
		{"session jwt", "bad token eyJhbGciOi.eyJzdWIiOiJ4.c2lnbmF0dXJl", "eyJzdWIiOiJ4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrub(tc.in)
			if strings.Contains(got, tc.mustNotContain) {
				t.Errorf("scrub(%q) = %q, still carries %q", tc.in, got, tc.mustNotContain)
			}
		})
	}

	// Short values are deliberately not scrubbed: a three-character "secret"
	// matches inside ordinary words and turns every message into redaction
	// confetti, which is worse than the leak it was guarding against.
	if got := scrub("the short answer is no"); !strings.Contains(got, "short") {
		t.Errorf("a value under the length floor was scrubbed anyway: %q", got)
	}
	// `toft_` is the credential prefix and also the cookie and metric prefix.
	// The key pattern requires the hex body every key is minted with, so the
	// names stay readable in the log lines that are about them.
	for _, name := range []string{"toft_session", "toft_app_session", "toft_build_info", "toft_apps_by_status"} {
		if got := scrub("rejected " + name + " for this request"); !strings.Contains(got, name) {
			t.Errorf("scrub redacted the name %q, which is not a credential: %q", name, got)
		}
	}
}

func TestExceptionMessageTruncated(t *testing.T) {
	long := strings.Repeat("x", maxExceptionMessage*3)
	got := scrub(long)
	if len(got) > maxExceptionMessage+4 { // +4: the ellipsis is three bytes
		t.Errorf("scrubbed message is %d bytes, want no more than %d", len(got), maxExceptionMessage)
	}
}

// TestEnduserIDIsOffByDefault, and on when asked for — and never on a metric at
// either setting, which is the D5 rule that has no override.
func TestEnduserIDIsOffByDefault(t *testing.T) {
	recordEnduserID.Store(false)
	stub := tracetest.SpanStub{Name: "s", Attributes: []attribute.KeyValue{
		attribute.String(string(enduserIDKey), "8b0f6a5e-0000-0000-0000-000000000000"),
	}}
	if attrs := redact(stub.Snapshot()).Attributes(); len(attrs) != 0 {
		t.Errorf("enduser.id survived with OTEL_RECORD_ENDUSER_ID off: %v", attrs)
	}

	recordEnduserID.Store(true)
	t.Cleanup(func() { recordEnduserID.Store(false) })
	attrs := redact(stub.Snapshot()).Attributes()
	if len(attrs) != 1 || attrs[0].Key != enduserIDKey {
		t.Errorf("enduser.id was dropped with the flag on: %v", attrs)
	}

	// The other half of the rule: no *metric* may declare it, at any setting.
	for _, s := range Inventory {
		for _, a := range s.Attrs {
			if a.Key == string(enduserIDKey) {
				t.Errorf("%s declares enduser.id as a metric attribute; that is a span attribute only", s.Name)
			}
		}
	}
}

// TestExportedStringsAreValidUTF8 is TL-2. protobuf-go refuses to marshal a
// proto3 string that is not UTF-8, and the exporter marshals a batch as one
// message, so one `GET /%ff` would lose every span exported beside it.
func TestExportedStringsAreValidUTF8(t *testing.T) {
	stub := tracetest.SpanStub{Name: "bad-bytes", Attributes: []attribute.KeyValue{
		attribute.String("url.path", "/\xff"),
		attribute.String("http.route", UnmatchedRoute),
	}}
	out := redact(stub.Snapshot())
	found := false
	for _, kv := range out.Attributes() {
		if kv.Key == "url.path" {
			found = true
			if !utf8.ValidString(kv.Value.AsString()) {
				t.Errorf("url.path survived redaction as invalid UTF-8: %q", kv.Value.AsString())
			}
		}
	}
	if !found {
		t.Fatal("url.path was dropped; it is allow-listed")
	}

	got := scrub(strings.Repeat("x", 255) + "é" + strings.Repeat("y", 50))
	if !utf8.ValidString(got) {
		t.Errorf("scrub cut through a rune: %q", got)
	}
	if len(got) > maxExceptionMessage+3 {
		t.Errorf("scrubbed message is %d bytes, want at most %d", len(got), maxExceptionMessage+3)
	}
	if got := scrub("bad \xff\xfe name"); !utf8.ValidString(got) {
		t.Errorf("scrub passed invalid UTF-8 through: %q", got)
	}
}
