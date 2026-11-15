package crypto

import (
	"context"
	"strings"
	"testing"
)

// A versionless Key Vault URI must be refused at startup.
//
// This is one of the failures that would otherwise be silent for months. Key
// Vault resolves a versionless identifier to whatever the current version is at
// call time, so a rotation performed in the vault would re-point every future
// wrap while every existing record still names the old key id — ADR §62's rule
// that every ciphertext names the key that wrote it, quietly broken with nothing
// on the install able to notice. The first symptom would be a DEK that will not
// unwrap, long after anyone remembers rotating anything.
//
// The refusal has to explain that, because "invalid URI" would read as a typo
// and get "fixed" by removing the version.
func TestVersionlessKeyVaultURIIsRefused(t *testing.T) {
	uri := SchemeAzureKeyVault + "toft-kv.vault.azure.net/keys/toft"
	_, _, _, err := parseAzureKeyVaultURI(uri)
	if err == nil {
		t.Fatal("a versionless Key Vault URI was accepted")
	}
	for _, want := range []string{"version", "rotat"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say why (%q missing): %v", want, err)
		}
	}
	// And it must be refused through the door an install actually comes in by,
	// not only by the parser.
	if _, err := KEKFromURI(context.Background(), uri); err == nil {
		t.Fatal("KEKFromURI accepted a versionless Key Vault URI")
	}
}

func TestAzureKeyVaultURIParsing(t *testing.T) {
	refused := map[string]string{
		"no path":          SchemeAzureKeyVault + "toft-kv.vault.azure.net",
		"no host":          SchemeAzureKeyVault + "/keys/toft/v1",
		"no keys segment":  SchemeAzureKeyVault + "kv.vault.azure.net/toft/v1",
		"empty name":       SchemeAzureKeyVault + "kv.vault.azure.net/keys//v1",
		"empty version":    SchemeAzureKeyVault + "kv.vault.azure.net/keys/toft/",
		"versionless":      SchemeAzureKeyVault + "kv.vault.azure.net/keys/toft",
		"too many parts":   SchemeAzureKeyVault + "kv.vault.azure.net/keys/toft/v1/extra",
		"bare vault alone": SchemeAzureKeyVault,
	}
	for name, uri := range refused {
		if _, _, _, err := parseAzureKeyVaultURI(uri); err == nil {
			t.Errorf("%s was accepted: %q", name, uri)
		}
	}

	// The shape an operator pastes out of the portal, with the scheme swapped.
	host, key, version, err := parseAzureKeyVaultURI(
		SchemeAzureKeyVault + "toft-kv.vault.azure.net/keys/toft/8f3b1c2d4e5f6a7b8c9d0e1f2a3b4c5d")
	if err != nil {
		t.Fatalf("a well-formed Key Vault URI was refused: %v", err)
	}
	if host != "toft-kv.vault.azure.net" {
		t.Errorf("vault host is %q", host)
	}
	if key != "toft" {
		t.Errorf("key name is %q", key)
	}
	if version != "8f3b1c2d4e5f6a7b8c9d0e1f2a3b4c5d" {
		t.Errorf("key version is %q", version)
	}
}

// No credential may appear in the URI, on any scheme. The vault host is in it
// so that nothing needs a second variable to mean anything — the same rule
// newAWSKMS reads the region out of the ARN for — and that is the whole of what
// the URI carries.
func TestKeyVaultURICarriesNoCredential(t *testing.T) {
	uri := SchemeAzureKeyVault + "toft-kv.vault.azure.net/keys/toft/v1"
	host, _, _, err := parseAzureKeyVaultURI(uri)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.ContainsAny(host, "@:") {
		t.Errorf("the vault host %q looks like it carries userinfo or a port credential", host)
	}
}
