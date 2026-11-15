package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"toft/cli/internal/client"
)

// The headers are sent verbatim and nothing is added.
//
// They are covered by the signature, so an extra header is as fatal as a
// missing one and the object store answers 403 either way. This is the property
// that keeps `write` meaning new-objects-only on the wire: the condition
// arrives as `If-None-Match: *`, and a client that "helpfully" dropped it would
// not be uploading anyway — it would be sending an invalid signature.
func TestUploadSendsTheSignedHeadersVerbatim(t *testing.T) {
	var got http.Header
	var gotMethod string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e, _ := testEnv()
	e.ctx = context.Background()

	signed := &signedRequest{
		URL:    srv.URL + "/acme-data/team/reports/nightly.parquet",
		Method: http.MethodPut,
		Headers: map[string]string{
			"If-None-Match":  "*",
			"Content-Md5":    "3q2+7w==",
			"Content-Length": "5",
			// Host is the one exception: net/http takes it from the URL, and
			// setting it as a header sends it twice.
			"Host": "should-not-be-set-as-a-header",
		},
	}
	if err := transfer(e, signed, strings.NewReader("hello"), 5); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT — the verb is the install's to choose", gotMethod)
	}
	if gotBody != "hello" {
		t.Errorf("body = %q, want the file's bytes", gotBody)
	}
	if got.Get("If-None-Match") != "*" {
		t.Error("If-None-Match was not sent; the `write` rung would then permit an overwrite")
	}
	if got.Get("Content-Md5") != "3q2+7w==" {
		t.Error("the digest header was not sent; the object store cannot check the body")
	}
	if got.Get("Host") == "should-not-be-set-as-a-header" {
		t.Error("Host was set as a header as well as taken from the URL")
	}
}

