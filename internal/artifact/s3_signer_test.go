package artifact

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func newTestSigner(t *testing.T, endpoint string) *S3Signer {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))

	signer, err := NewS3Signer(context.Background(), S3SignerConfig{
		Endpoint: endpoint, Region: "us-east-1", UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Signer: %v", err)
	}
	return signer
}

// The bucket is an argument, not a field. Every other presign on this platform
// addresses the artifact store's one bucket; a store is somebody else's, named
// by the administrator who created the resource.
func TestSignerAddressesAnyBucket(t *testing.T) {
	signer := newTestSigner(t, "http://s3.example.invalid")

	for _, bucket := range []string{"acme-data", "other-tenant"} {
		req, err := signer.PresignGet(context.Background(), bucket, "team/reports/x.parquet", time.Minute, "")
		if err != nil {
			t.Fatalf("PresignGet(%s): %v", bucket, err)
		}
		u, err := url.Parse(req.URL)
		if err != nil {
			t.Fatalf("parsing %q: %v", req.URL, err)
		}
		if want := "/" + bucket + "/team/reports/x.parquet"; u.Path != want {
			t.Errorf("path = %q, want %q", u.Path, want)
		}
		if req.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", req.Method)
		}
	}
}

// `If-None-Match: *` is the ladder's `write` rung expressed on the wire — new
// objects only. It has to be *signed*, not suggested: a client that drops the
// header must invalidate the signature rather than escape the condition.
func TestWriteConditionIsSignedNotSuggested(t *testing.T) {
	signer := newTestSigner(t, "http://s3.example.invalid")
	ctx := context.Background()

	conditional, err := signer.PresignPut(ctx, "acme-data", "team/reports/x.parquet", time.Minute,
		PutConditions{IfNoneMatch: true, ChecksumSHA256: "3q2+7w==", ContentMD5: "3q2+7w==", ContentLength: 12})
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if got := conditional.Headers["If-None-Match"]; got != "*" {
		t.Fatalf("If-None-Match = %q, want %q — the condition must be returned so the client sends it", got, "*")
	}
	// Signed, which is what makes dropping it fatal rather than permissive.
	if !signedHeaderList(t, conditional.URL, "if-none-match") {
		t.Error("If-None-Match is not in X-Amz-SignedHeaders; a client could drop it and still upload")
	}

	// Content-MD5 is the digest that is enforceable on every S3-compatible
	// server, and it is enforceable *because* it stays a signed header — the
	// presigner hoists every x-amz-* header into the query string, where a
	// server does not look for a checksum. TestSignedChecksumIsEnforced is the
	// other half of this: that a real server rejects a body that does not match.
	if got := conditional.Headers["Content-Md5"]; got != "3q2+7w==" {
		t.Errorf("Content-MD5 header = %q, want the caller's digest", got)
	}
	if !signedHeaderList(t, conditional.URL, "content-md5") {
		t.Error("Content-MD5 is not in X-Amz-SignedHeaders; a server would accept a body that does not match")
	}
	// The length is signed too, so the URL cannot upload something other than
	// the object whose size was declared and checked against the 5 GiB ceiling.
	if !signedHeaderList(t, conditional.URL, "content-length") {
		t.Error("Content-Length is not signed; the declared size is then a suggestion")
	}

	// `overwrite` and `full` are unconditional, and the header must be absent
	// rather than empty — an empty If-None-Match is not "no condition".
	unconditional, err := signer.PresignPut(ctx, "acme-data", "team/reports/x.parquet", time.Minute,
		PutConditions{ChecksumSHA256: "3q2+7w==", ContentMD5: "3q2+7w==", ContentLength: 12})
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if _, ok := unconditional.Headers["If-None-Match"]; ok {
		t.Error("an unconditional PUT carries no If-None-Match")
	}
	if signedHeaderList(t, unconditional.URL, "if-none-match") {
		t.Error("an unconditional PUT must not sign If-None-Match")
	}
}

