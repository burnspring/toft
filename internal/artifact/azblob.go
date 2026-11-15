package artifact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
)

// AzBlobStore keeps artifacts in Azure Blob Storage — the same job S3Store
// does, on the cloud the rest of this install may already be standing on.
//
// It is a second backend rather than a translation layer for the reason the S3
// package comment gives about MinIO and Ceph: the object interface is the one
// piece of this design with implementations that are not AWS, and pretending
// Blob is S3 through a gateway would put a credential with reach over the whole
// account in a pod Toft does not manage. The Store interface is five
// methods; two clients is cheaper than one lie.
type AzBlobStore struct {
	container *container.Client
	// prefix namespaces this install's artifacts inside a shared container.
	prefix string
	// name is the container, for error messages that say where the platform
	// was looking.
	name string
	// staging is where a release is packed before its upload; "" is TMPDIR.
	staging string
	// signer mints the SAS URLs an isolated workload fetches its release and
	// writes its output with, on the same account client as the store.
	signer *AzBlobSigner
}

var (
	_ Store     = (*AzBlobStore)(nil)
	_ Presigner = (*AzBlobStore)(nil)
)

// AzBlobConfig is the operator-supplied half. Credentials come from the ambient
// chain — workload identity in AKS, a managed identity on a VM, `az login` on a
// developer's machine — so that the common case needs no secret in Toft's
// own settings, exactly as the instance role does for S3.
type AzBlobConfig struct {
	// Account is the storage account name, e.g. "toftartifacts". The URL
	// is derived from it rather than configured, because an account name and a
	// service URL that disagree is a configuration that can be half-applied.
	Account string
	// Container holds the artifacts. Azure's word for a bucket.
	Container string
	Prefix    string
	// Endpoint overrides the derived service URL, for Azurite and for a
	// sovereign cloud whose suffix is not blob.core.windows.net. Empty means
	// the public cloud.
	Endpoint string
	// ConnectionString is Azurite's shape and only Azurite's: the emulator ships
	// one well-known account key and there is no identity chain to reach it
	// with. Never set on a real install — a real install authenticates from the
	// ambient chain, which is what keeps a storage credential out of `.env`.
	ConnectionString string
	// StagingDir is S3Config.StagingDir.
	StagingDir string
}

func NewAzBlobStore(cfg AzBlobConfig) (*AzBlobStore, error) {
	if cfg.Container == "" {
		return nil, fmt.Errorf("artifact: Azure Blob store needs a container")
	}
	svc, err := azblobService(cfg.Account, cfg.Endpoint, cfg.ConnectionString)
	if err != nil {
		return nil, err
	}

	signer, err := newAzBlobSigner(svc, cfg.ConnectionString)
	if err != nil {
		return nil, err
	}

	prefix := cfg.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &AzBlobStore{
		container: svc.NewContainerClient(cfg.Container),
		prefix:    prefix,
		name:      cfg.Container,
		staging:   cfg.StagingDir,
		signer:    signer,
	}, nil
}

// PresignGet implements Presigner: a read-only SAS on one blob.
func (s *AzBlobStore) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return s.signer.sign(ctx, s.name, s.objectKey(key), sas.BlobPermissions{Read: true}, ttl, "")
}

// PresignPut implements Presigner: create and write on one blob, so a retried
// output write may replace the first. **Put Blob needs `x-ms-blob-type:
// BlockBlob` on the request**, which a bare URL cannot carry: every SDK's
// write_output sends it, and S3 ignores it.
func (s *AzBlobStore) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return s.signer.sign(ctx, s.name, s.objectKey(key), sas.BlobPermissions{Create: true, Write: true}, ttl, "")
}

// azMaxRetries is the SDK's retry ceiling. It is a variable only so that a test
// asserting a *sustained* outage does not have to sit through the real backoff;
// nothing outside a test writes it, and an install runs on the default.
var azMaxRetries int32 = 3

func azClientOptions() *service.ClientOptions {
	opts := &service.ClientOptions{}
	opts.Retry.MaxRetries = azMaxRetries
	return opts
}

// azblobService builds the account-level client both the store and the signer
// need, so that "which URL is this account" is answered once.
func azblobService(account, endpoint, connectionString string) (*service.Client, error) {
	if connectionString != "" {
		svc, err := service.NewClientFromConnectionString(connectionString, azClientOptions())
		if err != nil {
			return nil, fmt.Errorf("%w: azure blob connection string: %v", ErrUnreachable, err)
		}
		return svc, nil
	}
	if account == "" {
		return nil, fmt.Errorf("artifact: Azure Blob needs a storage account name")
	}

	url := endpoint
	if url == "" {
		url = "https://" + account + ".blob.core.windows.net"
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("%w: loading Azure credentials: %v", ErrUnreachable, err)
	}
	svc, err := service.NewClient(url, cred, azClientOptions())
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnreachable, url, err)
	}
	return svc, nil
}

func (s *AzBlobStore) objectKey(key string) string { return s.prefix + key }

