package artifact

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Signer mints a URL a person may hold: one object, one verb, one window.
//
// Every other presign on this platform addresses the artifact store, which is
// one bucket from one configuration. A store is somebody else's bucket — an
// administrator names it when they create the resource — so the container is an
// argument here rather than a field.
//
// It is an interface because the platform signs against two object stores and
// they do not agree on how a condition travels. S3 signs request headers, so
// the `write` rung can be an `If-None-Match: *` the client cannot decline; an
// Azure SAS signs the permission set instead, and `create` is a permission
// rather than a header. The four methods below are the whole of what the API
// package needs, and they are deliberately the *outcome* — a URL and the
// headers that go with it — rather than anything either backend has to explain.
//
// **Absent, not dormant.** The server holds nil when the install has no
// object-store configuration, and the routes that need it 404 exactly as the
// admin storage routes already 404 when there is no storage backend at all.
// The nil is the interface's own — a typed nil pointer stored here would read
// as present and fail at the first call, which is why main.go declares its
// variable as this type and not as a concrete one.
// Origins is what a browser must be allowed to connect to for the URLs a
// signer mints: the platform's Content-Security-Policy adds them to
// connect-src, or every upload from the store browser is refused by the
// install's own page before a byte leaves it (docs/bugs 2026-10-22-e2e-storage
// #1). CSP host sources, so a wildcard on the leftmost label is allowed.
//
// A separate interface rather than a Signer method, for the reason
// internal/api gives about capability interfaces: a fake that lacks it simply
// adds nothing to the policy.
type Origins interface {
	ConnectOrigins() []string
}

type Signer interface {
	// PresignGet mints a download. attachAs, when it is not empty, is the
	// filename the object is saved under, and it is *signed* on both backends
	// — see the implementations for which field carries it.
	PresignGet(ctx context.Context, bucket, key string, ttl time.Duration, attachAs string) (*SignedRequest, error)

	// PresignPut mints an upload under the conditions the grant decided.
	PresignPut(ctx context.Context, bucket, key string, ttl time.Duration, cond PutConditions) (*SignedRequest, error)

	// PresignDelete mints a delete of one object.
	PresignDelete(ctx context.Context, bucket, key string, ttl time.Duration) (*SignedRequest, error)

	// ListObjects is the one operation here that is not a signature.
	//
	// A listing cannot be presigned into something a client may safely hold:
	// the URL that lists a prefix is the URL that lists the container with a
	// different query string, and handing that out would put the *container's*
	// contents one edit away from a caller granted one prefix. So the platform
	// lists, and returns names.
	//
	// **The delimiter is what makes a listing a screen.** Empty lists
	// everything beneath the prefix, which is what a CLI wants. "/" lists one
	// level of it and returns the folders beside the objects, which is what a
	// browser needs: a store with a hundred thousand keys under one prefix has
	// no honest flat page, because the thousand a page cap returns are an
	// arbitrary thousand and a screen showing them calls itself the contents.
	ListObjects(ctx context.Context, bucket, prefix, cursor, delimiter string, limit int32) (*ObjectPage, error)

	// HasObject is true when one object exists at key. An error means the
	// store could not say, which is not the same as "no".
	//
	// The one reader is a storage link being opened (/files/): a mint is
	// arithmetic and succeeds for a key nothing was written to, and a link
	// to a missing file must answer exactly what a link to an unreadable one
	// does — the not-found card — rather than redirect into the store's own
	// error page, which would tell the two apart.
	HasObject(ctx context.Context, bucket, key string) (bool, error)
}

// SignedRequest is one URL, one verb, and the headers the client must send.
//
// On S3 the headers are not advice: they are covered by the signature, so a
// client that drops one or adds one gets a 403 rather than a different outcome
// — which is exactly what makes a signed condition a condition rather than a
// request. An Azure SAS does not sign request headers, so a header emitted
// beside an `azblob` URL is a check the *server* still performs on a body the
// client volunteered. See AzBlobSigner for which guarantee that costs.
type SignedRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	// ExistsStatus is the HTTP status the store answers a create-only PUT of
	// a name that already exists: 412 where If-None-Match is signed (S3, the
	// host store), 403 where the rung is a create-only SAS the service refuses
	// (Azure). The SDKs read it to raise `already_exists` the same way on every
	// backend; before it, Azure's refusal surfaced as store_refused (code review
	// 2026-11-11, I-5). Zero on a request that is not create-only.
	ExistsStatus int `json:"exists_status,omitempty"`
}

