package crypto

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Which backend an install runs on, and how it says so.
//
// **Presence decides.** `ENCRYPTION_KEY_URI` set means managed; unset means
// local. That is the idiom `KubernetesConfigured()`, `ECSConfigured()` and
// `IsolatedConfigured()` already establish, and it is the "absent, not dormant"
// rule stated as configuration. There is no mode flag, so there is no way to
// declare a mode that disagrees with the rest of the configuration.
//
// **The local backend is a peer, not a fallback.** It is what `dev.sh`, the
// whole test suite, the one-box compose install (ADR §43) and every air-gapped
// site run, and it is documented without apology and supported indefinitely. It
// gets no startup warning and no "insecure mode" banner: it is a configuration
// chosen deliberately, and a nag would train operators to ignore the startup
// output, which is where the fatal errors are.
const (
	BackendLocal   = "local"
	BackendManaged = "managed"
)

// The URI schemes, following Tink's convention rather than a private one.
const (
	SchemeAWSKMS        = "aws-kms://"
	SchemeVault         = "hcvault://"
	SchemeAzureKeyVault = "azurekeyvault://"
)

// KnownSchemes is what an unknown one is reported against. Three schemes cover
// nearly everyone and there is no plugin system, so this list is the whole of
// it (ADR §62). A fourth is a code change, which is the right amount of
// friction: a KEK is the thing standing between a stolen database dump and the
// data in it, and a plugin surface there would be a way to configure one that
// is not.
var KnownSchemes = []string{SchemeAWSKMS, SchemeVault, SchemeAzureKeyVault}

// Options carry what a keyring needs beyond the key material.
type Options struct {
	// URI selects the managed backend when set. Empty means local.
	URI string
	// DEKs is where wrapped data keys are stored. Required when URI is set.
	DEKs DEKStore
	// KEK overrides the one the URI would build. For tests, which use the fake
	// rather than a cloud account.
	KEK KEK
	// CacheTTL overrides DefaultDEKCacheTTL.
	CacheTTL time.Duration
}

// New builds the keyring an install runs on: the local keys always, and the
// envelope half when a URI selects it.
//
// The two are not exclusive, and that is the supported path from local to
// managed on a live install. The old local key stays in ENCRYPTION_KEY_OLD and
// keeps reading everything written before the move; the DEK writes everything
// after it; and `toft rotate-key` sweeps across the two with no special
// case, because to the sweep they are just two key ids.
func New(ctx context.Context, hexKey string, retired []string, opts Options) (*Encryptor, error) {
	managed := opts.URI != "" || opts.KEK != nil

	// ENCRYPTION_KEY stays required on the local backend — there is no
	// unencrypted mode. On the managed one it becomes optional, because the DEK
	// is the encrypt key and any local key present is there to read what was
	// written before the move.
	e, err := newLocalKeyring(hexKey, retired, !managed)
	if err != nil {
		return nil, err
	}
	if !managed {
		return e, nil
	}

	if opts.DEKs == nil {
		return nil, fmt.Errorf("the managed backend needs somewhere to store wrapped data keys")
	}
	kek := opts.KEK
	if kek == nil {
		if kek, err = KEKFromURI(ctx, opts.URI); err != nil {
			return nil, err
		}
	}
	env, err := newEnvelope(ctx, kek, opts.DEKs, opts.CacheTTL)
	if err != nil {
		return nil, err
	}
	e.env = env
	return e, nil
}

// KEKFromURI builds the key-encrypting key the URI names.
//
// **No credentials appear in the URI**, by construction: authentication comes
// from the ambient chain — the instance profile or IRSA for `aws-kms://`,
// `VAULT_TOKEN` or Kubernetes auth for `hcvault://`, workload or managed
// identity for `azurekeyvault://`. A URI that could carry a secret would end up
// in `.env`, in `values.yaml`, and in a support ticket.
//
// An unknown scheme is an error naming the ones that exist, so a typo fails at
// startup rather than at the first decrypt an hour later.
func KEKFromURI(ctx context.Context, uri string) (KEK, error) {
	switch {
	case strings.HasPrefix(uri, SchemeAWSKMS):
		return newAWSKMS(ctx, uri)
	case strings.HasPrefix(uri, SchemeVault):
		return newVaultTransit(uri)
	case strings.HasPrefix(uri, SchemeAzureKeyVault):
		return newAzureKeyVault(uri)
	default:
		scheme := uri
		if i := strings.Index(uri, "://"); i >= 0 {
			scheme = uri[:i+3]
		}
		return nil, fmt.Errorf("ENCRYPTION_KEY_URI: unknown scheme %q — this build understands %s",
			scheme, strings.Join(KnownSchemes, ", "))
	}
}
