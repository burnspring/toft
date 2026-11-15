package artifact

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// These tests run the artifact store against a *real* S3-compatible server
// rather than the hand-written fake in s3_test.go. The fake pins the request
// shapes this code sends; only a real server proves the other half — that a
// server's actual responses, error bodies, and metadata handling are read
// correctly. A fake written by the same person as the client agrees with the
// client by construction, which is exactly the failure this closes.
//
// MinIO is the local stand-in, and it is not only a convenience: the buyers
// who require running in their own account are often not on AWS at all, so an
// on-premises S3-compatible store is a supported target in its own right.
//
//	docker compose --profile s3 up -d minio minio-init
//	export TEST_S3_ENDPOINT=http://127.0.0.1:9000
//	go test ./internal/artifact/
//
// Skipped when TEST_S3_ENDPOINT is unset, following the TEST_DATABASE_URL
// pattern, so `go test ./...` stays green with nothing running.
const (
	s3EndpointEnv = "TEST_S3_ENDPOINT"
	s3AccessEnv   = "TEST_S3_ACCESS_KEY"
	s3SecretEnv   = "TEST_S3_SECRET_KEY"
)

// liveS3Store returns a store backed by a bucket dedicated to this test, and
// removes the bucket when the test ends. A bucket per test keeps Expire's
// prefix sweeps from seeing another test's objects.
func liveS3Store(t *testing.T) (*S3Store, string) {
	t.Helper()

	endpoint := os.Getenv(s3EndpointEnv)
	if endpoint == "" {
		t.Skipf("%s not set — skipping live object-store test (see file docs)", s3EndpointEnv)
	}
	access := envOr(s3AccessEnv, "toft")
	secret := envOr(s3SecretEnv, "toft-dev-secret")

	// A bucket name derived from the test name: lowercase, no underscores.
	bucket := "toft-test-" + strings.ToLower(strings.NewReplacer(
		"_", "-", "/", "-", "TestS3Live", "",
	).Replace(t.Name()))
	if len(bucket) > 60 {
		bucket = bucket[:60]
	}

	ctx := context.Background()
	client := rawS3Client(t, endpoint, access, secret)

	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		// Already existing is fine; anything else means the server is not
		// usable and the test should say so rather than fail obscurely later.
		if !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") &&
			!strings.Contains(err.Error(), "BucketAlreadyExists") {
			t.Fatalf("creating test bucket %s at %s: %v", bucket, endpoint, err)
		}
	}
	t.Cleanup(func() { emptyAndRemoveBucket(client, bucket) })

	// Credentials reach the store through the ambient AWS environment, exactly
	// as they do on ECS via a task role.
	t.Setenv("AWS_ACCESS_KEY_ID", access)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))

	store, err := NewS3Store(ctx, S3Config{
		Bucket:   bucket,
		Prefix:   "install-a",
		Endpoint: endpoint,
		Region:   "us-east-1",
		// Required by MinIO and by most self-hosted S3-compatible servers,
		// which cannot do virtual-host addressing.
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	return store, bucket
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func rawS3Client(t *testing.T, endpoint, access, secret string) *s3.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(access, secret, "")),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
}

func emptyAndRemoveBucket(client *s3.Client, bucket string) {
	ctx := context.Background()
	p := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return
		}
		for _, obj := range page.Contents {
			client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: obj.Key})
		}
	}
	client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
}

