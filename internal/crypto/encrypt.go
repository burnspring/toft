package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// The stored record format.
//
//	v2:<keyid>:<hex>   written by this version, and by every version after it
//	<hex>              written before key identifiers existed
//
// A record written before this landed is bare hex and can therefore never
// contain a colon, which is what makes the two forms tellable apart with no
// migration, no schema change and no rewrite of a single row. That property is
// the whole reason the format was chosen — a rotation that has to rewrite the
// estate before it can begin is the flag day this exists to end.
const (
	recordVersion = "v2"

	// LegacyKeyID is what an untagged record is called when a key id has to be
	// named for one. Nothing records which key actually wrote those records, so
	// it is a label rather than a lookup — see Decrypt.
	LegacyKeyID = "0"

	// keyIDBytes is how much of a key's SHA-256 becomes its id. Four bytes is
	// eight hex characters: long enough that two keys on one install will not
	// collide, short enough to read in a log line, and a preimage of nothing.
	keyIDBytes = 4
)

// key is one key in the ring: its opaque id and the AEAD built from it.
type key struct {
	id  string
	gcm cipher.AEAD
}

// Encryptor is a keyring. One key encrypts; every key in the ring can decrypt,
// which is what lets a new key be introduced without touching a stored record.
//
// **Two backends, one code path.** The local backend's keys come from
// ENCRYPTION_KEY and ENCRYPTION_KEY_OLD; the managed backend's come from a data
// key this process generated, wrapped by a KEK that never leaves the KMS, and
// unwrapped on demand into a cache with a TTL. Everything below the seam —
// AES-256-GCM, the nonce, the record format, the key id — is identical, which
// is what keeps this one implementation rather than two encryption schemes with
// a switch over them.
//
// The two halves are live at once on a managed install, and that is not an edge
// case: it is how an install that has already stored secrets locally moves to a
// KMS. The old key stays in ENCRYPTION_KEY_OLD and keeps reading what it wrote,
// while the DEK writes everything new.
type Encryptor struct {
	// encrypt is the key every new record is written under and stamped with —
	// on a managed install, the zero value, because the DEK is resolved through
	// env instead and may need a KMS call to answer.
	encrypt key
	// decrypt is every *local* key by id, including the local encrypt key.
	decrypt map[string]cipher.AEAD
	// order is every local key with the encrypt key first. It exists for the
	// untagged case, which has no id to look up.
	order []key
	// env is the envelope half, nil unless ENCRYPTION_KEY_URI is set.
	env *envelope
}

// NewEncryptor builds a single-key ring — the shape an install that has never
// rotated has, and the one every test and every caller that does not care about
// retired keys wants.
func NewEncryptor(hexKey string) (*Encryptor, error) {
	return NewKeyring(hexKey, nil)
}

// NewKeyring builds the ring from the encrypt key and any number of retired
// keys kept only for reading. Retired keys that repeat the encrypt key, or each
// other, are folded together rather than refused: an operator part-way through
// a rotation should not have to reason about whether a key appears twice.
func NewKeyring(hexKey string, retired []string) (*Encryptor, error) {
	return newLocalKeyring(hexKey, retired, true)
}

// newLocalKeyring builds the local half. `required` is false only on a managed
// install, where an absent ENCRYPTION_KEY means "nothing was stored under a
// local key" rather than a missing configuration.
func newLocalKeyring(hexKey string, retired []string, required bool) (*Encryptor, error) {
	e := &Encryptor{decrypt: map[string]cipher.AEAD{}}

	if hexKey == "" && !required {
		// No local encrypt key: every new record goes to the envelope half, and
		// anything in ENCRYPTION_KEY_OLD below is read-only, as always.
	} else {
		k, err := newKey(hexKey)
		if err != nil {
			return nil, err
		}
		e.encrypt = k
		e.decrypt[k.id] = k.gcm
		e.order = []key{k}
	}

	for i, h := range retired {
		rk, err := newKey(h)
		if err != nil {
			return nil, fmt.Errorf("retired encryption key %d of %d: %w", i+1, len(retired), err)
		}
		if _, dup := e.decrypt[rk.id]; dup {
			continue
		}
		e.decrypt[rk.id] = rk.gcm
		e.order = append(e.order, rk)
	}

	return e, nil
}

func newKey(hexKey string) (key, error) {
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return key{}, fmt.Errorf("invalid encryption key hex: %w", err)
	}
	if len(raw) != 32 {
		return key{}, fmt.Errorf("encryption key must be 32 bytes (64 hex chars), got %d bytes", len(raw))
	}

	block, err := aes.NewCipher(raw)
	if err != nil {
		return key{}, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return key{}, fmt.Errorf("creating GCM: %w", err)
	}

	return key{id: keyID(raw), gcm: gcm}, nil
}

// keyID fingerprints the key material. It is derived rather than configured so
// that an operator cannot label two different keys the same, and it is a hash
// rather than the key so that a record, a log line and an error message can
// carry it without carrying anything an attacker wants.
func keyID(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:keyIDBytes])
}

// Backend names where the key that writes new records comes from — "local" or
// "managed". Read-only and derived from configuration rather than declared, for
// the same reason `topology` is: an operator should not be able to mislabel
// their own install, and a surface that states what is protecting the data must
// never be able to say something untrue about it.
func (e *Encryptor) Backend() string {
	if e.env != nil {
		return BackendManaged
	}
	return BackendLocal
}

