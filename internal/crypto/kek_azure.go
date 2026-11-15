package crypto

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
)

// Azure Key Vault as the key-encrypting key.
//
//	azurekeyvault://toft-kv.vault.azure.net/keys/toft/8f3b…
//
// The vault host is in the URI, so nothing here needs a second variable to mean
// anything — the same rule newAWSKMS reads the region out of the ARN for.
// Credentials come from the ambient chain and never from the URI: workload
// identity in AKS, a managed identity on a VM, `az login` on a developer's
// machine. DefaultAzureCredential is that chain.
//
// **The version is part of the URI and is not optional.** Key Vault accepts a
// versionless identifier and resolves it to whatever the current version is at
// call time, which would mean a rotation performed *in the vault* silently
// changes which key wraps — the exact opposite of ADR §62's rule that every
// ciphertext names the key that wrote it. Records written before the rotation
// would still name the old key id while nothing on this install could tell that
// their KEK had changed underneath them. So a versionless URI is refused at
// startup, where it is one line of output, rather than months later as a DEK
// that will not unwrap.
//
// **Wrapping is RSA-OAEP-256, not a symmetric encrypt.** Key Vault offers
// symmetric key operations only on Managed HSM, which is a different and far
// more expensive product; the standard vault holds RSA keys and wraps with
// them. This is invisible above this package: §62's record format stores the
// key *id* and never the algorithm, so nothing else learns which was used. It
// also makes §63's envelope argument stronger rather than weaker — a 2048-bit
// RSA wrap caps its payload at about 190 bytes, so encrypting anything larger
// than a data key directly under the KEK was never on the table here.

type azureKeyVault struct {
	client  *azkeys.Client
	keyName string
	version string
	uri     string
}

func newAzureKeyVault(uri string) (KEK, error) {
	host, name, version, err := parseAzureKeyVaultURI(uri)
	if err != nil {
		return nil, err
	}

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("ENCRYPTION_KEY_URI: loading Azure credentials: %w", err)
	}
	client, err := azkeys.NewClient("https://"+host, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("ENCRYPTION_KEY_URI: %s: %w", host, err)
	}

	return &azureKeyVault{client: client, keyName: name, version: version, uri: uri}, nil
}

// parseAzureKeyVaultURI is pure, so the refusals above are testable without an
// Azure account: the credential chain is reached only after the URI is known to
// be well-formed, which is also the order the errors should arrive in.
func parseAzureKeyVaultURI(uri string) (host, name, version string, err error) {
	rest := strings.TrimPrefix(uri, SchemeAzureKeyVault)
	host, path, ok := strings.Cut(rest, "/")
	if !ok || host == "" {
		return "", "", "", fmt.Errorf("ENCRYPTION_KEY_URI: %s needs <vault>/keys/<name>/<version>, e.g. "+
			"%stoft-kv.vault.azure.net/keys/toft/8f3b1c2d4e5f6a7b8c9d0e1f2a3b4c5d",
			SchemeAzureKeyVault, SchemeAzureKeyVault)
	}

	// keys/<name>/<version> — the shape of Key Vault's own key identifiers, so
	// an operator can paste the portal's "Key Identifier" and change only the
	// scheme.
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 2 && parts[0] == "keys" && parts[1] != "" {
		return "", "", "", fmt.Errorf("ENCRYPTION_KEY_URI: %q names no key version. Key Vault resolves a "+
			"versionless identifier to whatever is current at call time, so rotating the key in the "+
			"vault would silently change which key wraps while every existing record still names the "+
			"old one. Append the version: %s%s/keys/%s/<version>",
			path, SchemeAzureKeyVault, host, parts[1])
	}
	if len(parts) != 3 || parts[0] != "keys" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("ENCRYPTION_KEY_URI: %q should be keys/<name>/<version>, e.g. "+
			"keys/toft/8f3b1c2d4e5f6a7b8c9d0e1f2a3b4c5d", path)
	}
	return host, parts[1], parts[2], nil
}

func (a *azureKeyVault) URI() string { return a.uri }

func (a *azureKeyVault) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	alg := azkeys.EncryptionAlgorithmRSAOAEP256
	out, err := a.client.WrapKey(ctx, a.keyName, a.version, azkeys.KeyOperationParameters{
		Algorithm: &alg,
		Value:     dek,
	}, nil)
	if err != nil {
		return nil, err
	}
	if len(out.Result) == 0 {
		return nil, fmt.Errorf("key vault returned no wrapped key")
	}
	return out.Result, nil
}

func (a *azureKeyVault) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	// The version is passed on unwrap as well as wrap, and it is the one this
	// install was configured with rather than anything read off the blob. Key
	// Vault's wrapped output carries no key identifier, so there is nothing to
	// read — but the property that matters is the same one awsKMS keeps by
	// passing KeyId on Decrypt: the *grant* is the control, not the blob.
	alg := azkeys.EncryptionAlgorithmRSAOAEP256
	out, err := a.client.UnwrapKey(ctx, a.keyName, a.version, azkeys.KeyOperationParameters{
		Algorithm: &alg,
		Value:     wrapped,
	}, nil)
	if err != nil {
		return nil, err
	}
	return out.Result, nil
}
