package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// A workload must never hold an object-store credential.
//
// This is the whole reason presigning exists here. A job legitimately needs to
// read its release and its inputs and to write its outputs — but a credential
// that can do those things can, in every object store worth using, also read
// every *other* app's releases and outputs. Handing one to app code would make
// a single compromised job a reader of the entire platform's contents.
//
// So the platform signs, and the workload gets URLs: narrow to one object, one
// verb, and one short window. A leaked URL is worth exactly one object until it
// expires, which is a blast radius an operator can reason about.
//
// The same rule holds in shared mode, where a filesystem path would have worked
// and would have been easier. Outputs are readable by API or presigned URL
// only, in both modes — because if shared mode were quietly more capable, an
// app written against it could not move to isolated mode, and the mode would
// stop being a knob and become a one-way door.

// Presigner mints time-limited URLs for a single object.
type Presigner interface {
	// PresignGet returns a URL that reads one object.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	// PresignPut returns a URL that writes one object.
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// PresignerOf reports the store's presigner, seeing through the telemetry
// decorator, which embeds only Store and so would otherwise hide it — as
// UploaderOf does for Uploader. Every caller that wants a signed URL goes
// through this, never a bare type assertion on the store.
func PresignerOf(s Store) (Presigner, bool) {
	if i, ok := s.(instrumented); ok {
		s = i.Store
	}
	if s == nil {
		return nil, false
	}
	p, ok := s.(Presigner)
	return p, ok
}

// PresignGet implements Presigner for S3-compatible stores.
func (s *S3Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	ps := s3.NewPresignClient(s.client, func(o *s3.PresignOptions) { o.Expires = ttl })
	req, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		return "", mapS3Error(err, key)
	}
	return req.URL, nil
}

func (s *S3Store) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	ps := s3.NewPresignClient(s.client, func(o *s3.PresignOptions) { o.Expires = ttl })
	req, err := ps.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		return "", mapS3Error(err, key)
	}
	return req.URL, nil
}

// LocalStore cannot presign: there is no signed-URL concept for a directory,
// and inventing one would mean standing up an authenticated file endpoint.
//
// Reporting that plainly is correct rather than a gap. A local store is the VM
// install's choice, and a VM install has no isolated mode and therefore no
// workload that needs a URL — the runner reads the release from disk. Faking a
// URL here would only defer the failure to somewhere less obvious.
func (s *LocalStore) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "", fmt.Errorf("%w: a local artifact store cannot presign URLs; "+
		"isolated workloads need an object store (ARTIFACT_STORE=s3)", ErrNotFound)
}

func (s *LocalStore) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "", fmt.Errorf("%w: a local artifact store cannot presign URLs; "+
		"isolated workloads need an object store (ARTIFACT_STORE=s3)", ErrNotFound)
}

// --- Job input and output keys ---
//
// One prefix per run, so a run's URLs cannot address another run's data even
// if the key were guessable — the signature covers the exact key, and the
// layout means a mistake in key construction still lands inside the run.

// RunInputKey addresses the payload a caller supplied when enqueueing.
func RunInputKey(appID, runID string) string {
	return "runs/" + appID + "/" + runID + "/input"
}

// RunOutputKey addresses one named output a job produced. The name is the
// job's own choice, so it is the caller's responsibility to keep it inside the
// run's prefix — which safeJoin-style validation enforces before signing.
func RunOutputKey(appID, runID, name string) string {
	return "runs/" + appID + "/" + runID + "/outputs/" + name
}

// RunPrefix is everything belonging to one run, for expiry.
func RunPrefix(appID, runID string) string {
	return "runs/" + appID + "/" + runID + "/"
}

// PresignTTL is how long a job's URLs live at most.
//
// Long enough to cover most jobs' whole run, because a URL that expires
// mid-run turns a working job into a mysterious failure near the end — the
// worst possible time. Short enough that a leaked URL is not indefinitely
// useful. A run longer than this, or one whose role credential expires before
// its URL, writes its output through a URL minted at write time instead
// (OutputURLTTL, the runner's output-url route), which every SDK asks for
// first.
const PresignTTL = 6 * time.Hour