// The whole point of the store: a built release survives the machine that
// built it, byte for byte, including the details that make a tree runnable.
func TestS3LiveRoundTrip(t *testing.T) {
	store, _ := liveS3Store(t)
	ctx := context.Background()

	src := t.TempDir()
	writeFileTree(t, src, map[string]string{
		"app.py":                  "print('hi')",
		"requirements.txt":        "flask\n",
		"venv/lib/site/mod.py":    "x = 1",
		"static/nested/deep/a.js": "console.log(1)",
	})
	// An executable binary and a symlink: a Go app is useless without the
	// executable bit, and a venv is mostly symlinks.
	if err := os.WriteFile(filepath.Join(src, "toft-app"), []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(src, "venv/bin"), 0755)
	os.WriteFile(filepath.Join(src, "venv/bin/python3.12"), []byte("binary"), 0755)
	if err := os.Symlink("python3.12", filepath.Join(src, "venv/bin/python")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	key := ReleaseKey("app-1", "deploy-1")
	desc, err := store.Put(ctx, key, src)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if len(desc.SHA256) != 64 || desc.Size == 0 {
		t.Fatalf("descriptor looks wrong: %+v", desc)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if err := store.Get(ctx, key, dest); err != nil {
		t.Fatalf("get: %v", err)
	}

	want, got := readTree(t, src), readTree(t, dest)
	if len(want) != len(got) {
		t.Fatalf("restored %d entries, packed %d\n want %v\n  got %v", len(got), len(want), want, got)
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("%s = %q, want %q", name, got[name], body)
		}
	}

	fi, err := os.Stat(filepath.Join(dest, "toft-app"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0111 == 0 {
		t.Errorf("executable bit lost through a real object store: %v", fi.Mode().Perm())
	}
	if li, err := os.Lstat(filepath.Join(dest, "venv/bin/python")); err == nil {
		if li.Mode()&os.ModeSymlink == 0 {
			t.Error("symlink was materialised as a regular file")
		}
	}
}

// The digest must survive as object metadata on a real server. Without it Get
// has nothing to verify against, ErrIntegrity becomes unreachable, and a
// corrupt transfer surfaces later as a mysteriously broken app.
func TestS3LiveDigestSurvivesAsMetadata(t *testing.T) {
	store, _ := liveS3Store(t)
	ctx := context.Background()

	src := t.TempDir()
	writeFileTree(t, src, map[string]string{"a.py": strings.Repeat("x", 100000)})

	key := ReleaseKey("app-1", "deploy-1")
	desc, err := store.Put(ctx, key, src)
	if err != nil {
		t.Fatal(err)
	}

	stat, err := store.Stat(ctx, key)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat.SHA256 != desc.SHA256 {
		t.Errorf("digest did not survive the round trip: stored %q, read back %q", desc.SHA256, stat.SHA256)
	}
	if stat.Size != desc.Size {
		t.Errorf("size = %d, want %d", stat.Size, desc.Size)
	}
}

// A real server's 404 must map to ErrNotFound, on both the GET and the HEAD
// path — they report a missing key differently, and only a real server shows
// which. Getting this wrong in the other direction is what matters: an outage
// read as "release gone" would tell an operator to redeploy every app.
func TestS3LiveMissingKeyIsNotFound(t *testing.T) {
	store, _ := liveS3Store(t)
	ctx := context.Background()

	if err := store.Get(ctx, ReleaseKey("app-1", "never-built"), t.TempDir()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get err = %v, want ErrNotFound", err)
	}
	if _, err := store.Stat(ctx, ReleaseKey("app-1", "never-built")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat err = %v, want ErrNotFound", err)
	}
}

// An unreachable server must be ErrUnreachable, never ErrNotFound.
func TestS3LiveUnreachableServerIsNotNotFound(t *testing.T) {
	if os.Getenv(s3EndpointEnv) == "" {
		t.Skipf("%s not set", s3EndpointEnv)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))

	// A port with nothing on it.
	store, err := NewS3Store(context.Background(), S3Config{
		Bucket: "whatever", Endpoint: "http://127.0.0.1:1", Region: "us-east-1", UsePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = store.Get(context.Background(), ReleaseKey("app-1", "deploy-1"), t.TempDir())
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("an unreachable store must never read as a missing release: %v", err)
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

// Corruption in the object must be caught by the digest rather than unpacked,
// and the existing tree must survive the failed restore untouched.
func TestS3LiveCorruptObjectIsRefused(t *testing.T) {
	store, bucket := liveS3Store(t)
	ctx := context.Background()

	src := t.TempDir()
	writeFileTree(t, src, map[string]string{"app.py": strings.Repeat("new", 20000)})

	key := ReleaseKey("app-1", "deploy-1")
	if _, err := store.Put(ctx, key, src); err != nil {
		t.Fatal(err)
	}

	// Overwrite the object's bytes while leaving the recorded digest alone.
	client := rawS3Client(t, os.Getenv(s3EndpointEnv),
		envOr(s3AccessEnv, "toft"), envOr(s3SecretEnv, "toft-dev-secret"))
	objKey := "install-a/" + key
	orig, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(objKey)})
	if err != nil {
		t.Fatal(err)
	}
	meta := orig.Metadata
	orig.Body.Close()
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(objKey),
		Body:     strings.NewReader("this is not a gzipped tarball"),
		Metadata: meta, // the original digest, now describing different bytes
	}); err != nil {
		t.Fatal(err)
	}

	// A destination already holding the previous, working release.
	dest := filepath.Join(t.TempDir(), "current")
	os.MkdirAll(dest, 0755)
	os.WriteFile(filepath.Join(dest, "app.py"), []byte("print('old')"), 0644)

	if err := store.Get(ctx, key, dest); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "app.py"))
	if err != nil || string(body) != "print('old')" {
		t.Errorf("a failed restore damaged the running release: %v %q", err, body)
	}
}

