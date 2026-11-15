package crypto

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Two distinct 32-byte keys, as 64 hex characters each — the only shape
// NewEncryptor accepts.
const (
	keyA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	keyB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func newFor(t *testing.T, hexKey string) *Encryptor {
	t.Helper()
	e, err := NewEncryptor(hexKey)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return e
}

// bodyOf returns the hex half of a v2 record, having checked the two fields in
// front of it.
func bodyOf(t *testing.T, e *Encryptor, record string) []byte {
	t.Helper()
	parts := strings.SplitN(record, ":", 3)
	if len(parts) != 3 {
		t.Fatalf("record is not v2:<keyid>:<hex>: %q", record)
	}
	if parts[0] != recordVersion {
		t.Fatalf("record version is %q, want %q", parts[0], recordVersion)
	}
	if parts[1] != e.EncryptKeyID() {
		t.Fatalf("record carries key %q, want the encrypt key %q", parts[1], e.EncryptKeyID())
	}
	raw, err := hex.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("record body is not hex: %v", err)
	}
	return raw
}

// legacy writes a record in the pre-keyring format — bare hex, no version, no
// key id — which is what every row on an existing install holds.
func legacy(t *testing.T, e *Encryptor, plaintext string) string {
	t.Helper()
	record, err := e.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return hex.EncodeToString(bodyOf(t, e, record))
}

func TestRoundTrip(t *testing.T) {
	e := newFor(t, keyA)

	// The empty string is a real stored value: an env var may legitimately be
	// set to nothing, and it has to come back as nothing rather than as an
	// error.
	for _, plain := range []string{
		"",
		"hunter2",
		"ghp_0123456789abcdefghijklmnopqrstuvwxyz",
		strings.Repeat("x", 64*1024),
		"a value with \x00 a NUL and a ✓ and a newline\n",
	} {
		ciphertext, err := e.Encrypt(plain)
		if err != nil {
			t.Fatalf("Encrypt(%d bytes): %v", len(plain), err)
		}
		if ciphertext == plain && plain != "" {
			t.Fatalf("Encrypt returned its input")
		}
		bodyOf(t, e, ciphertext)
		got, err := e.Decrypt(ciphertext)
		if err != nil {
			t.Fatalf("Decrypt(%d bytes): %v", len(plain), err)
		}
		if got != plain {
			t.Errorf("round trip changed the value: got %d bytes, want %d", len(got), len(plain))
		}
	}
}

// A repeated nonce under one key is a total break of GCM — the keystream
// repeats and the authentication key falls out. This is the property the whole
// construction rests on, so it is asserted rather than assumed.
func TestNonceIsFreshPerEncryption(t *testing.T) {
	e := newFor(t, keyA)

	const n = 200
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		ciphertext, err := e.Encrypt("the same plaintext every time")
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		raw := bodyOf(t, e, ciphertext)
		nonce := string(raw[:e.encrypt.gcm.NonceSize()])
		if seen[nonce] {
			t.Fatalf("nonce reused after %d encryptions", i)
		}
		seen[nonce] = true
	}
}

// Encrypting one value twice must produce two different records, or the
// database tells a reader which apps share a secret without decrypting
// anything.
func TestCiphertextIsNotDeterministic(t *testing.T) {
	e := newFor(t, keyA)

	first, err := e.Encrypt("shared-across-two-apps")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	second, err := e.Encrypt("shared-across-two-apps")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if first == second {
		t.Error("two encryptions of one value are identical")
	}
}

// GCM authenticates, so a modified record must fail rather than decrypt to
// something. Every byte position matters: the nonce prefix, the ciphertext
// body and the tag.
func TestTamperedCiphertextIsRejected(t *testing.T) {
	e := newFor(t, keyA)

	ciphertext, err := e.Encrypt("the value that must not be edited")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	raw := bodyOf(t, e, ciphertext)
	prefix := recordVersion + ":" + e.EncryptKeyID() + ":"

	positions := map[string]int{
		"nonce":      0,
		"body":       e.encrypt.gcm.NonceSize() + 1,
		"final byte": len(raw) - 1,
	}
	for name, i := range positions {
		flipped := make([]byte, len(raw))
		copy(flipped, raw)
		flipped[i] ^= 0x01

		if plain, err := e.Decrypt(prefix + hex.EncodeToString(flipped)); err == nil {
			t.Errorf("a record with a flipped bit in the %s decrypted to %q", name, plain)
		}
	}

	// Truncation is not tampering with a byte, but it is the other way a
	// record arrives damaged.
	if _, err := e.Decrypt(prefix + hex.EncodeToString(raw[:len(raw)-1])); err == nil {
		t.Error("a truncated record decrypted")
	}
}