// PutConditions are the parts of a PUT that the *grant* decides, plus the
// integrity check the client supplies.
type PutConditions struct {
	// IfNoneMatch expresses the ladder's `write` rung: new objects only.
	//
	// On S3 it is signed into the URL, so a client that drops the header
	// invalidates the signature rather than escaping the condition. On Azure it
	// is the SAS `c` permission without `w`, which the service enforces on the
	// grant rather than on a header — the same rung, expressed where that
	// backend can make it stick.
	IfNoneMatch bool
	// Two digests of the same bytes, because one of them is not enough and the
	// difference was measured rather than assumed.
	//
	// ChecksumSHA256 is base64 SHA-256. It is the checksum AWS S3 stores
	// alongside the object, so an operator can re-verify the object later —
	// worth having, and the reason it is here. But the SigV4 *query* presigner
	// hoists every `x-amz-*` header into the query string, wherever it was set
	// from, and a server that reads its checksum from the header does not see
	// one: MinIO accepts a body that does not match. On real S3 it is honoured.
	// Azure Blob has no equivalent at all and ignores it.
	//
	// ContentMD5 is the half that is enforceable everywhere. `Content-MD5` is
	// not an `x-amz-` header, so on S3 it stays a signed *header* — and every
	// S3-compatible server refuses a body whose digest does not match it. Azure
	// refuses the mismatch too, but does not cover the header with the SAS, so
	// there it is the server's check on a digest the client chose to send.
	// TestSignedChecksumIsEnforced is the standing version of that measurement.
	//
	// So: SHA-256 for durable verification where the store supports it, MD5 for
	// the rejection that has to happen on every store. The client computes both
	// from one pass over the same bytes.
	ChecksumSHA256 string
	ContentMD5     string
	// ContentLength is signed so the URL cannot be used to upload something
	// other than the object whose size was declared and checked against the
	// single-PUT ceiling.
	ContentLength int64
}

// StoreObject is one object under a store's prefix, named as the caller named
// it — relative to the prefix, because the prefix is not the caller's business.
type StoreObject struct {
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
	ETag         string    `json:"etag,omitempty"`
}

// ObjectPage is one page of a listing, plus the cursor for the next.
type ObjectPage struct {
	Objects []StoreObject `json:"objects"`
	// NextCursor is empty when this is the last page. It is the backend's own
	// continuation token passed through rather than an offset of our own: an
	// offset over a prefix that is being written to skips and repeats objects,
	// and a listing that quietly lies is worse than one that costs a round trip.
	NextCursor string `json:"next_cursor,omitempty"`
	// Prefixes are the common prefixes one level below the one listed — the
	// folders, to a reader who thinks in folders — named relative to the listed
	// prefix and with their trailing slash kept, because that slash is the only
	// thing distinguishing "reports/" the folder from "reports" the object.
	//
	// Empty whenever no delimiter was passed, which is a flat listing and has
	// no levels to have a folder in.
	Prefixes []string `json:"prefixes,omitempty"`
}

// contentDisposition writes the header value for a download.
//
// Both spellings, because they fail in different directions: `filename` is what
// every browser has always read and cannot carry a non-ASCII character, and
// RFC 5987's `filename*` carries anything and is ignored by nothing current.
// The plain form is stripped down to ASCII rather than escaped, so a name that
// is entirely Japanese still leaves a usable fallback rather than an empty one.
//
// Shared by both backends: S3 signs it as `response-content-disposition` and a
// service SAS signs it as `rscd`, and the value itself is the same string.
func contentDisposition(name string) string {
	var ascii strings.Builder
	for _, r := range name {
		switch {
		case r == '"' || r == '\\' || r < 0x20 || r > 0x7e:
			ascii.WriteByte('_')
		default:
			ascii.WriteRune(r)
		}
	}
	fallback := ascii.String()
	if fallback == "" {
		fallback = "download"
	}
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", fallback, url.PathEscape(name))
}

func signedFrom(url, method string, header http.Header) *SignedRequest {
	headers := map[string]string{}
	for name := range header {
		// One value per header. Every header S3 signs on these three verbs is
		// single-valued, and flattening keeps the wire contract a plain object
		// that every client library can send without ceremony.
		headers[name] = header.Get(name)
	}
	return &SignedRequest{URL: url, Method: method, Headers: headers}
}

// relativeName strips the listed prefix off a key, and reports whether anything
// is left. Nothing is left for the prefix marker itself, which some tools
// create as a zero-byte object: it is not something anybody uploaded and
// listing it only prompts the question.
func relativeName(prefix, full string) (string, bool) {
	name := full
	if len(prefix) > 0 && len(full) >= len(prefix) {
		name = full[len(prefix):]
	}
	return name, name != ""
}