// An empty file goes up with a Content-Length of 0, never chunked.
//
// net/http sends a non-nil body of unknown length chunked, and an *os.File of
// size 0 is exactly that; S3 answers a chunked presigned PUT with 501 "A header
// you provided implies functionality that is not implemented". `toft store put`
// of an empty file failed that way on trytoft (2026-10-23) while `store cp`,
// which special-cased it, did not.
func TestAnEmptyUploadIsNotChunked(t *testing.T) {
	var te []string
	var cl int64 = -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		te, cl = r.TransferEncoding, r.ContentLength
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e, _ := testEnv()
	e.ctx = context.Background()
	f, err := os.CreateTemp(t.TempDir(), "empty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	signed := &signedRequest{URL: srv.URL + "/bucket/empty.txt", Method: http.MethodPut}
	if err := transfer(e, signed, f, 0); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if len(te) != 0 || cl != 0 {
		t.Errorf("Transfer-Encoding = %v, Content-Length = %d; want no encoding and 0", te, cl)
	}
}

// The install's refusals and the object store's are different pages, and the
// exit code is how a red pipeline tells them apart without reading the log.
//
// A 403 from the install is a credential or an access problem — exit 3. The
// upload itself failing is the platform doing the thing and it failing —
// exit 1. Conflating them sends somebody to rotate a token that was fine.
func TestStoreFailuresExitOnTheRightCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"no grant on the store", &client.Error{Status: http.StatusForbidden,
			Message: `your access to this store is "read", and this needs "write"`}, exitAuth},
		{"a revoked key", &client.Error{Status: http.StatusUnauthorized}, exitAuth},
		{"a name the server would not take", usageCodef("x", "a name must not contain \"..\" segments"), exitUsage},
		{"the upload itself failing", errors.New("the object store answered 500"), exitFailed},
		{"the file already exists under a write grant",
			errors.New("that name already exists in this store"), exitFailed},
		{"the object store unreachable, not the install",
			&client.UnreachableError{URL: "s3.eu-west-1.amazonaws.com",
				Err: errors.New("no route to host")}, exitUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := testEnv()
			if got := report(e, tc.err); got != tc.want {
				t.Errorf("report(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// When the transfer cannot reach the object store, the error names *that* host.
//
// The bytes go straight to the store, so a network that reaches the install
// does not necessarily reach the bucket — a proxy-only corporate network is the
// ordinary case. "Upload failed" sends somebody to ask why Toft is broken.
func TestAnUnreachableStoreNamesTheStore(t *testing.T) {
	e, _ := testEnv()
	e.ctx = context.Background()

	signed := &signedRequest{
		// A port nothing is listening on, so the dial fails locally rather than
		// making the test depend on DNS or on being offline.
		URL:     "http://127.0.0.1:1/acme-data/team/x",
		Method:  http.MethodPut,
		Headers: map[string]string{},
	}
	err := transfer(e, signed, strings.NewReader("x"), 1)
	if err == nil {
		t.Fatal("transfer to a closed port returned nil")
	}

	var unreachable *client.UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("err = %v, want an UnreachableError so the exit code is 4", err)
	}
	if !strings.Contains(unreachable.URL, "127.0.0.1:1") {
		t.Errorf("the error names %q, want the object store's host", unreachable.URL)
	}
	if !strings.Contains(err.Error(), "not Toft") {
		t.Errorf("the error should say the install is not the thing that failed: %v", err)
	}
}

// A 412 from the object store is the `write` rung refusing to replace something,
// and it is worth its own sentence: the raw answer is "PreconditionFailed",
// which explains nothing to somebody who did not know their grant was `write`.
func TestOverwriteRefusalExplainsTheRung(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusPreconditionFailed,
		Body:       io.NopCloser(strings.NewReader(`<Error><Code>PreconditionFailed</Code></Error>`)),
	}
	err := objectStoreRefusal(resp)
	if err == nil {
		t.Fatal("a 412 was not an error")
	}
	for _, want := range []string{"already exists", "write", "full"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// An S3 error body is forty lines of XML around one sentence, and the sentence
// is the only part worth printing.
func TestObjectStoreErrorsAreSummarized(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message>` +
		`<Key>team/x</Key><RequestId>ABC</RequestId></Error>`
	if got := summarizeXML([]byte(body)); got != "The specified key does not exist." {
		t.Errorf("summarizeXML = %q, want the Message alone", got)
	}
	// Something that is not S3's XML at all still prints, bounded.
	if got := summarizeXML([]byte(strings.Repeat("x", 500))); len(got) > 210 {
		t.Errorf("an unrecognised body printed %d characters", len(got))
	}
}

// The name is a path and its separators are part of it, but each segment is
// escaped — otherwise a space or a `#` in a filename would truncate the URL.
func TestObjectNamesAreEscapedSegmentwise(t *testing.T) {
	cases := map[string]string{
		"nightly.parquet":       "nightly.parquet",
		"2026/08/17.parquet":    "2026/08/17.parquet",
		"reports/q3 summary.md": "reports/q3%20summary.md",
		"reports/a#b.csv":       "reports/a%23b.csv",
	}
	for name, want := range cases {
		if got := escapeObjectName(name); got != want {
			t.Errorf("escapeObjectName(%q) = %q, want %q", name, got, want)
		}
	}
}

// Both digests come from one pass over the file, and they are the digests of
// the same bytes. A mismatch here would be an upload the object store refuses
// for a reason nobody could reproduce.
func TestDigestsAreOfTheSameBytes(t *testing.T) {
	sha, md5sum, err := digests(strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("digests: %v", err)
	}
	// Known values, so a change to the hashing is a failure rather than a
	// tautology comparing the code to itself.
	const wantSHA = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	const wantMD5 = "5d41402abc4b2a76b9719d911017c592"
	if sha != wantSHA {
		t.Errorf("sha256 = %q, want %q", sha, wantSHA)
	}
	if md5sum != wantMD5 {
		t.Errorf("md5 = %q, want %q", md5sum, wantMD5)
	}
}

// `toft store ls` shows folders, and can be pointed at one.
//
// The API has always answered with both — objects and the common prefixes one
// level down — and this command decoded the folders away, then refused the
// `store/folder` spelling by name. An app archiving under a generated name put
// its objects somewhere `toft` could neither list nor reach, and the refusal
// blamed access, which sends an operator to an administrator for a grant they
// already hold.
func TestStoreListShowsFoldersAndCanBePointedAtOne(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Query().Get("path"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("path") == "backup" {
			io.WriteString(w, `{"objects":[{"name":"2026-09-03.tar.gz","size":52000,`+
				`"last_modified":"2026-09-03T01:59:00Z"}],"access":"full"}`)
			return
		}
		io.WriteString(w, `{"objects":[{"name":"sales.db","size":84000,`+
			`"last_modified":"2026-09-03T01:57:00Z"}],"prefixes":["backup/"],`+
			`"access":"full"}`)
	}))
	defer srv.Close()

	s := &session{client: client.New(srv.URL, "tok", "toft-test")}

	e, buf := testEnv()
	if err := listObjects(e, s, &globalFlags{}, mustRemote(t, "drop"), false); err != nil {
		t.Fatalf("listing the root: %v", err)
	}
	root := buf.String()
	if !strings.Contains(root, "toft://drop/backup/") {
		t.Errorf("the folder is missing from the listing:\n%s", root)
	}
	if !strings.Contains(root, "toft://drop/sales.db") {
		t.Errorf("the objects are missing from the listing:\n%s", root)
	}

	e, buf = testEnv()
	// The trailing slash is what the line above prints, so it has to be
	// something this command takes back.
	if err := listObjects(e, s, &globalFlags{}, mustRemote(t, "drop/backup/"), false); err != nil {
		t.Fatalf("listing the folder: %v", err)
	}
	if !strings.Contains(buf.String(), "toft://drop/backup/2026-09-03.tar.gz") {
		t.Errorf("the folder's own objects are missing:\n%s", buf.String())
	}
	if len(asked) != 2 || asked[0] != "" || asked[1] != "backup" {
		t.Errorf("path parameters sent = %q, want [\"\" \"backup\"]", asked)
	}
}

// A folder reported on every page of a paged listing is one folder.
func TestStoreListDoesNotRepeatAFolderAcrossPages(t *testing.T) {
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page++
		if page == 1 {
			io.WriteString(w, `{"objects":[],"prefixes":["backup/"],"next_cursor":"c1"}`)
			return
		}
		io.WriteString(w, `{"objects":[],"prefixes":["backup/"]}`)
	}))
	defer srv.Close()

	e, buf := testEnv()
	s := &session{client: client.New(srv.URL, "tok", "toft-test")}
	if err := listObjects(e, s, &globalFlags{}, mustRemote(t, "drop"), false); err != nil {
		t.Fatalf("listing: %v", err)
	}
	if got := strings.Count(buf.String(), "drop/backup/"); got != 1 {
		t.Errorf("the folder was printed %d times, want once:\n%s", got, buf.String())
	}
}

