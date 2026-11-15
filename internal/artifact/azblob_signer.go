package artifact

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
)

// AzBlobSigner is Signer against Azure Blob Storage.
//
// **The ladder is re-expressed as SAS permissions, not translated into
// headers**, and this is the decision the Azure store half turns on.
//
// An S3 presigned PUT carries `If-None-Match: *` *inside* the signature, so
// `write` means "new objects only" whatever the client sends. A SAS signs the
// resource, the permissions, the window, the protocol and the response-header
// overrides — and **not** the request's conditional headers. Emitting the
// condition as a header the client is asked to send would be a permission the
// client could decline, which is not a permission. So the rung comes from the
// permission set instead:
//
//	read  | GET     | r
//	write | PUT     | c        — create, which fails on a blob that exists
//	full  | PUT     | c + w
//	full  | DELETE  | d
//
// Each URL is minted for one verb, so it carries the least the verb needs at
// that rung rather than the whole cumulative row. `c` without `w` is what makes
// `write` mean at the URL what it means at the mount: Put Blob against an
// existing blob needs `w`, so a create-only SAS is refused by the service and
// not by a client's good manners.
//
// **One guarantee weakens, and it is stated rather than designed around.**
// Content-MD5 is still sent and Azure still refuses a body that does not match
// it, but the header is not covered by the SAS, so a client may omit it — and
// `x-amz-checksum-sha256` has no Azure equivalent at all. The platform keeps
// computing and sending both; on this backend the digest is the *server's*
// check on a body the client volunteered, not a condition of the URL. What the
// design actually rests on is untouched: one object, one verb, one window.
type AzBlobSigner struct {
	svc *service.Client
	// endpoint is the service URL without a trailing slash — the base every
	// signed URL is built on, so "which URL is this account" is answered once.
	endpoint string
	// https is false only for a local emulator over plain HTTP. It decides the
	// SAS `spr` field, which is signed: a URL restricted to HTTPS cannot be
	// replayed over a plain connection somebody downgraded.
	https bool
	// sharedKey is set when the operator gave an account key — Azurite, and
	// nothing else. Nil on a real install.
	sharedKey *azblob.SharedKeyCredential

	// The user delegation key, which is how a SAS is signed when the platform
	// authenticates as an identity rather than with an account key. It is
	// fetched from the service, has its own window, and is cached: minting one
	// per presign would put a round trip on every URL the store browser draws.
	mu           sync.Mutex
	delegation   *service.UserDelegationCredential
	delegationTo time.Time
}

var _ Signer = (*AzBlobSigner)(nil)

// AzBlobSignerConfig is the ambient half of AzBlobConfig — everything except
// which container, because there is no single container. A store is somebody
// else's, and an administrator names it when they create the resource.
type AzBlobSignerConfig struct {
	// Account is the storage account the stores live in.
	Account string
	// Endpoint overrides the derived service URL, for Azurite and for a
	// sovereign cloud whose suffix is not blob.core.windows.net.
	Endpoint string
	// ConnectionString is Azurite's shape and only Azurite's. See AzBlobConfig.
	ConnectionString string
}

// ConnectOrigins implements Origins: every URL it mints is on the account's
// endpoint.
func (a *AzBlobSigner) ConnectOrigins() []string {
	u, err := url.Parse(a.endpoint)
	if err != nil || u.Host == "" {
		return nil
	}
	return []string{u.Scheme + "://" + u.Host}
}

func NewAzBlobSigner(cfg AzBlobSignerConfig) (*AzBlobSigner, error) {
	svc, err := azblobService(cfg.Account, cfg.Endpoint, cfg.ConnectionString)
	if err != nil {
		return nil, err
	}
	return newAzBlobSigner(svc, cfg.ConnectionString)
}

// newAzBlobSigner builds a signer on an account client that already exists, so
// the artifact store can presign on the same client and credential it reads
// and writes with.
func newAzBlobSigner(svc *service.Client, connectionString string) (*AzBlobSigner, error) {
	var err error
	s := &AzBlobSigner{svc: svc, endpoint: strings.TrimSuffix(svc.URL(), "/")}
	s.https = strings.HasPrefix(s.endpoint, "https://")

	if connectionString != "" {
		account, key := parseAzConnectionKey(connectionString)
		if key == "" {
			return nil, fmt.Errorf("%w: the Azure connection string carries no AccountKey, "+
				"so no SAS can be signed with it", ErrUnreachable)
		}
		if s.sharedKey, err = azblob.NewSharedKeyCredential(account, key); err != nil {
			return nil, fmt.Errorf("%w: azure shared key: %v", ErrUnreachable, err)
		}
	}
	return s, nil
}

// parseAzConnectionKey reads AccountName and AccountKey out of a connection
// string. Written here rather than reached for in the SDK because the SDK's
// parser is internal, and because this is the only place a *key* is used at
// all: a real install authenticates from the ambient identity chain and signs
// with a user delegation key, which is the credential this whole design would
// rather have.
func parseAzConnectionKey(conn string) (account, key string) {
	for _, part := range strings.Split(conn, ";") {
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "accountname":
			account = strings.TrimSpace(value)
		case "accountkey":
			// Not TrimSpace-and-Cut on the first "=": a base64 key ends in
			// padding, and cutting on the first separator is what keeps it.
			key = strings.TrimSpace(value)
		}
	}
	return account, key
}

