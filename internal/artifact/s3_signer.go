package artifact

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Signer is Signer against S3 and everything that speaks it — real AWS,
// MinIO, Ceph. It is S3Store's mechanics without S3Store's bucket.
//
// It lives in this package because this package already owns request signing,
// the endpoint override and the path-style flag. A second copy of SigV4
// configuration somewhere else would be a second place for a region to be
// wrong, and a region that is wrong produces a signature that verifies nowhere
// with an error that names neither.
type S3Signer struct {
	client *s3.Client
	// origins are the hosts its URLs name, for the browser's connect-src.
	origins []string
}

// S3Signer is one implementation of Signer and the compiler is told so here, so
// that a method whose shape drifts fails at the definition rather than at the
// call site in internal/api.
var _ Signer = (*S3Signer)(nil)

// S3SignerConfig is the ambient half of S3Config — everything except which
// bucket, because there is no single bucket. Credentials and region come from
// the ambient AWS configuration, the same as the artifact store's.
type S3SignerConfig struct {
	// Endpoint overrides the AWS endpoint for an S3-compatible store. Empty
	// means real S3. Stores cannot yet be mounted from a non-AWS endpoint —
	// StoreMountOptions emits no endpoint option — so setting this today makes
	// a store that can be uploaded to and not mounted. It is honoured here so
	// that the mount is the only thing left to do.
	Endpoint string
	Region   string
	// UsePathStyle is required by most self-hosted S3-compatible servers,
	// which cannot do virtual-host addressing.
	UsePathStyle bool
}

func NewS3Signer(ctx context.Context, cfg S3SignerConfig) (*S3Signer, error) {
	loadOpts := []func(*awsconfig.LoadOptions) error{
		// Never add a checksum the caller did not ask for.
		//
		// The SDK's default is to compute a CRC32 for every PutObject, which for
		// a presigned URL means signing something the client has no way to know
		// it must send — the upload then fails with a signature mismatch that
		// looks like a clock problem. Every PUT this package signs carries the
		// caller's own digests instead, computed from the bytes it is about to
		// send. See PutConditions for which one is enforced where.
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
	}
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
	return &S3Signer{client: client, origins: s3Origins(cfg, awsCfg.Region)}, nil
}

// ConnectOrigins implements Origins.
func (b *S3Signer) ConnectOrigins() []string { return b.origins }

// s3Origins is where a signed URL can point: real S3 addresses a bucket as a
// virtual host of the regional endpoint (bucket.s3.<region>.amazonaws.com), or
// as a path under it for a bucket whose name holds a dot; a custom endpoint is
// itself, and its subdomains unless it is path-style.
func s3Origins(cfg S3SignerConfig, region string) []string {
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || u.Host == "" {
			return nil
		}
		base := u.Scheme + "://" + u.Host
		if cfg.UsePathStyle {
			return []string{base}
		}
		return []string{base, u.Scheme + "://*." + u.Host}
	}
	if region == "" {
		region = "us-east-1"
	}
	host := "s3." + region + ".amazonaws.com"
	return []string{"https://" + host, "https://*." + host,
		"https://s3.amazonaws.com", "https://*.s3.amazonaws.com"}
}

// PresignGet signs `response-content-disposition` rather than trusting the
// client to ask for it, which is what stops a browser rendering a CSV inline or
// saving the last path segment of a signed URL under a name full of query
// string. Being *signed* is what makes it safe to derive from a request at all
// — a caller cannot append one to a URL the platform already minted.
func (b *S3Signer) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration, attachAs string) (*SignedRequest, error) {
	in := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	if attachAs != "" {
		in.ResponseContentDisposition = aws.String(contentDisposition(attachAs))
	}
	ps := s3.NewPresignClient(b.client, func(o *s3.PresignOptions) { o.Expires = ttl })
	req, err := ps.PresignGetObject(ctx, in)
	if err != nil {
		return nil, mapS3Error(err, key)
	}
	return signedFrom(req.URL, req.Method, req.SignedHeader), nil
}

func (b *S3Signer) PresignPut(ctx context.Context, bucket, key string, ttl time.Duration, cond PutConditions) (*SignedRequest, error) {
	in := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	if cond.IfNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}
	if cond.ChecksumSHA256 != "" {
		in.ChecksumSHA256 = aws.String(cond.ChecksumSHA256)
	}
	if cond.ContentMD5 != "" {
		in.ContentMD5 = aws.String(cond.ContentMD5)
	}
	if cond.ContentLength > 0 {
		in.ContentLength = aws.Int64(cond.ContentLength)
	}

	ps := s3.NewPresignClient(b.client, func(o *s3.PresignOptions) { o.Expires = ttl })
	req, err := ps.PresignPutObject(ctx, in)
	if err != nil {
		return nil, mapS3Error(err, key)
	}
	signed := signedFrom(req.URL, req.Method, req.SignedHeader)
	if cond.IfNoneMatch {
		signed.ExistsStatus = http.StatusPreconditionFailed
	}
	return signed, nil
}

func (b *S3Signer) PresignDelete(ctx context.Context, bucket, key string, ttl time.Duration) (*SignedRequest, error) {
	ps := s3.NewPresignClient(b.client, func(o *s3.PresignOptions) { o.Expires = ttl })
	req, err := ps.PresignDeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, mapS3Error(err, key)
	}
	return signedFrom(req.URL, req.Method, req.SignedHeader), nil
}

func (b *S3Signer) ListObjects(ctx context.Context, bucket, prefix, cursor, delimiter string, limit int32) (*ObjectPage, error) {
	in := &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(limit),
	}
	if delimiter != "" {
		in.Delimiter = aws.String(delimiter)
	}
	if cursor != "" {
		in.ContinuationToken = aws.String(cursor)
	}
	out, err := b.client.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, mapS3Error(err, prefix)
	}

	page := &ObjectPage{Objects: []StoreObject{}}
	for _, obj := range out.Contents {
		name, ok := relativeName(prefix, aws.ToString(obj.Key))
		if !ok {
			continue
		}
		page.Objects = append(page.Objects, StoreObject{
			Name:         name,
			Size:         aws.ToInt64(obj.Size),
			LastModified: aws.ToTime(obj.LastModified),
			ETag:         aws.ToString(obj.ETag),
		})
	}
	for _, cp := range out.CommonPrefixes {
		name, ok := relativeName(prefix, aws.ToString(cp.Prefix))
		if !ok {
			continue
		}
		page.Prefixes = append(page.Prefixes, name)
	}
	if aws.ToBool(out.IsTruncated) {
		page.NextCursor = aws.ToString(out.NextContinuationToken)
	}
	return page, nil
}

func (b *S3Signer) HasObject(ctx context.Context, bucket, key string) (bool, error) {
	_, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	if mapped := mapS3Error(err, key); errors.Is(mapped, ErrNotFound) {
		return false, nil
	} else {
		return false, mapped
	}
}