// Expire must sweep only this app's prefix, against a real paginated listing.
func TestS3LiveExpireKeepsExactlyTheNamedReleases(t *testing.T) {
	store, _ := liveS3Store(t)
	ctx := context.Background()

	src := t.TempDir()
	writeFileTree(t, src, map[string]string{"app.py": "x"})

	var keys []string
	for _, id := range []string{"d1", "d2", "d3"} {
		k := ReleaseKey("app-1", id)
		if _, err := store.Put(ctx, k, src); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	other := ReleaseKey("app-2", "d1")
	if _, err := store.Put(ctx, other, src); err != nil {
		t.Fatal(err)
	}

	removed, err := store.Expire(ctx, "app-1", keys[2:])
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d, want 2", removed)
	}
	if _, err := store.Stat(ctx, keys[2]); err != nil {
		t.Errorf("a kept release was removed: %v", err)
	}
	if _, err := store.Stat(ctx, other); err != nil {
		t.Errorf("another app's release was expired: %v", err)
	}
	for _, k := range keys[:2] {
		if _, err := store.Stat(ctx, k); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s should be gone, got %v", k, err)
		}
	}
}

// A release large enough to exercise a multi-part-sized body and a listing
// with real pagination behaviour, since a dependency tree is tens of thousands
// of small files and that is the shape the store actually carries.
func TestS3LiveHandlesAManyFileTree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the many-file tree in -short mode")
	}
	store, _ := liveS3Store(t)
	ctx := context.Background()

	src := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 2000; i++ {
		files[fmt.Sprintf("venv/lib/python3.12/site-packages/pkg%03d/mod%02d.py", i/20, i%20)] =
			fmt.Sprintf("VALUE = %d\n", i)
	}
	writeFileTree(t, src, files)

	key := ReleaseKey("app-1", "deploy-1")
	if _, err := store.Put(ctx, key, src); err != nil {
		t.Fatalf("put: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if err := store.Get(ctx, key, dest); err != nil {
		t.Fatalf("get: %v", err)
	}
	got := readTree(t, dest)
	if len(got) != len(files) {
		t.Fatalf("restored %d files, packed %d", len(got), len(files))
	}
	for name, body := range files {
		if got[name] != body {
			t.Errorf("%s = %q, want %q", name, got[name], body)
			break
		}
	}
}

// writeFileTree materialises files under an existing directory.
func writeFileTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// A workload must never hold an object-store credential — a credential that
// can read its own release can read every other app's. Presigned URLs are the
// answer, and only a real server proves the signature is accepted.
func TestS3LivePresignedURLsWorkAndAreScoped(t *testing.T) {
	store, _ := liveS3Store(t)
	ctx := context.Background()

	key := ReleaseKey("app-1", "deploy-1")
	if _, err := store.Put(ctx, key, writeTreeAt(t, map[string]string{"app.py": "print('hi')"})); err != nil {
		t.Fatal(err)
	}

	url, err := store.PresignGet(ctx, key, 10*time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	// A plain HTTP client with no AWS credentials at all — which is exactly
	// what a workload is.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("fetching the presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("presigned GET returned %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("presigned GET returned no bytes")
	}

	// The signature is scoped to one object: the same URL with a different key
	// must not work, or the whole point is lost.
	other := strings.Replace(url, "deploy-1.tar.gz", "deploy-2.tar.gz", 1)
	if other != url {
		resp2, err := http.Get(other)
		if err == nil {
			defer resp2.Body.Close()
			if resp2.StatusCode == http.StatusOK {
				t.Error("a presigned URL was reusable for a different object")
			}
		}
	}
}