// OutputURLTTL is the life of an output URL minted at the moment a job
// writes: minutes, because the PUT follows at once, and a URL signed with a
// role's temporary credential has that credential's remaining life at most.
const OutputURLTTL = 15 * time.Minute

// runURLMargin is how far past its deadline a run's URLs stay usable: a job
// writing its output in its last second must not find the link closed.
const runURLMargin = 10 * time.Minute

// RunURLTTL is the life of one run's input and output URLs: the run's own
// timeout plus a margin, never longer than PresignTTL and never shorter than
// the margin.
//
// The URL carries a signature — and on S3 behind a role, a session token — in
// its query string, and a job that logs its environment writes it into a log
// people read. Six hours for a thirty-second job was six hours of a working
// credential sitting in that log for no reason; the run cannot use the link
// after its deadline, so nobody else should be able to either.
func RunURLTTL(timeout time.Duration) time.Duration {
	ttl := timeout + runURLMargin
	if timeout <= 0 || ttl > PresignTTL {
		return PresignTTL
	}
	return ttl
}

// signedQueryParam matches the credential-bearing parameters of an S3 (SigV4)
// or Azure (SAS) presigned URL — as query parameters, after `?` or `&`, so a
// line that merely says `sig=` is left alone — up to the next separator.
var signedQueryParam = regexp.MustCompile(
	`(?i)([?&](?:X-Amz-Signature|X-Amz-Security-Token|X-Amz-Credential|sig))=[^&\s"'<>]+`)

// MaskSignedURLs replaces the secret parts of any presigned URL in a line with
// a placeholder, keeping the rest — host, key, expiry — readable. Applied to a
// job's log on the way into the database, because a job that prints its
// environment prints TOFT_OUTPUT_URI, and the log outlives the run.
func MaskSignedURLs(line string) string {
	if !strings.Contains(line, "=") {
		return line
	}
	return signedQueryParam.ReplaceAllString(line, "$1=REDACTED")
}

// BlobWriter stores a small object directly, without packing a directory.
//
// Separate from Put because a run's input payload is not a release tree: it is
// a handful of bytes a caller supplied, and routing it through tar-and-digest
// would be machinery for nothing.
type BlobWriter interface {
	PutBlob(ctx context.Context, key string, data []byte) error
}

// BlobReader reports whether a small object is there, without transferring it.
//
// Separate from Store.Stat, which describes a *release*: it reads back a digest
// the packer wrote, and a blob carries none — the local store keeps that
// descriptor in a sidecar file PutBlob does not write, so Stat answers
// not-found for a blob that exists. This asks the only question a caller has
// about a blob it did not write itself.
//
// The caller is the job bootstrap, deciding whether to mint a presigned input
// URL. Signing is pure arithmetic and succeeds for a key nothing was ever
// written to, so without this a run started with parameters and no payload was
// handed a URL that 404s — and `if run.input_uri:`, the obvious test for "was I
// given a payload", was true either way.
type BlobReader interface {
	// HasBlob is true when the object exists. An error means the store could
	// not say, which is not the same as "no".
	HasBlob(ctx context.Context, key string) (bool, error)
}

func (s *S3Store) HasBlob(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err == nil {
		return true, nil
	}
	if mapped := mapS3Error(err, key); errors.Is(mapped, ErrNotFound) {
		return false, nil
	}
	return false, mapS3Error(err, key)
}

// HasBlob on a local store is the file being there. The sidecar Stat reads is
// deliberately not consulted: PutBlob writes bytes and no descriptor, so the
// file is the whole of what exists.
func (s *LocalStore) HasBlob(ctx context.Context, key string) (bool, error) {
	path, err := s.path(key)
	if err != nil {
		return false, err
	}
	switch _, err := os.Stat(path); {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
}

func (s *S3Store) PutBlob(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.objectKey(key)),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		return mapS3Error(err, key)
	}
	return nil
}

// PutBlob on a local store writes the bytes to a file. A VM install has no
// isolated workloads and therefore never presigns, but the local store is also
// what a developer points at, and failing here would make run payloads
// untestable without an object store.
func (s *LocalStore) PutBlob(ctx context.Context, key string, data []byte) error {
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return nil
}