func mustRemote(t *testing.T, arg string) remoteRef {
	t.Helper()
	ref, err := parseRemote(arg)
	if err != nil {
		t.Fatal(err)
	}
	ref.dir = true
	return ref
}

// An object store this machine cannot reach is refused naming its host, and
// never printing the presigned URL: its query is a signature, a credential id
// and a session token that write the key for an hour, and the line lands in CI
// logs (docs/bugs 2026-10-22-e2e-storage #2).
func TestAnUnreachableStoreNeverPrintsTheSignedURL(t *testing.T) {
	signed := "https://bucket.s3.us-east-1.amazonaws.com/e2e/x.txt?X-Amz-Credential=ASIAXXXX%2F20261022" +
		"&X-Amz-Security-Token=IQoJsecret&X-Amz-Signature=deadbeef"
	// The error a failed dial hands back: a *url.Error carrying the whole URL.
	err := unreachableStore(signed, &url.Error{Op: "Put", URL: signed, Err: errors.New("connection refused")})
	msg := err.Error()
	for _, secret := range []string{"X-Amz-Signature", "X-Amz-Security-Token", "X-Amz-Credential", "deadbeef", "IQoJ"} {
		if strings.Contains(msg, secret) {
			t.Errorf("the refusal prints %s: %s", secret, msg)
		}
	}
	if !strings.Contains(msg, "bucket.s3.us-east-1.amazonaws.com") || !strings.Contains(msg, "connection refused") {
		t.Errorf("the refusal must still name the host and the transport's error: %s", msg)
	}
	var u *client.UnreachableError
	if !errors.As(err, &u) {
		t.Errorf("not an UnreachableError, so not exit 4: %T", err)
	}
	// A batch whose every item failed the same way is that refusal, exit 4.
	if b := batchError([]error{err, err}, 2); !errors.As(b, &u) {
		t.Errorf("a batch of unreachable failures became %T", b)
	}
}
