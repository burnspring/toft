// Package gitconn owns the answer to one question: what does git authenticate
// with when this platform clones an app's repository.
//
// Before this package there was no such answer, only two implementations of it
// that disagreed. `Builder.decryptGitToken` returned a string or an error the
// build swallowed into "decrypting git credential: …", and the auto-deploy
// poller decrypted `apps.git_token` inline and recorded a `git_auth_failed`
// event with a sentence of its own. The two behaviours differed on the failure
// that matters most — a rotated `ENCRYPTION_KEY`, which is indistinguishable
// from a revoked token to the app's owner — and one of them told the owner
// nothing at all.
//
// **The resolver is the single reader of a clone credential**, and its answer
// is a closed list: the app's own `apps.git_token`, or nothing. A connected git
// host is the third, and it lands here rather than beside either call site
// precisely because a credential that may be minted over the network belongs
// next to the thing it is minted from, not next to the process that spawns git.
//
// What this package deliberately does not do is phrase a *git* failure.
// `builder.GitFailure.Explain` does that, and it takes a `Credential` so it can
// name the right thing to go and fix. The division is that gitconn knows where
// a credential came from and whether it could be read; the builder knows what
// git said.
package gitconn

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/safefetch"
)

// CredentialSource says where a clone credential came from. It exists so that
// a message can name something the reader can actually go and change.
type CredentialSource string

const (
	// SourceNone is an app with no credential at all, cloning anonymously.
	SourceNone CredentialSource = "none"
	// SourceAppToken is `apps.git_token`, pasted by the app's owner.
	SourceAppToken CredentialSource = "app_token"
	// SourceConnection is the install's own credential, held by a connected
	// git host and belonging to nobody in particular.
	SourceConnection CredentialSource = "connection"
)

// Credential is what the two call sites hold, and **neither may reconstruct
// it**. A caller that keeps `Token` and drops the rest has thrown away the only
// fact that makes the failure message correct.
type Credential struct {
	// Token is what reaches GitAuthEnv, and the only field that is a secret.
	Token string
	// Source is where it came from.
	Source CredentialSource
	// Connection is the connection's display name, when Source is
	// SourceConnection, and empty otherwise. It is a name a person would
	// recognise on the admin page, never an id.
	Connection string
}

// Unreadable is the error CloneCredential returns when a credential exists and
// cannot be turned into a token — in practice a rotated `ENCRYPTION_KEY`.
//
// It carries the sentence the reader is told rather than leaving each caller to
// invent one, which is the whole reason this package exists: the builder used
// to say "decrypting git credential: cipher: message authentication failed" in
// a build log and the poller used to say something else in an event, about the
// same app on the same afternoon.
type Unreadable struct {
	// Cred is what was found, with Token empty. Source and Connection are
	// still known — the row said where the credential lives, and only the
	// bytes would not open.
	Cred Credential
	// Summary is the owner-facing sentence.
	Summary string
	// Err is the decryption failure, for a log and for the event's detail.
	Err error
}

func (u *Unreadable) Error() string { return u.Summary }
func (u *Unreadable) Unwrap() error { return u.Err }

// Resolver answers CloneCredential. One per process.
type Resolver struct {
	db  *database.DB
	enc *crypto.Encryptor

	// mu guards the two caches below. Neither is a correctness mechanism.
	mu sync.Mutex
	// clients is one *safefetch.Client per connection, each built from that
	// connection's own base URL. **The exception is not a flag**: a connection
	// *is* a configured URL, so it gets a client the way the gallery and the
	// advisory feed already do. A GitHub Enterprise Server on 10.0.0.0/8 is
	// reachable because an administrator typed it into an admin-only field,
	// and nothing else on that network becomes reachable — a URL that arrives
	// *inside* a response from that host gets the strict client like every
	// other URL somebody else chose.
	clients map[string]*safefetch.Client
	// minted is the installation-token cache, keyed by connection **and
	// repository** because a mint is narrowed to the repository being cloned.
	// Several nodes minting the same token concurrently is fine — GitHub
	// issues one per request and they coexist — so this holds no lock beyond
	// the map's own and is dropped whole on any write to the row.
	//
	// **It is not in the database.** A cache that were would be a credential
	// at rest with a second expiry to reason about, and would need its own
	// entry in the encrypted-columns registry, in exchange for nothing.
	minted map[string]mintedToken
}

// NewResolver builds the resolver. The database is here for the connection row
// the third source reads; the encryptor opens every stored ciphertext.
func NewResolver(db *database.DB, enc *crypto.Encryptor) *Resolver {
	return &Resolver{
		db:      db,
		enc:     enc,
		clients: map[string]*safefetch.Client{},
		minted:  map[string]mintedToken{},
	}
}

// Forget drops everything cached for one connection.
//
// **Called on every write to the row, which is what makes a disable immediate.**
// The resolver holds no cached `enabled` — that is read live — so this exists
// for the host client and the minted token, and it means an administrator
// disabling a connection in an incident needs no restart and no flush.
func (r *Resolver) Forget(connectionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, connectionID)
	for key := range r.minted {
		if strings.HasPrefix(key, connectionID+"\n") {
			delete(r.minted, key)
		}
	}
}

// mintedToken is one installation token and when it stops being usable.
type mintedToken struct {
	token   string
	expires time.Time
}

// CloneCredential returns what git should authenticate this app's clone with.
//
// **The order is the policy.** A connection wins over the app's own token,
// because an administrator's install-wide decision outranks a token one owner
// pasted, and because the alternative — a stale personal token quietly winning
// over a working connection — is the failure connections exist to end. The
// app's settings say which one is in force.
//
// A returned error is always *Unreadable. There is no failure here that is not
// a credential the platform holds and cannot open.
func (r *Resolver) CloneCredential(ctx context.Context, app *database.App) (Credential, error) {
	if app == nil {
		return Credential{Source: SourceNone}, nil
	}
	if app.GitConnectionID != "" {
		cred, err := r.connectionCredential(ctx, app)
		// A connection that has gone missing — deleted out from under an app
		// the removal refusal should have caught — falls through to whatever
		// the app has of its own rather than failing the deploy on a row that
		// is not there. Everything else is the connection's answer.
		if !errors.Is(err, database.ErrGitConnectionNotFound) {
			return cred, err
		}
		log.Printf("gitconn: %s points at connection %s, which no longer exists", app.Slug, app.GitConnectionID)
	}
	if app.GitToken == "" {
		return Credential{Source: SourceNone}, nil
	}
	token, err := r.enc.DecryptContext(ctx, app.GitToken)
	if err != nil {
		cred := Credential{Source: SourceAppToken}
		return cred, &Unreadable{
			Cred: cred,
			Summary: fmt.Sprintf("The stored access token for %s could not be read — "+
				"re-enter it in the app's settings.", app.RepoURL),
			Err: err,
		}
	}
	return Credential{Token: token, Source: SourceAppToken}, nil
}