// userDelegation returns a delegation key that will still be valid for the
// whole life of a URL valid for ttl from now.
//
// **A user-delegation SAS cannot outlive the key that signed it**, so the
// reuse test is the key's remaining life against the URL about to be signed —
// never a fixed fraction of the key's window. A key reused for a fixed 4/5 of
// a 6 h window could have as little as 72 minutes left, which a 6 h job URL
// (PresignTTL) signed under it would not survive. The key is a credential the
// platform holds and refreshing it is a round trip, so it is minted for a day
// and refreshed once its remainder falls below what the URL needs.
func (a *AzBlobSigner) userDelegation(ctx context.Context, ttl time.Duration) (*service.UserDelegationCredential, error) {
	need := ttl + azClockSkew
	if need > azDelegationWindow-azClockSkew {
		return nil, fmt.Errorf("%w: a %s URL outlives the %s Azure delegation key that would sign it",
			ErrUnreachable, ttl, azDelegationWindow)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.delegation != nil && time.Now().Add(need).Before(a.delegationTo) {
		return a.delegation, nil
	}

	now := time.Now().UTC().Add(-azClockSkew)
	expiry := now.Add(azDelegationWindow)
	cred, err := a.svc.GetUserDelegationCredential(ctx, service.KeyInfo{
		Start:  to.Ptr(now.Format(sas.TimeFormat)),
		Expiry: to.Ptr(expiry.Format(sas.TimeFormat)),
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: requesting an Azure user delegation key: %v", ErrUnreachable, err)
	}
	a.delegation = cred
	// The key's own expiry, less the skew a clock ahead of Azure's would eat.
	a.delegationTo = expiry.Add(-azClockSkew)
	return cred, nil
}

// azDelegationWindow is how long a user delegation key is asked for. Azure
// allows up to seven days; a day is long enough that the longest URL minted
// from it (PresignTTL) forces a refresh at most every eighteen hours.
const azDelegationWindow = 24 * time.Hour

// azClockSkew is how far back a SAS window starts. A URL minted on a host whose
// clock is a minute ahead of Azure's would otherwise be refused as not yet
// valid, which reads to the person holding it as a permission problem.
const azClockSkew = 5 * time.Minute

// sign mints one URL: one blob, one permission set, one window.
func (a *AzBlobSigner) sign(ctx context.Context, containerName, key string,
	perms sas.BlobPermissions, ttl time.Duration, disposition string) (string, error) {

	protocol := sas.ProtocolHTTPS
	if !a.https {
		// Only reachable against a local emulator. A SAS whose `spr` allowed
		// plain HTTP on a real account would be a URL somebody could replay off
		// the wire, so this follows the endpoint rather than being configurable.
		protocol = sas.ProtocolHTTPSandHTTP
	}

	now := time.Now().UTC()
	values := sas.BlobSignatureValues{
		Protocol:      protocol,
		StartTime:     now.Add(-azClockSkew),
		ExpiryTime:    now.Add(ttl),
		Permissions:   perms.String(),
		ContainerName: containerName,
		BlobName:      key,
		// rscd is a *signed* field of a SAS, which is what makes it safe to
		// derive from a request at all — the same guarantee
		// response-content-disposition gets under SigV4. A caller cannot append
		// one to a URL the platform already minted.
		ContentDisposition: disposition,
	}

	var (
		qp  sas.QueryParameters
		err error
	)
	if a.sharedKey != nil {
		qp, err = values.SignWithSharedKey(a.sharedKey)
	} else {
		var cred *service.UserDelegationCredential
		if cred, err = a.userDelegation(ctx, ttl); err == nil {
			qp, err = values.SignWithUserDelegation(cred)
		}
	}
	if err != nil {
		return "", fmt.Errorf("%w: signing %s: %v", ErrUnreachable, key, err)
	}

	u, err := url.Parse(a.endpoint)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrUnreachable, a.endpoint, err)
	}
	// Assigning Path (not RawPath) leaves escaping to net/url, which keeps the
	// separators in a key like "reports/2026/q1.csv" separators and escapes
	// everything else.
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + containerName + "/" + key
	u.RawQuery = qp.Encode()
	return u.String(), nil
}

func (a *AzBlobSigner) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration, attachAs string) (*SignedRequest, error) {
	disposition := ""
	if attachAs != "" {
		disposition = contentDisposition(attachAs)
	}
	signed, err := a.sign(ctx, bucket, key, sas.BlobPermissions{Read: true}, ttl, disposition)
	if err != nil {
		return nil, err
	}
	return &SignedRequest{URL: signed, Method: "GET", Headers: map[string]string{}}, nil
}