func TestMalformedRecordIsRejected(t *testing.T) {
	e := newFor(t, keyA)

	cases := map[string]string{
		"empty":                "",
		"not hex":              "not-hex-at-all",
		"odd hex length":       "abc",
		"shorter than a nonce": hex.EncodeToString([]byte("short")),
		"nonce only":           hex.EncodeToString(make([]byte, 12)),
	}
	for name, ciphertext := range cases {
		if plain, err := e.Decrypt(ciphertext); err == nil {
			t.Errorf("%s decrypted to %q", name, plain)
		}
	}
}

// The case the read sites already name as a rotated ENCRYPTION_KEY: a record
// written under one key must fail under another, and fail rather than return
// rubbish.
func TestWrongKeyFails(t *testing.T) {
	a := newFor(t, keyA)
	b := newFor(t, keyB)

	ciphertext, err := a.Encrypt("written under key A")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if plain, err := b.Decrypt(ciphertext); err == nil {
		t.Errorf("key B decrypted key A's record to %q", plain)
	}
}

func TestNewEncryptorRejectsBadKeys(t *testing.T) {
	cases := map[string]string{
		"empty":          "",
		"not hex":        strings.Repeat("z", 64),
		"31 bytes":       strings.Repeat("ab", 31),
		"33 bytes":       strings.Repeat("ab", 33),
		"odd hex length": strings.Repeat("a", 63),
		"a passphrase":   "correct-horse-battery-staple",
	}
	for name, key := range cases {
		if _, err := NewEncryptor(key); err == nil {
			t.Errorf("NewEncryptor accepted %s", name)
		}
	}

	if _, err := NewEncryptor(strings.ToUpper(keyA)); err != nil {
		t.Errorf("NewEncryptor rejected uppercase hex: %v", err)
	}
}

// Every new record is v2:<keyid>:<hex>, and the id is the encrypt key's — the
// wire contract the rotate command, the startup report and the settings fields
// all read.
func TestRecordCarriesTheEncryptKeyID(t *testing.T) {
	e := newFor(t, keyA)

	record, err := e.Encrypt("stamped")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if got := RecordKeyID(record); got != e.EncryptKeyID() {
		t.Errorf("RecordKeyID = %q, want %q", got, e.EncryptKeyID())
	}
	if !e.CanRead(record) {
		t.Error("the ring that wrote the record cannot read it")
	}

	// The id is derived from the key material, so it is stable across
	// processes and differs between keys — the two properties that make it
	// usable as a label.
	if newFor(t, keyA).EncryptKeyID() != e.EncryptKeyID() {
		t.Error("the same key produced two different ids")
	}
	if newFor(t, keyB).EncryptKeyID() == e.EncryptKeyID() {
		t.Error("two different keys produced the same id")
	}
	if e.EncryptKeyID() == LegacyKeyID {
		t.Error("a real key took the id reserved for untagged records")
	}
	if strings.Contains(e.EncryptKeyID(), ":") {
		t.Error("a key id contains the field separator")
	}
}

// The property the whole format turns on: a record written before any of this
// existed is bare hex, so it can never be mistaken for a tagged one, and it
// keeps reading with no migration.
func TestUntaggedRecordsStillRead(t *testing.T) {
	e := newFor(t, keyA)

	old := legacy(t, e, "written before key identifiers")
	if strings.Contains(old, ":") {
		t.Fatalf("a legacy record contains a colon: %q", old)
	}
	if got := RecordKeyID(old); got != LegacyKeyID {
		t.Errorf("RecordKeyID of an untagged record = %q, want %q", got, LegacyKeyID)
	}

	plain, err := e.Decrypt(old)
	if err != nil {
		t.Fatalf("Decrypt of an untagged record: %v", err)
	}
	if plain != "written before key identifiers" {
		t.Errorf("got %q", plain)
	}
}