// A job writes its outputs through a presigned PUT, for the same reason.
func TestS3LivePresignedPutAcceptsAnOutput(t *testing.T) {
	store, _ := liveS3Store(t)
	ctx := context.Background()

	key := RunOutputKey("app-1", "run-7", "report.csv")
	url, err := store.PresignPut(ctx, key, 10*time.Minute)
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader("a,b\n1,2\n"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("uploading: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("presigned PUT returned %d: %s", resp.StatusCode, body)
	}

	// And the platform can read back what the job wrote.
	readURL, err := store.PresignGet(ctx, key, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, err := http.Get(readURL)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	content, _ := io.ReadAll(got.Body)
	if string(content) != "a,b\n1,2\n" {
		t.Errorf("read back %q", content)
	}
}

// A local store says plainly that it cannot presign rather than returning
// something that fails later somewhere less obvious.
func TestLocalStoreCannotPresign(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PresignGet(context.Background(), "k", time.Minute); err == nil {
		t.Error("a local store should refuse to presign")
	}
	if _, err := store.PresignPut(context.Background(), "k", time.Minute); err == nil {
		t.Error("a local store should refuse to presign")
	}
}

// writeTreeAt materialises a tree and returns its directory.
func writeTreeAt(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	writeFileTree(t, dir, files)
	return dir
}

// Run keys keep every run's data under its own prefix, so a signing mistake
// still lands inside the run rather than in another app's data.
func TestRunKeysAreScopedToTheirRun(t *testing.T) {
	in := RunInputKey("app-1", "run-7")
	out := RunOutputKey("app-1", "run-7", "report.csv")
	prefix := RunPrefix("app-1", "run-7")

	for _, k := range []string{in, out} {
		if !strings.HasPrefix(k, prefix) {
			t.Errorf("%q is not under its run prefix %q", k, prefix)
		}
	}
	if strings.HasPrefix(RunInputKey("app-2", "run-7"), prefix) {
		t.Error("another app's run shares this run's prefix")
	}
	if strings.HasPrefix(RunInputKey("app-1", "run-8"), prefix) {
		t.Error("another run of the same app shares this prefix")
	}
}

// --- The store signer, against a real server ---

// liveS3Signer returns a signer and a bucket dedicated to this test.
//
// The signer is not bound to a bucket, so unlike liveS3Store this hands back
// both — which is the whole point of the type and worth exercising against a
// server rather than only in the presigner's arithmetic.
func liveS3Signer(t *testing.T) (*S3Signer, string) {
	t.Helper()

	endpoint := os.Getenv(s3EndpointEnv)
	if endpoint == "" {
		t.Skipf("%s not set — skipping live object-store test (see file docs)", s3EndpointEnv)
	}
	access := envOr(s3AccessEnv, "toft")
	secret := envOr(s3SecretEnv, "toft-dev-secret")

	bucket := "toft-test-signer-" + strings.ToLower(strings.NewReplacer("_", "-", "/", "-").Replace(t.Name()))
	if len(bucket) > 60 {
		bucket = bucket[:60]
	}

	ctx := context.Background()
	client := rawS3Client(t, endpoint, access, secret)
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		if !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") &&
			!strings.Contains(err.Error(), "BucketAlreadyExists") {
			t.Fatalf("creating test bucket %s at %s: %v", bucket, endpoint, err)
		}
	}
	t.Cleanup(func() { emptyAndRemoveBucket(client, bucket) })

	t.Setenv("AWS_ACCESS_KEY_ID", access)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))

	signer, err := NewS3Signer(ctx, S3SignerConfig{
		Endpoint: endpoint, Region: "us-east-1", UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Signer: %v", err)
	}
	return signer, bucket
}