func (a *AzBlobSigner) PresignPut(ctx context.Context, bucket, key string, ttl time.Duration, cond PutConditions) (*SignedRequest, error) {
	// The `write` rung is Create without Write. Put Blob against a blob that
	// already exists needs Write, so a create-only SAS is refused by the
	// *service* — which is what makes this rung mean the same thing the signed
	// If-None-Match means on S3, rather than a request the client may decline.
	perms := sas.BlobPermissions{Create: true}
	if !cond.IfNoneMatch {
		perms.Write = true
	}
	signed, err := a.sign(ctx, bucket, key, perms, ttl, "")
	if err != nil {
		return nil, err
	}

	headers := map[string]string{
		// Required by the Put Blob REST API, and the reason this is not an
		// optional header the client may reason about.
		"x-ms-blob-type": "BlockBlob",
	}
	// Not signed, and the documentation says so: Azure still refuses a body
	// whose digest does not match this header, but the SAS does not cover it,
	// so a client may leave it off and upload unverified. The platform sends it
	// because the check is worth having when it is honoured, not because the
	// URL enforces it. x-amz-checksum-sha256 has no equivalent here and is
	// deliberately not invented as a metadata header nothing would read.
	if cond.ContentMD5 != "" {
		headers["Content-MD5"] = cond.ContentMD5
	}
	signedPut := &SignedRequest{URL: signed, Method: "PUT", Headers: headers}
	if cond.IfNoneMatch {
		// A create-only SAS against an existing blob is refused by the service
		// with 403, not 412 (I-5).
		signedPut.ExistsStatus = http.StatusForbidden
	}
	return signedPut, nil
}

func (a *AzBlobSigner) PresignDelete(ctx context.Context, bucket, key string, ttl time.Duration) (*SignedRequest, error) {
	signed, err := a.sign(ctx, bucket, key, sas.BlobPermissions{Delete: true}, ttl, "")
	if err != nil {
		return nil, err
	}
	return &SignedRequest{URL: signed, Method: "DELETE", Headers: map[string]string{}}, nil
}

func (a *AzBlobSigner) ListObjects(ctx context.Context, bucket, prefix, cursor, delimiter string, limit int32) (*ObjectPage, error) {
	cont := a.svc.NewContainerClient(bucket)
	page := &ObjectPage{Objects: []StoreObject{}}

	// Two pagers, because Azure splits what S3 folds into one call: a flat
	// listing has no notion of a folder, and the hierarchy listing is a
	// different operation. The delimiter picks which, exactly as it does on S3.
	if delimiter != "" {
		pager := cont.NewListBlobsHierarchyPager(delimiter, &container.ListBlobsHierarchyOptions{
			Prefix:     &prefix,
			Marker:     azMarker(cursor),
			MaxResults: &limit,
		})
		if !pager.More() {
			return page, nil
		}
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapAzError(err, prefix)
		}
		for _, item := range resp.Segment.BlobItems {
			if name, ok := relativeName(prefix, azName(item.Name)); ok {
				page.Objects = append(page.Objects, azStoreObject(name, item.Properties))
			}
		}
		for _, cp := range resp.Segment.BlobPrefixes {
			if name, ok := relativeName(prefix, azName(cp.Name)); ok {
				page.Prefixes = append(page.Prefixes, name)
			}
		}
		page.NextCursor = azNextMarker(resp.NextMarker)
		return page, nil
	}

	pager := cont.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:     &prefix,
		Marker:     azMarker(cursor),
		MaxResults: &limit,
	})
	if !pager.More() {
		return page, nil
	}
	resp, err := pager.NextPage(ctx)
	if err != nil {
		return nil, mapAzError(err, prefix)
	}
	for _, item := range resp.Segment.BlobItems {
		if name, ok := relativeName(prefix, azName(item.Name)); ok {
			page.Objects = append(page.Objects, azStoreObject(name, item.Properties))
		}
	}
	page.NextCursor = azNextMarker(resp.NextMarker)
	return page, nil
}

func azStoreObject(name string, props *container.BlobProperties) StoreObject {
	obj := StoreObject{Name: name}
	if props == nil {
		return obj
	}
	if props.ContentLength != nil {
		obj.Size = *props.ContentLength
	}
	if props.LastModified != nil {
		obj.LastModified = props.LastModified.UTC()
	}
	if props.ETag != nil {
		obj.ETag = string(*props.ETag)
	}
	return obj
}

func azName(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func azMarker(cursor string) *string {
	if cursor == "" {
		return nil
	}
	return &cursor
}

// azNextMarker is empty when this is the last page. Azure sends an empty marker
// rather than omitting it, so the emptiness is the signal and a non-nil pointer
// to "" is not a cursor.
func azNextMarker(marker *string) string {
	if marker == nil {
		return ""
	}
	return *marker
}

func (a *AzBlobSigner) HasObject(ctx context.Context, bucket, key string) (bool, error) {
	_, err := a.svc.NewContainerClient(bucket).NewBlobClient(key).GetProperties(ctx, nil)
	if err == nil {
		return true, nil
	}
	if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound, bloberror.ResourceNotFound) {
		return false, nil
	}
	return false, mapAzError(err, key)
}
