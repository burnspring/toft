package database

import (
	"database/sql"
	"time"
)

// AppToken is an app's outbound credential — see migration 28. The secret is
// never stored, only its SHA-256, which is also why there is no way to
// re-display one. It is not a credential a person handles: it is not shown in
// the UI, not copyable, and not re-mintable. A deploy is how it rotates.
type AppToken struct {
	ID           string `json:"id"`
	AppID        string `json:"app_id"`
	DeploymentID string `json:"deployment_id,omitempty"`
	// Workload names the process this token was minted for — the same name the
	// deploy, the bootstrap claims and the backend use. A token belongs to a
	// workload *start*, so when that workload starts again the previous
	// instance's token is superseded: the process holding it is gone.
	Workload   string     `json:"workload,omitempty"`
	TokenHash  string     `json:"-"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// ResolvedAppToken is what authentication needs in one read: the token, and
// enough of its app to decide whether to accept it.
//
// The app's status comes back with it because a stopped app's token is refused
// — an app a person switched off does nothing, including notify. `sleeping` is
// *not* stopped (ADR §16): a request can wake a slept app and the very next
// thing it does may be to send, so its token stays good.
type ResolvedAppToken struct {
	Token     AppToken
	AppSlug   string
	AppName   string
	AppStatus string
	OwnerID   string
	// AppLocked is whether the app is locked. A lock stops the app, but a
	// stop that failed would leave its token live, so the token's own check
	// reads the lock rather than trusting the status to have followed it.
	AppLocked bool
}

// CreateAppToken records a freshly minted token.
//
// No audit row. This is the platform issuing a workload its own identity as part of
// a deploy that is already recorded, and writing a second row per deploy would
// add noise without adding a fact.
func (db *DB) CreateAppToken(t *AppToken) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := db.conn.Exec(`
		INSERT INTO app_tokens (id, app_id, deployment_id, workload, token_hash, prefix, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.AppID, nullIfEmpty(t.DeploymentID), t.Workload, t.TokenHash, t.Prefix, t.CreatedAt)
	return err
}

// ResolveAppToken looks up a live token by hash, with its app. Returns nil when
// nothing matches — a hash that resolves to nothing is a flat refusal, because
// that one can be an attack.
//
// The workload comes back with it because the token is how a request names
// *which start of the app* is calling, and during a deploy the app row and the
// caller are two different releases: see Server.callerMode, which answers an
// app's own mode from the deployment this workload belongs to rather than from
// a column the flip has not written yet.
//
// Revoked tokens are excluded here rather than reported, for the same reason:
// whoever presents a revoked token learns only that it no longer works, which
// is what "revoked" means.
func (db *DB) ResolveAppToken(hash string) (*ResolvedAppToken, error) {
	var r ResolvedAppToken
	var deploymentID sql.NullString
	err := db.conn.QueryRow(`
		SELECT t.id, t.app_id, t.deployment_id, t.workload, t.token_hash, t.prefix,
		       t.created_at, t.last_used_at,
		       a.slug, a.name, a.status, COALESCE(a.owner_user_id,''), a.locked_at IS NOT NULL
		FROM app_tokens t
		JOIN apps a ON a.id = t.app_id
		WHERE t.token_hash = ? AND t.revoked_at IS NULL`, hash).
		Scan(&r.Token.ID, &r.Token.AppID, &deploymentID, &r.Token.Workload,
			&r.Token.TokenHash, &r.Token.Prefix, &r.Token.CreatedAt, &r.Token.LastUsedAt,
			&r.AppSlug, &r.AppName, &r.AppStatus, &r.OwnerID, &r.AppLocked)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Token.DeploymentID = deploymentID.String
	return &r, nil
}

// TouchAppToken records last-use. Throttled by the caller to once a minute per
// token, as personal keys are: an app's notification must not add a write per call.
func (db *DB) TouchAppToken(id string) error {
	_, err := db.conn.Exec(
		`UPDATE app_tokens SET last_used_at = ? WHERE id = ?`, time.Now().UTC(), id)
	return err
}

// RevokeAppTokens withdraws every live token for an app. Called when the app is
// stopped or deleted — the workload those tokens belong to is gone.
func (db *DB) RevokeAppTokens(appID string) error {
	_, err := db.conn.Exec(
		`UPDATE app_tokens SET revoked_at = ? WHERE app_id = ? AND revoked_at IS NULL`,
		time.Now().UTC(), appID)
	return err
}

// RevokeAppToken withdraws one token — the one minted for a start that never
// came up.
func (db *DB) RevokeAppToken(id string) error {
	_, err := db.conn.Exec(
		`UPDATE app_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UTC(), id)
	return err
}

// SupersedeWorkloadTokens withdraws the tokens of earlier starts of one
// workload, keeping the one just minted.
//
// Scoped to the workload rather than the app, and that is the whole point. A
// workload that starts again — an isolated pod recreated, a slept app woken, a
// crashed one restored — supersedes only *its own* previous instance, whose
// process is gone. The other release running beside it during a deploy keeps
// its token and keeps serving, which is the case RevokeAppTokensExcept exists
// to protect and the reason this is not simply that function.
//
// An empty keepTokenID keeps none of the workload's — a failed start's
// cleanup, the workload having been deleted.
//
// An empty workload matches nothing on purpose: a token minted outside a
// workload start has no predecessor to supersede, and a query that treated ""
// as a wildcard would revoke every legacy row on the first restart.
func (db *DB) SupersedeWorkloadTokens(appID, workload, keepTokenID string) error {
	if workload == "" {
		return nil
	}
	_, err := db.conn.Exec(`
		UPDATE app_tokens SET revoked_at = ?
		WHERE app_id = ? AND workload = ? AND id <> ? AND revoked_at IS NULL`,
		time.Now().UTC(), appID, workload, keepTokenID)
	return err
}

// RevokeAppTokensExceptWorkload withdraws an app's live tokens apart from one
// workload's.
//
// This is what a deploy calls once the new release is up, and the exception is
// the point: the incoming workload's token must survive while the tokens of
// releases that are no longer running must not. Revoking *everything* at mint
// time would break the outgoing process mid-request during a rolling replace,
// which is why this runs after Deploy rather than beside it.
//
// Keyed on the workload rather than a token id because the incoming workload's
// token is not always minted by the deploy: in isolated mode the pod mints its
// own at bootstrap, which has already happened by the time Deploy returns
// ready. One rule covers both — keep this workload's, revoke the rest.
func (db *DB) RevokeAppTokensExceptWorkload(appID, workload string) error {
	if workload == "" {
		return nil
	}
	_, err := db.conn.Exec(`
		UPDATE app_tokens SET revoked_at = ?
		WHERE app_id = ? AND workload <> ? AND revoked_at IS NULL`,
		time.Now().UTC(), appID, workload)
	return err
}