// putSigned performs a presigned PUT exactly as a client would: the returned
// headers, verbatim, and nothing else.
func putSigned(t *testing.T, req *SignedRequest, body []byte) (int, string) {
	t.Helper()
	hr, err := http.NewRequest(req.Method, req.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	for name, value := range req.Headers {
		if strings.EqualFold(name, "Host") {
			continue // net/http sets it from the URL, and setting it twice is a 400.
		}
		hr.Header.Set(name, value)
	}
	hr.ContentLength = int64(len(body))

	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatalf("presigned %s: %v", req.Method, err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(msg)
}

// The integrity check has to be enforced by the *server*, or it is decoration.
//
// This test exists because the first implementation was decoration and nobody
// would have noticed: the SDK hoists `x-amz-checksum-sha256` into the query
// string, where it is covered by the signature and read by no server, and a
// tampered body uploaded successfully. Content-MD5 is not an x-amz- header, so
// it survives as a signed header and is refused. Both are sent; this is the one
// that a store on any implementation will reject.
func TestSignedChecksumIsEnforced(t *testing.T) {
	signer, bucket := liveS3Signer(t)
	ctx := context.Background()

	body := []byte("the bytes the client declared")
	sha := sha256.Sum256(body)
	md5sum := md5.Sum(body)
	cond := PutConditions{
		ChecksumSHA256: base64.StdEncoding.EncodeToString(sha[:]),
		ContentMD5:     base64.StdEncoding.EncodeToString(md5sum[:]),
		ContentLength:  int64(len(body)),
	}

	req, err := signer.PresignPut(ctx, bucket, "grants/ok", time.Minute, cond)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if code, msg := putSigned(t, req, body); code != http.StatusOK {
		t.Fatalf("the declared body was refused: %d %s", code, msg)
	}

	// The same URL, a different body of the same length. The signature still
	// verifies — it covers the request, not the bytes — so the digest is the
	// only thing standing between a corrupted upload and a stored object.
	tampered := []byte("the bytes somebody-else sent!")
	if len(tampered) != len(body) {
		t.Fatalf("the tampered body must be the same length, or Content-Length refuses it first")
	}
	req, err = signer.PresignPut(ctx, bucket, "grants/tampered", time.Minute, cond)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if code, _ := putSigned(t, req, tampered); code == http.StatusOK {
		t.Error("a body that does not match the signed digest was stored; the integrity check is decoration")
	}
}

// `write` means new objects only, on the wire as at the mount.
//
// `If-None-Match: *` is an S3 conditional write and it is what makes the rung
// mean the same thing in both expressions of the ladder. MinIO honours it,
// which is what lets a developer exercise the rung without an AWS account.
func TestWriteRungRefusesAnOverwrite(t *testing.T) {
	signer, bucket := liveS3Signer(t)
	ctx := context.Background()

	body := []byte("first")
	sha := sha256.Sum256(body)
	md5sum := md5.Sum(body)
	base := PutConditions{
		ChecksumSHA256: base64.StdEncoding.EncodeToString(sha[:]),
		ContentMD5:     base64.StdEncoding.EncodeToString(md5sum[:]),
		ContentLength:  int64(len(body)),
	}

	// `overwrite` writes it the first time.
	req, err := signer.PresignPut(ctx, bucket, "grants/once", time.Minute, base)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if code, msg := putSigned(t, req, body); code != http.StatusOK {
		t.Fatalf("seeding: %d %s", code, msg)
	}

	// `write` refuses the second.
	conditional := base
	conditional.IfNoneMatch = true
	req, err = signer.PresignPut(ctx, bucket, "grants/once", time.Minute, conditional)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	code, _ := putSigned(t, req, body)
	if code != http.StatusPreconditionFailed {
		t.Errorf("overwriting under a `write` grant returned %d, want 412 — the rung is not holding", code)
	}

	// And `overwrite` still may, which is what makes the two rungs different
	// rather than one rung with a flag nobody sets.
	req, err = signer.PresignPut(ctx, bucket, "grants/once", time.Minute, base)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if code, msg := putSigned(t, req, body); code != http.StatusOK {
		t.Errorf("an unconditional PUT was refused: %d %s", code, msg)
	}
}