// Local rotation, end to end: yesterday's key becomes a retired one, today's
// key writes everything new, and both halves of the estate read.
func TestRotationReadsBothHalves(t *testing.T) {
	before := newFor(t, keyB)

	tagged, err := before.Encrypt("written yesterday")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	untagged := legacy(t, before, "written long before yesterday")

	after, err := NewKeyring(keyA, []string{keyB})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}

	for name, record := range map[string]string{"tagged": tagged, "untagged": untagged} {
		if !after.CanRead(record) {
			t.Errorf("the ring reports it cannot read the %s record", name)
		}
		if _, err := after.Decrypt(record); err != nil {
			t.Errorf("Decrypt of the %s record after rotation: %v", name, err)
		}
	}

	// New writes go under the new key alone, which is what makes the sweep
	// finite: the retired key's share of the estate only shrinks.
	fresh, err := after.Encrypt("written today")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if got := RecordKeyID(fresh); got != after.EncryptKeyID() {
		t.Errorf("a new record was written under %q, want the encrypt key %q", got, after.EncryptKeyID())
	}
	if RecordKeyID(fresh) == before.EncryptKeyID() {
		t.Error("a new record was written under the retired key")
	}

	// And the retired key is read-only: a ring holding only it must not read
	// what the new key wrote.
	if before.CanRead(fresh) {
		t.Error("the retired key claims it can read a record written after it was retired")
	}
	if _, err := before.Decrypt(fresh); err == nil {
		t.Error("the retired key decrypted a record written after it was retired")
	}
}

// A record naming a key nobody has is the case that used to be indistinguishable
// from corruption. It must fail by name, and say what to do about it.
func TestRecordNamingAnAbsentKeyIsRefusedByName(t *testing.T) {
	orphan, err := newFor(t, keyB).Encrypt("written under a key that has been dropped")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	e := newFor(t, keyA)
	if e.CanRead(orphan) {
		t.Fatal("CanRead said yes to a key the ring does not hold")
	}
	_, err = e.Decrypt(orphan)
	if err == nil {
		t.Fatal("a record under an absent key decrypted")
	}
	if !strings.Contains(err.Error(), RecordKeyID(orphan)) {
		t.Errorf("the error does not name the key that wrote the record: %v", err)
	}
	if !strings.Contains(err.Error(), "ENCRYPTION_KEY_OLD") {
		t.Errorf("the error does not say where to put the key: %v", err)
	}
}

func TestUnknownRecordVersionIsRefused(t *testing.T) {
	e := newFor(t, keyA)

	record, err := e.Encrypt("from a build that does not exist yet")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	future := "v9" + strings.TrimPrefix(record, recordVersion)

	if _, err := e.Decrypt(future); err == nil {
		t.Fatal("a record from an unknown version decrypted")
	} else if !strings.Contains(err.Error(), "v9") {
		t.Errorf("the error does not name the version it refused: %v", err)
	}
}

func TestKeyringFoldsRepeatedKeys(t *testing.T) {
	e, err := NewKeyring(keyA, []string{keyB, keyA, keyB, ""})
	if err == nil {
		t.Fatalf("NewKeyring accepted an empty retired key, ring: %v", e.KeyIDs())
	}

	e, err = NewKeyring(keyA, []string{keyB, keyA, keyB})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	ids := e.KeyIDs()
	if len(ids) != 2 {
		t.Errorf("keyring holds %d keys, want 2: %v", len(ids), ids)
	}
	if ids[0] != e.EncryptKeyID() {
		t.Errorf("the encrypt key is not first: %v", ids)
	}
}

// An install that sets nothing new must behave exactly as it did, which is the
// claim step 3 of the plan rests on.
func TestSingleKeyRingIsTheDefault(t *testing.T) {
	e := newFor(t, keyA)
	if ids := e.KeyIDs(); len(ids) != 1 || ids[0] != e.EncryptKeyID() {
		t.Errorf("a keyless-rotation install has ring %v, want just the encrypt key", ids)
	}
}

// Two encryptors built from one key are interchangeable — which is what makes
// a restart with the same ENCRYPTION_KEY a no-op, and is the property a
// keyring will have to preserve.
func TestSameKeyDecryptsAcrossInstances(t *testing.T) {
	ciphertext, err := newFor(t, keyA).Encrypt("survives a restart")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := newFor(t, keyA).Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != "survives a restart" {
		t.Errorf("got %q", got)
	}
}
