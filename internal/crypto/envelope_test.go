package crypto

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const kekKey = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"

func managed(t *testing.T, local string, retired []string, store DEKStore, kek KEK) *Encryptor {
	t.Helper()
	e, err := New(context.Background(), local, retired, Options{KEK: kek, DEKs: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func fake(t *testing.T) *FakeKEK {
	t.Helper()
	k, err := NewFakeKEK(kekKey)
	if err != nil {
		t.Fatalf("NewFakeKEK: %v", err)
	}
	return k
}

// The managed backend produces the same records the local one does. That is the
// property that keeps this one code path, and the property an install moving
// between backends depends on.
func TestManagedWritesTheSameRecordFormat(t *testing.T) {
	e := managed(t, "", nil, NewMemoryDEKStore(), fake(t))

	record, err := e.Encrypt("under a wrapped data key")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if got := RecordKeyID(record); got != e.EncryptKeyID() {
		t.Errorf("record carries key %q, want %q", got, e.EncryptKeyID())
	}
	if !strings.HasPrefix(record, recordVersion+":") {
		t.Errorf("managed record is not in the v2 format: %q", record[:min(len(record), 20)])
	}
	if !e.CanRead(record) {
		t.Error("the ring that wrote the record says it cannot read it")
	}

	plain, err := e.Decrypt(record)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plain != "under a wrapped data key" {
		t.Errorf("got %q", plain)
	}
	if e.Backend() != BackendManaged {
		t.Errorf("Backend() = %q, want %q", e.Backend(), BackendManaged)
	}
	if e.KEKURI() == "" {
		t.Error("a managed install reports no KEK URI")
	}
}

// Presence decides, and nothing else does.
func TestPresenceDecidesTheBackend(t *testing.T) {
	local, err := New(context.Background(), keyA, nil, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if local.Backend() != BackendLocal {
		t.Errorf("with no URI the backend is %q", local.Backend())
	}
	if local.KEKURI() != "" {
		t.Errorf("a local install reports KEK %q", local.KEKURI())
	}

	// And there is no unencrypted mode: with no URI, the key is required.
	if _, err := New(context.Background(), "", nil, Options{}); err == nil {
		t.Error("New accepted an install with neither a key nor a URI")
	}
}

// The data key survives a restart, because it is stored wrapped rather than
// regenerated — the failure mode that would silently orphan every record
// written before the restart.
func TestDataKeySurvivesARestart(t *testing.T) {
	store, kek := NewMemoryDEKStore(), fake(t)

	first := managed(t, "", nil, store, kek)
	record, err := first.Encrypt("written before the restart")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	second := managed(t, "", nil, store, kek)
	if second.EncryptKeyID() != first.EncryptKeyID() {
		t.Errorf("the restart minted a new data key: %s then %s", first.EncryptKeyID(), second.EncryptKeyID())
	}
	if store.PutCalls != 1 {
		t.Errorf("the store was written %d times, want once", store.PutCalls)
	}
	plain, err := second.Decrypt(record)
	if err != nil {
		t.Fatalf("Decrypt after restart: %v", err)
	}
	if plain != "written before the restart" {
		t.Errorf("got %q", plain)
	}
}

// The cache is what keeps a KMS call off every secret. It is also what bounds
// how long a revoked grant keeps working, so the TTL has to actually expire.
func TestDEKCacheHoldsAndThenExpires(t *testing.T) {
	store, kek := NewMemoryDEKStore(), fake(t)
	e, err := New(context.Background(), "", nil, Options{KEK: kek, DEKs: store, CacheTTL: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	after := kek.Unwraps
	for i := 0; i < 20; i++ {
		if _, err := e.Encrypt("cheap"); err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
	}
	if kek.Unwraps != after {
		t.Errorf("%d unwrap calls for 20 encryptions — the cache is not holding", kek.Unwraps-after)
	}

	time.Sleep(70 * time.Millisecond)
	if _, err := e.Encrypt("after the TTL"); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if kek.Unwraps == after {
		t.Error("the cache never expired — a revoked KMS grant would keep working until a restart")
	}
}

// An unreachable KMS is fatal at startup and stays fatal. There is no runtime
// fallback to a local key, ever: an escape hatch that triggers on its own is a
// downgrade attack.
func TestUnreachableKMSIsFatalAndNeverFallsBack(t *testing.T) {
	kek := fake(t)
	kek.Err = errors.New("kms is down")

	if _, err := New(context.Background(), keyA, nil, Options{KEK: kek, DEKs: NewMemoryDEKStore()}); err == nil {
		t.Fatal("New succeeded with an unreachable KMS")
	}

	// And an outage that begins after startup fails the operation rather than
	// quietly writing under the local key that is also configured.
	store, live := NewMemoryDEKStore(), fake(t)
	e, err := New(context.Background(), keyA, nil, Options{KEK: live, DEKs: store, CacheTTL: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dekID := e.EncryptKeyID()
	live.Err = errors.New("kms went away")
	time.Sleep(5 * time.Millisecond)

	record, err := e.Encrypt("during the outage")
	if err == nil {
		if got := RecordKeyID(record); got != dekID {
			t.Fatalf("a KMS outage fell back to key %s — the local key", got)
		}
		t.Fatal("Encrypt succeeded during a KMS outage")
	}
}

// The path from local to managed on a live install: the old key keeps reading,
// the data key writes, and the sweep sees two ids and no special case.
func TestLocalToManagedKeepsReadingTheOldRecords(t *testing.T) {
	before := newFor(t, keyA)
	old, err := before.Encrypt("written under the local key")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ancient := legacy(t, before, "written before key identifiers")

	// The move: the local key is retired, the URI arrives.
	after := managed(t, "", []string{keyA}, NewMemoryDEKStore(), fake(t))

	for name, record := range map[string]string{"tagged": old, "untagged": ancient} {
		if !after.CanRead(record) {
			t.Errorf("the %s local record reads as unreadable after the move", name)
		}
		if _, err := after.Decrypt(record); err != nil {
			t.Errorf("Decrypt of the %s local record after the move: %v", name, err)
		}
	}

	fresh, err := after.Encrypt("written after the move")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if RecordKeyID(fresh) == before.EncryptKeyID() {
		t.Error("a record written after the move went under the retired local key")
	}
	if RecordKeyID(fresh) != after.EncryptKeyID() {
		t.Errorf("a new record is under %s, want the data key %s", RecordKeyID(fresh), after.EncryptKeyID())
	}

	// Both halves are in the ring, which is what the startup report and the
	// sweep read.
	ids := after.KeyIDs()
	if len(ids) != 2 || ids[0] != after.EncryptKeyID() {
		t.Errorf("the ring is %v, want the data key first and the retired local key after it", ids)
	}
}

// A record naming a data key this install has no wrapped copy of must say so,
// rather than failing as an ordinary decryption error.
func TestUnknownDataKeyIsRefusedByName(t *testing.T) {
	other := managed(t, "", nil, NewMemoryDEKStore(), fake(t))
	orphan, err := other.Encrypt("written on another install")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	e := managed(t, "", nil, NewMemoryDEKStore(), fake(t))
	if e.CanRead(orphan) {
		t.Fatal("CanRead said yes to a data key this install does not hold")
	}
	_, err = e.Decrypt(orphan)
	if err == nil {
		t.Fatal("a record under an unknown data key decrypted")
	}
	if !strings.Contains(err.Error(), RecordKeyID(orphan)) {
		t.Errorf("the error does not name the data key: %v", err)
	}
}

// A KMS that hands back different key material than it was given is caught at
// the unwrap rather than as an authentication failure on every record.
func TestUnwrapVerifiesTheKeyItGotBack(t *testing.T) {
	store, kek := NewMemoryDEKStore(), fake(t)
	e := managed(t, "", nil, store, kek)

	// Substitute a different wrapped key under the same id.
	wrongDEK, err := kek.Wrap(context.Background(), []byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if err := store.PutDEK(e.EncryptKeyID(), wrongDEK, kek.URI()); err != nil {
		t.Fatalf("PutDEK: %v", err)
	}

	fresh, err := New(context.Background(), "", nil, Options{KEK: kek, DEKs: store})
	if err == nil {
		t.Fatalf("New accepted a data key whose material does not match its id (%s)", fresh.EncryptKeyID())
	}
	if !strings.Contains(err.Error(), "different key") {
		t.Errorf("the error does not say the KMS returned the wrong key: %v", err)
	}
}

func TestUnknownURISchemeNamesTheOnesThatExist(t *testing.T) {
	_, err := KEKFromURI(context.Background(), "gcp-kms://projects/p/locations/l/keyRings/r/cryptoKeys/k")
	if err == nil {
		t.Fatal("KEKFromURI accepted a scheme this build does not implement")
	}
	for _, want := range []string{"gcp-kms://", SchemeAWSKMS, SchemeVault, SchemeAzureKeyVault} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestURIParsing(t *testing.T) {
	refused := map[string]string{
		"no scheme":            "arn:aws:kms:us-east-1:1234:alias/toft",
		"aws with no arn":      SchemeAWSKMS,
		"aws with a bare name": SchemeAWSKMS + "toft",
		"aws with a short arn": SchemeAWSKMS + "arn:aws:kms",
		"vault with no path":   SchemeVault + "vault.internal:8200",
		"vault with no keys":   SchemeVault + "vault.internal:8200/transit/toft",
		"vault with no name":   SchemeVault + "vault.internal:8200/transit/keys/",
	}
	for name, uri := range refused {
		if _, err := KEKFromURI(context.Background(), uri); err == nil {
			t.Errorf("%s was accepted: %q", name, uri)
		}
	}

	// Vault is built without talking to anything, so it can be checked here.
	// AWS is not: LoadDefaultConfig reaches for credentials.
	kek, err := KEKFromURI(context.Background(), SchemeVault+"vault.internal:8200/transit/keys/toft")
	if err != nil {
		t.Fatalf("a well-formed Vault URI was refused: %v", err)
	}
	v, ok := kek.(*vaultTransit)
	if !ok {
		t.Fatalf("hcvault:// built a %T", kek)
	}
	if v.addr != "https://vault.internal:8200" || v.mount != "transit" || v.keyName != "toft" {
		t.Errorf("parsed as addr=%q mount=%q key=%q", v.addr, v.mount, v.keyName)
	}
	if v.URI() != SchemeVault+"vault.internal:8200/transit/keys/toft" {
		t.Errorf("URI() = %q", v.URI())
	}
}

func TestAWSRegionComesFromTheARN(t *testing.T) {
	region, err := regionFromARN("arn:aws:kms:eu-west-2:123456789012:alias/toft")
	if err != nil {
		t.Fatalf("regionFromARN: %v", err)
	}
	if region != "eu-west-2" {
		t.Errorf("region = %q", region)
	}
	if _, err := regionFromARN("arn:aws:kms::123456789012:alias/toft"); err == nil {
		t.Error("an ARN with no region was accepted")
	}
}

// The managed backend still needs somewhere to keep the wrapped key. Refused at
// construction rather than at the first write.
func TestManagedNeedsAStore(t *testing.T) {
	if _, err := New(context.Background(), keyA, nil, Options{KEK: fake(t)}); err == nil {
		t.Error("New built a managed keyring with nowhere to store the wrapped key")
	}
}

// The Vault Transit client is hand-rolled over net/http rather than taken from
// the SDK, so the wire format it speaks is this package's responsibility and is
// tested against a server rather than reasoned about: base64 in, "vault:v1:…"
// out, the token in a header, and the whole envelope round-tripping through it.
func TestVaultTransitSpeaksTheWireFormat(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "s.testtoken")

	var seenToken, seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenToken = r.Header.Get(vaultTokenHeader)
		seenPath = r.URL.Path

		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/encrypt/toft"):
			// Transit takes base64 and returns an opaque versioned string.
			raw, err := base64.StdEncoding.DecodeString(body["plaintext"])
			if err != nil {
				http.Error(w, "plaintext is not base64", http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, `{"data":{"ciphertext":"vault:v1:%s"}}`,
				base64.StdEncoding.EncodeToString(raw))
		case strings.HasSuffix(r.URL.Path, "/decrypt/toft"):
			blob := strings.TrimPrefix(body["ciphertext"], "vault:v1:")
			raw, err := base64.StdEncoding.DecodeString(blob)
			if err != nil {
				http.Error(w, "ciphertext is not what we issued", http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, `{"data":{"plaintext":"%s"}}`, base64.StdEncoding.EncodeToString(raw))
		default:
			http.Error(w, "no such path", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// http+ in the host is how a development Vault is reached without a second
	// variable for the scheme.
	uri := SchemeVault + "http+" + strings.TrimPrefix(srv.URL, "http://") + "/transit/keys/toft"
	kek, err := KEKFromURI(context.Background(), uri)
	if err != nil {
		t.Fatalf("KEKFromURI: %v", err)
	}

	e, err := New(context.Background(), "", nil, Options{KEK: kek, DEKs: NewMemoryDEKStore()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	record, err := e.Encrypt("through a real HTTP round trip")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	plain, err := e.Decrypt(record)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plain != "through a real HTTP round trip" {
		t.Errorf("got %q", plain)
	}
	if seenToken != "s.testtoken" {
		t.Errorf("the token header was %q", seenToken)
	}
	if !strings.HasPrefix(seenPath, "/v1/transit/") {
		t.Errorf("the last path was %q, want /v1/transit/…", seenPath)
	}
	if e.KEKURI() != uri {
		t.Errorf("KEKURI() = %q", e.KEKURI())
	}
}

// A Vault that refuses must fail the operation with what it said, rather than
// producing a record nobody can read.
func TestVaultErrorsCarryVaultsOwnWords(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "s.expired")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"errors":["permission denied"]}`)
	}))
	defer srv.Close()

	kek, err := KEKFromURI(context.Background(),
		SchemeVault+"http+"+strings.TrimPrefix(srv.URL, "http://")+"/transit/keys/toft")
	if err != nil {
		t.Fatalf("KEKFromURI: %v", err)
	}

	_, err = New(context.Background(), "", nil, Options{KEK: kek, DEKs: NewMemoryDEKStore()})
	if err == nil {
		t.Fatal("New succeeded against a Vault that refused every call")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the error does not carry Vault's own words: %v", err)
	}
}

// With no VAULT_TOKEN and no service-account token on disk, the failure has to
// say which of the two ways in was missing.
func TestVaultSaysHowItCouldNotAuthenticate(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "")

	kek, err := KEKFromURI(context.Background(), SchemeVault+"vault.invalid:8200/transit/keys/toft")
	if err != nil {
		t.Fatalf("KEKFromURI: %v", err)
	}
	_, err = kek.Wrap(context.Background(), make([]byte, 32))
	if err == nil {
		t.Fatal("Wrap succeeded with no credentials at all")
	}
	if !strings.Contains(err.Error(), "VAULT_TOKEN") {
		t.Errorf("the error does not name the variable that was missing: %v", err)
	}
}