// digestMetaKey is the same name S3Store uses, and it is carried the same way:
// as user metadata rather than as the ETag, which for a block upload is a
// digest of digests and not of the object.
//
// Azure normalises metadata keys to C# identifier rules — a hyphen is not one —
// so the key is spelled with an underscore here. It is read back from the map
// the SDK returns case-insensitively, because Azure title-cases what it stores
// and a lookup that assumed the spelling back would find nothing and report a
// missing digest as a clean read.
const azDigestMetaKey = "toft_sha256"

func (s *AzBlobStore) Put(ctx context.Context, key, dir string) (Descriptor, error) {
	return putStaged(ctx, s, key, dir)
}

// Stage packs to a local temp file first, for the same two reasons S3Store
// does: the upload wants a seekable body it can retry, and the digest has to be
// known before the object is written so it can be stored beside it.
func (s *AzBlobStore) Stage(ctx context.Context, dir string) (*Staged, error) {
	return StageTo(ctx, s.staging, dir)
}

func (s *AzBlobStore) Upload(ctx context.Context, key string, st *Staged) (Descriptor, error) {
	body, err := st.body()
	if err != nil {
		return Descriptor{}, err
	}
	sum := st.SHA256
	blob := s.container.NewBlockBlobClient(s.objectKey(key))
	_, err = blob.UploadFile(ctx, body, &azblob.UploadFileOptions{
		Metadata: map[string]*string{azDigestMetaKey: &sum},
	})
	if err != nil {
		return Descriptor{}, mapAzError(err, key)
	}

	return Descriptor{
		Key:      key,
		Size:     st.Size,
		SHA256:   st.SHA256,
		StoredAt: time.Now().UTC(),
	}, nil
}

func (s *AzBlobStore) Get(ctx context.Context, key, dest string) error {
	blob := s.container.NewBlobClient(s.objectKey(key))
	out, err := blob.DownloadStream(ctx, nil)
	if err != nil {
		return mapAzError(err, key)
	}
	defer out.Body.Close()

	desc := Descriptor{Key: key, SHA256: azMeta(out.Metadata, azDigestMetaKey)}
	if out.ContentLength != nil {
		desc.Size = *out.ContentLength
	}
	return verifyAndUnpack(out.Body, desc, dest)
}

func (s *AzBlobStore) Stat(ctx context.Context, key string) (Descriptor, error) {
	blob := s.container.NewBlobClient(s.objectKey(key))
	out, err := blob.GetProperties(ctx, nil)
	if err != nil {
		return Descriptor{}, mapAzError(err, key)
	}

	desc := Descriptor{Key: key, SHA256: azMeta(out.Metadata, azDigestMetaKey)}
	if out.ContentLength != nil {
		desc.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		desc.StoredAt = out.LastModified.UTC()
	}
	return desc, nil
}

func (s *AzBlobStore) Delete(ctx context.Context, key string) error {
	blob := s.container.NewBlobClient(s.objectKey(key))
	_, err := blob.Delete(ctx, nil)
	if err != nil {
		// Deleting an absent key is not an error — pruning has to be
		// idempotent, and Azure, unlike S3, says 404 rather than shrugging.
		if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound) {
			return nil
		}
		return mapAzError(err, key)
	}
	return nil
}

func (s *AzBlobStore) Expire(ctx context.Context, appID string, keep []string) (int, error) {
	kept := make(map[string]bool, len(keep))
	for _, k := range keep {
		kept[k] = true
	}

	prefix := s.objectKey(releasePrefix(appID))
	pager := s.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix: &prefix,
	})

	removed := 0
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return removed, mapAzError(err, prefix)
		}
		for _, item := range page.Segment.BlobItems {
			if item.Name == nil {
				continue
			}
			key := strings.TrimPrefix(*item.Name, s.prefix)
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

// azMeta reads one metadata value case-insensitively.
//
// Azure title-cases metadata keys on the way out — a blob written with
// `toft_sha256` comes back as `Toft_sha256` — and a lookup that
// assumed the spelling it wrote would find nothing. That failure is the
// dangerous direction: an empty digest reads as "this artifact was stored
// without one", so verifyAndUnpack has nothing to compare against and a corrupt
// download unpacks clean.
func azMeta(meta map[string]*string, key string) string {
	for k, v := range meta {
		if strings.EqualFold(k, key) && v != nil {
			return *v
		}
	}
	return ""
}

// mapAzError collapses the SDK's error surface onto the three failure modes a
// caller must distinguish, and it matters in the same direction mapS3Error's
// comment names: a transient outage reported as ErrNotFound would have the
// reconciler conclude every release is gone and give up on the whole install.
//
// So the not-found list is explicit and short. Anything else — a 403, a 500, a
// DNS failure, a context deadline — is unreachable, which is retryable.
func mapAzError(err error, key string) error {
	if err == nil {
		return nil
	}
	if bloberror.HasCode(err,
		bloberror.BlobNotFound,
		bloberror.ContainerNotFound,
		bloberror.ResourceNotFound,
	) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	// A HEAD (GetProperties) reports a missing blob as a bare 404 with no error
	// code at all, the same gap HeadObject has on S3.
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) && respErr.StatusCode == 404 {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return fmt.Errorf("%w: %s: %v", ErrUnreachable, key, err)
}