// KEKURI is the key-encrypting key this install was configured with, or empty
// on the local backend. It carries no credential by construction.
func (e *Encryptor) KEKURI() string {
	if e.env == nil {
		return ""
	}
	return e.env.kek.URI()
}

// EncryptKeyID is the id every record written from now on carries.
func (e *Encryptor) EncryptKeyID() string {
	if e.env != nil {
		return e.env.currentID
	}
	return e.encrypt.id
}

// KeyIDs is every id the ring can read, the encrypt key first. On a managed
// install the local ids after it are the retired keys that still read what was
// written before the move to a KMS.
func (e *Encryptor) KeyIDs() []string {
	out := make([]string, 0, len(e.order)+1)
	if e.env != nil {
		out = append(out, e.env.currentID)
	}
	for _, k := range e.order {
		out = append(out, k.id)
	}
	return out
}

// encryptWith resolves the key new records are written under. On the local
// backend it cannot fail; on the managed one it is a cache read, and a KMS call
// once per TTL.
func (e *Encryptor) encryptWith(ctx context.Context) (string, cipher.AEAD, error) {
	if e.env != nil {
		gcm, err := e.env.aead(ctx, e.env.currentID)
		if err != nil {
			return "", nil, err
		}
		return e.env.currentID, gcm, nil
	}
	if e.encrypt.gcm == nil {
		return "", nil, fmt.Errorf("no encryption key is configured")
	}
	return e.encrypt.id, e.encrypt.gcm, nil
}

func (e *Encryptor) Encrypt(plaintext string) (string, error) {
	return e.EncryptContext(context.Background(), plaintext)
}

// EncryptContext is Encrypt with a deadline for the KMS call the managed
// backend may need. Encrypt is the shape every call site already uses and stays
// the one they use; this exists for the places that hold a request context.
func (e *Encryptor) EncryptContext(ctx context.Context, plaintext string) (string, error) {
	id, gcm, err := e.encryptWith(ctx)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return recordVersion + ":" + id + ":" + hex.EncodeToString(ciphertext), nil
}

func (e *Encryptor) Decrypt(record string) (string, error) {
	return e.DecryptContext(context.Background(), record)
}

func (e *Encryptor) DecryptContext(ctx context.Context, record string) (string, error) {
	version, id, body, tagged := splitRecord(record)

	if !tagged {
		// Nothing says which key wrote an untagged record, so every local key
		// is tried. This is the behaviour the format exists to stop needing,
		// and it is bounded: no new record is ever written untagged, so this
		// set only shrinks. The envelope half is not tried — an untagged record
		// predates key identifiers, so it predates any KMS this install has.
		for _, k := range e.order {
			if plain, err := open(k.gcm, record); err == nil {
				return plain, nil
			}
		}
		return "", fmt.Errorf("decrypting: no key in the keyring reads this record — " +
			"it predates key identifiers and was written under a key this install no longer has")
	}

	if version != recordVersion {
		return "", fmt.Errorf("decrypting: unknown record version %q, this build writes and reads %q", version, recordVersion)
	}
	// Local first: it needs no network, and on a managed install these are the
	// retired keys that read everything written before the move.
	if gcm, ok := e.decrypt[id]; ok {
		return open(gcm, body)
	}
	if e.env != nil {
		gcm, err := e.env.aead(ctx, id)
		if err != nil {
			return "", fmt.Errorf("decrypting: %w", err)
		}
		return open(gcm, body)
	}
	return "", fmt.Errorf("decrypting: this record was written under key %s, which is not in the keyring — "+
		"put that key in ENCRYPTION_KEY or ENCRYPTION_KEY_OLD", id)
}

// RecordKeyID names the key a stored record was written under, for the report
// that tells an operator what a keyring cannot read. An untagged record answers
// LegacyKeyID, which is a statement that nothing knows rather than a key.
func RecordKeyID(record string) string {
	_, id, _, tagged := splitRecord(record)
	if !tagged {
		return LegacyKeyID
	}
	return id
}

// CanRead reports whether the ring holds the key this record names. It says
// nothing about whether the bytes are intact — only that the right key is
// present, which is the question a startup report and a settings field ask.
// An untagged record is read as a maybe: only a decrypt can answer for it.
func (e *Encryptor) CanRead(record string) bool {
	_, id, _, tagged := splitRecord(record)
	if !tagged {
		_, err := e.Decrypt(record)
		return err == nil
	}
	if _, ok := e.decrypt[id]; ok {
		return true
	}
	if e.env != nil {
		// A wrapped copy this install holds is the answer, without a KMS call:
		// the question is whether the key is *this install's*, and a KMS that
		// is momentarily unreachable does not change that.
		_, ok, err := e.env.store.DEK(id)
		return err == nil && ok
	}
	return false
}

// splitRecord separates the two forms. A record with no colon is untagged and
// is returned whole; anything else must be exactly three fields, and a
// malformed tagged record is reported as tagged with an empty version so
// Decrypt refuses it by name rather than trying every key against rubbish.
func splitRecord(record string) (version, id, body string, tagged bool) {
	if !strings.Contains(record, ":") {
		return "", LegacyKeyID, record, false
	}
	parts := strings.SplitN(record, ":", 3)
	if len(parts) != 3 {
		return "", "", record, true
	}
	return parts[0], parts[1], parts[2], true
}

func open(gcm cipher.AEAD, ciphertextHex string) (string, error) {
	ciphertext, err := hex.DecodeString(ciphertextHex)
	if err != nil {
		return "", fmt.Errorf("decoding ciphertext: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypting: %w", err)
	}

	return string(plaintext), nil
}