// The SDK's default is to compute a CRC32 for every PutObject. On a presigned
// URL that means signing a header the client has no way to know it must send,
// and the upload fails with a signature mismatch that looks like a clock
// problem. Every PUT here carries the caller's SHA-256 instead.
func TestSignerAddsNoChecksumOfItsOwn(t *testing.T) {
	signer := newTestSigner(t, "http://s3.example.invalid")

	req, err := signer.PresignPut(context.Background(), "acme-data", "team/x", time.Minute,
		PutConditions{ChecksumSHA256: "3q2+7w==", ContentMD5: "3q2+7w==", ContentLength: 3})
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	for name := range req.Headers {
		if name == "X-Amz-Checksum-Crc32" || name == "X-Amz-Sdk-Checksum-Algorithm" {
			t.Errorf("the signer added %s, which no client was told to send", name)
		}
	}
	if signedHeaderList(t, req.URL, "x-amz-sdk-checksum-algorithm") {
		t.Error("the signer signed a checksum algorithm the client was never told about")
	}
}

// A listing returns names relative to the prefix. The prefix is the containment
// boundary and it is not the caller's business — a name that carried it would
// invite a client to build keys out of it.
func TestListingNamesAreRelativeToThePrefix(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	signer := newTestSigner(t, srv.URL)
	ctx := context.Background()

	for _, key := range []string{"team/reports/2026-08.parquet", "team/reports/nested/a.csv", "team/other/x"} {
		if err := (&S3Store{client: signer.client, bucket: "acme-data"}).PutBlob(ctx, key, []byte("x")); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
	}

	page, err := signer.ListObjects(ctx, "acme-data", "team/reports/", "", "", 100)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	got := map[string]bool{}
	for _, obj := range page.Objects {
		got[obj.Name] = true
	}
	for _, want := range []string{"2026-08.parquet", "nested/a.csv"} {
		if !got[want] {
			t.Errorf("listing is missing %q; got %v", want, got)
		}
	}
	// A sibling prefix under the same bucket is not in this store.
	for name := range got {
		if name == "team/other/x" || name == "x" {
			t.Errorf("listing reached outside the prefix: %q", name)
		}
	}
}

// signedHeaderList reports whether a presigned URL's X-Amz-SignedHeaders names
// the header. A returned header that is not in this list is decoration.
func signedHeaderList(t *testing.T, raw, header string) bool {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	for _, name := range splitSemicolons(u.Query().Get("X-Amz-SignedHeaders")) {
		if name == header {
			return true
		}
	}
	return false
}

