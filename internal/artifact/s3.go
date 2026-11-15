package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Store keeps artifacts in an S3-compatible object store. This is what makes
// releases durable under containers: a task replacement destroys the apps
// directory, and the reconciler restores every release from here on the next
// boot.
//
// "S3-compatible" is deliberate. The buyers who require running inside their
// own account are the same ones who may not be on AWS at all, and the object
// interface is the one piece of this design with an on-premises
// implementation (MinIO, Ceph). Endpoint is what points it at one.
type S3Store struct {
	client *s3.Client
	bucket string
	// prefix namespaces this install's artifacts inside a shared bucket.
	prefix string
	// staging is where a release is packed before its upload; "" is TMPDIR.
	staging string
}

// S3Config is the operator-supplied half. Region and credentials come from the
// ambient AWS configuration — environment, instance role, or task role — so
// that the common case on ECS needs no secrets in Toft's own settings.
type S3Config struct {
	Bucket string
	Prefix string
	// Endpoint overrides the AWS endpoint for an S3-compatible store. Empty
	// means real S3.
	Endpoint string
	Region   string
	// UsePathStyle is required by most self-hosted S3-compatible servers,
	// which cannot do virtual-host addressing.
	UsePathStyle bool
	// StagingDir is where a release is packed before its upload — the disk
	// the release trees are on, not a TMPDIR that is a small tmpfs on some
	// hosts. Empty means TMPDIR.
	StagingDir string
}

func NewS3Store(ctx context.Context, cfg S3Config) (*S3Store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("artifact: S3 store needs a bucket")
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("%w: loading AWS configuration: %v", ErrUnreachable, err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})

	prefix := cfg.Prefix
	if prefix != "" && prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}
	return &S3Store{client: client, bucket: cfg.Bucket, prefix: prefix, staging: cfg.StagingDir}, nil
}

func (s *S3Store) objectKey(key string) string { return s.prefix + key }

// digestMetaKey carries the artifact digest as user metadata. The ETag is not
// usable for this: for a multipart upload it is a digest of digests, not of
// the object, so a caller that trusted it would compare unequal things.
const digestMetaKey = "toft-sha256"

func (s *S3Store) Put(ctx context.Context, key, dir string) (Descriptor, error) {
	return putStaged(ctx, s, key, dir)
}

// Stage packs to a local temp file first. The upload needs a seekable body to
// sign and retry, and the digest must be known before the object is written so
// it can be stored as metadata alongside it.
func (s *S3Store) Stage(ctx context.Context, dir string) (*Staged, error) {
	return StageTo(ctx, s.staging, dir)
}

func (s *S3Store) Upload(ctx context.Context, key string, st *Staged) (Descriptor, error) {
	body, err := st.body()
	if err != nil {
		return Descriptor{}, err
	}
	if st.Size > s3MultipartThreshold {
		err = s.uploadMultipart(ctx, key, body, st)
	} else {
		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(s.objectKey(key)),
			Body:          body,
			ContentLength: aws.Int64(st.Size),
			Metadata:      map[string]string{digestMetaKey: st.SHA256},
		})
	}
	if err != nil {
		return Descriptor{}, mapS3Error(err, key)
	}

	return Descriptor{
		Key:      key,
		Size:     st.Size,
		SHA256:   st.SHA256,
		StoredAt: time.Now().UTC(),
	}, nil
}

// A single PutObject is capped at 5 GiB by S3, and a release may unpack to
// 8 GiB, so a bigger packed release goes up in parts (AS-14). Variables only
// so a test can lower them; nothing else writes them. 64 MiB parts reach 640
// GiB within S3's 10,000-part ceiling, far past anything pack will produce.
var (
	s3MultipartThreshold int64 = 1 << 30
	s3PartSize           int64 = 64 << 20
)

// uploadMultipart writes one staged release as a multipart upload, carrying
// the digest as metadata on the object the completion creates, exactly as the
// single PUT does. Parts are read as sections of the staged file, so each is
// seekable for the SDK's retries, and a failure aborts the upload so no
// orphaned parts are billed.
func (s *S3Store) uploadMultipart(ctx context.Context, key string, file *os.File, st *Staged) error {
	objKey := s.objectKey(key)
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(objKey),
		Metadata: map[string]string{digestMetaKey: st.SHA256},
	})
	if err != nil {
		return err
	}
	abort := func() {
		// On its own context: the upload's may be the reason it failed.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_, _ = s.client.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: aws.String(objKey), UploadId: created.UploadId,
		})
	}

	var parts []types.CompletedPart
	for off, n := int64(0), int32(1); off < st.Size; off, n = off+s3PartSize, n+1 {
		size := min(s3PartSize, st.Size-off)
		out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(objKey),
			UploadId:      created.UploadId,
			PartNumber:    aws.Int32(n),
			Body:          io.NewSectionReader(file, off, size),
			ContentLength: aws.Int64(size),
		})
		if err != nil {
			abort()
			return err
		}
		parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(n)})
	}
	if _, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(objKey),
		UploadId:        created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		abort()
		return err
	}
	return nil
}

func (s *S3Store) Get(ctx context.Context, key, dest string) error {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		return mapS3Error(err, key)
	}
	defer out.Body.Close()

	desc := Descriptor{Key: key, SHA256: out.Metadata[digestMetaKey]}
	if out.ContentLength != nil {
		desc.Size = *out.ContentLength
	}
	return verifyAndUnpack(out.Body, desc, dest)
}

func (s *S3Store) Stat(ctx context.Context, key string) (Descriptor, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		return Descriptor{}, mapS3Error(err, key)
	}

	desc := Descriptor{Key: key, SHA256: out.Metadata[digestMetaKey]}
	if out.ContentLength != nil {
		desc.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		desc.StoredAt = out.LastModified.UTC()
	}
	return desc, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		// S3 delete is already idempotent; a not-found here is a bucket or
		// permission problem, not a missing key.
		return mapS3Error(err, key)
	}
	return nil
}

func (s *S3Store) Expire(ctx context.Context, appID string, keep []string) (int, error) {
	kept := make(map[string]bool, len(keep))
	for _, k := range keep {
		kept[k] = true
	}

	prefix := s.objectKey(releasePrefix(appID))
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	removed := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return removed, mapS3Error(err, prefix)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			key := (*obj.Key)[len(s.prefix):]
			if kept[key] {
				continue
			}
			if err := s.Delete(ctx, key); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// mapS3Error collapses the SDK's error surface onto the three failure modes a
// caller must distinguish. Getting this wrong in the other direction is what
// matters: a transient outage reported as ErrNotFound would have the
// reconciler conclude every release is gone and give up on the whole install.
func mapS3Error(err error, key string) error {
	if err == nil {
		return nil
	}

	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	// HeadObject reports a missing key as a bare 404 with no typed error.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return fmt.Errorf("%w: %s", ErrNotFound, key)
		}
	}
	return fmt.Errorf("%w: %s: %v", ErrUnreachable, key, err)
}