func splitSemicolons(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ';' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// A folder is a common prefix and nothing else, and the names on both halves of
// a page are relative to what was listed — the objects already were, and the
// prefixes have to be for the same reason: a name carrying the store's prefix
// is an invitation to build keys out of it.
//
// The strict-extension pair is the case worth pinning. "team/" and
// "team-archive/" share five characters and are different stores; listing one
// level of the bucket root must return both as folders and neither's contents,
// which is the same trailing-slash confusion storeKeyFor guards on the other
// side.
func TestDelimiterReturnsFoldersRelativeToThePrefix(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	signer := newTestSigner(t, srv.URL)
	ctx := context.Background()

	for _, key := range []string{
		"team/top.txt",
		"team/reports/2026-08.parquet",
		"team/reports/nested/a.csv",
		"team-archive/old.txt",
	} {
		if err := (&S3Store{client: signer.client, bucket: "acme-data"}).PutBlob(ctx, key, []byte("x")); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
	}

	// One level of the store "team/".
	page, err := signer.ListObjects(ctx, "acme-data", "team/", "", "/", 100)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(page.Objects) != 1 || page.Objects[0].Name != "top.txt" {
		t.Errorf("objects = %+v, want just top.txt", page.Objects)
	}
	if len(page.Prefixes) != 1 || page.Prefixes[0] != "reports/" {
		t.Errorf("prefixes = %v, want [reports/]", page.Prefixes)
	}

	// One level of the bucket root: two folders, no objects, and the sibling
	// store is a folder rather than part of this one.
	page, err = signer.ListObjects(ctx, "acme-data", "", "", "/", 100)
	if err != nil {
		t.Fatalf("ListObjects(root): %v", err)
	}
	if len(page.Objects) != 0 {
		t.Errorf("objects = %+v, want none at the root", page.Objects)
	}
	got := map[string]bool{}
	for _, p := range page.Prefixes {
		got[p] = true
	}
	if !got["team/"] || !got["team-archive/"] {
		t.Errorf("prefixes = %v, want both team/ and team-archive/", page.Prefixes)
	}

	// No delimiter is the flat listing this had before folders, unchanged.
	page, err = signer.ListObjects(ctx, "acme-data", "team/", "", "", 100)
	if err != nil {
		t.Fatalf("ListObjects(flat): %v", err)
	}
	if len(page.Objects) != 3 {
		t.Errorf("flat objects = %+v, want all three under team/", page.Objects)
	}
	if len(page.Prefixes) != 0 {
		t.Errorf("flat prefixes = %v, want none: a flat listing has no levels", page.Prefixes)
	}
}

// The disposition is signed, and it names the object rather than the URL.
//
// Signed is the security half: a caller cannot bolt a filename onto a URL the
// platform already minted, because the signature covers the query. Naming the
// object is the useful half — without it a browser saves the last path segment
// of a signed URL, or renders a CSV inline.
func TestDownloadDispositionIsSignedAndNamesTheObject(t *testing.T) {
	signer := newTestSigner(t, "http://s3.example.invalid")
	ctx := context.Background()

	plain, err := signer.PresignGet(ctx, "acme-data", "team/reports/sales.parquet", time.Minute, "")
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if strings.Contains(plain.URL, "response-content-disposition") {
		t.Errorf("a plain download carries a disposition it did not ask for: %s", plain.URL)
	}

	attached, err := signer.PresignGet(ctx, "acme-data", "team/reports/sales.parquet", time.Minute, "sales.parquet")
	if err != nil {
		t.Fatalf("PresignGet(attachment): %v", err)
	}
	u, err := url.Parse(attached.URL)
	if err != nil {
		t.Fatalf("parsing %q: %v", attached.URL, err)
	}
	got := u.Query().Get("response-content-disposition")
	if !strings.Contains(got, `filename="sales.parquet"`) {
		t.Errorf("disposition = %q, want it to name the object", got)
	}
	// Signed, not decoration: the query is covered by the signature, so a
	// SignedHeaders check is the wrong instrument — what proves it is that the
	// two URLs above have different signatures.
	if u.Query().Get("X-Amz-Signature") == "" {
		t.Fatal("no signature on the attachment URL")
	}
	plainURL, _ := url.Parse(plain.URL)
	if plainURL.Query().Get("X-Amz-Signature") == u.Query().Get("X-Amz-Signature") {
		t.Error("the disposition did not change the signature, so it is not signed")
	}
}

// A name a browser cannot spell in ASCII still has to leave a usable filename.
func TestDispositionCarriesBothSpellings(t *testing.T) {
	got := contentDisposition("売上.csv")
	if !strings.Contains(got, `filename="__.csv"`) {
		t.Errorf("%q has no ASCII fallback", got)
	}
	if !strings.Contains(got, "filename*=UTF-8''") {
		t.Errorf("%q has no RFC 5987 form", got)
	}
	if q := contentDisposition(`a"b.csv`); strings.Count(q, `"`) != 2 {
		t.Errorf("a quote in the name escaped its own header value: %q", q)
	}
}

// Where a signed URL can point, for the browser's connect-src.
func TestS3OriginsCoverTheAddressesASignedURLUses(t *testing.T) {
	got := s3Origins(S3SignerConfig{}, "eu-west-2")
	for _, want := range []string{"https://s3.eu-west-2.amazonaws.com", "https://*.s3.eu-west-2.amazonaws.com"} {
		if !slices.Contains(got, want) {
			t.Errorf("real S3 origins %v lack %s", got, want)
		}
	}
	if got := s3Origins(S3SignerConfig{Endpoint: "http://minio:9000", UsePathStyle: true}, ""); !slices.Equal(got, []string{"http://minio:9000"}) {
		t.Errorf("a path-style endpoint is itself alone: %v", got)
	}
}
