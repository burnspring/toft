package database

import (
	"database/sql"
	"errors"
	"time"
)

// The ten minutes between "a machine asked to sign in" and "a person said yes".
//
// Every row is scaffolding for a credential that lives elsewhere: what the flow
// produces is an ordinary user_api_keys row. Nothing here survives the answer,
// and nothing here is a secret — the device code is stored as a hash, and the
// minted key's plaintext is never written at all. See 00046_cli_device_codes.sql
// for why approval and minting are two separate moments.

// DeviceCode is one pending `toft login`.
//
// The device code itself is absent from this struct by construction: the server
// holds the hash, the client holds the code, and nothing in between ever needs
// both. What the approval screen shows is the two display fields and the clock.
type DeviceCode struct {
	UserCode      string `json:"user_code"`
	Hostname      string `json:"hostname"`
	ClientVersion string `json:"client_version"`
	// Client is which program asked — DeviceClientLP or DeviceClientRSConnect
	// — and RequestedFrom the address it asked from. Both are display: what is
	// minted is the same personal key either way.
	Client        string     `json:"client"`
	RequestedFrom string     `json:"requested_from"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	LastPolledAt  *time.Time `json:"-"`

	ApprovedUserID string     `json:"-"`
	ApprovedAt     *time.Time `json:"-"`
	DeniedAt       *time.Time `json:"-"`
}

// Pending reports whether this request is still waiting on a human, as of now.
// An expired request is not pending — it is over, and the difference is what a
// polling client is told.
func (d *DeviceCode) Pending(now time.Time) bool {
	return d.ApprovedAt == nil && d.DeniedAt == nil && d.ExpiresAt.After(now)
}

// The two programs a device code can belong to.
const (
	DeviceClientLP        = "toft"
	DeviceClientRSConnect = "rsconnect"
)

const deviceCodeColumns = `user_code, hostname, client_version, client, requested_from,
	created_at, expires_at, last_polled_at, COALESCE(approved_user_id, ''), approved_at, denied_at`

func scanDeviceCode(row interface{ Scan(...any) error }) (*DeviceCode, error) {
	var d DeviceCode
	err := row.Scan(&d.UserCode, &d.Hostname, &d.ClientVersion, &d.Client, &d.RequestedFrom, &d.CreatedAt,
		&d.ExpiresAt, &d.LastPolledAt, &d.ApprovedUserID, &d.ApprovedAt, &d.DeniedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ErrTooManyPendingCodes refuses a new sign-in request while too many are
// already waiting — from one source, or on the install as a whole.
var ErrTooManyPendingCodes = errors.New("too many sign-in requests are already waiting")

// The two bounds on pending requests. They are concurrency rules, not rate
// limits (ADR §75): a pending request is a row held for ten minutes, and what
// they bound is how many one source — and everyone together — may hold at
// once. Twenty is far more than one office behind one address starts in ten
// minutes; the install-wide bound is what keeps a distributed caller from
// growing the table without end.
const (
	maxPendingCodesPerSource = 20
	maxPendingCodes          = 5000
)

// pendingCodesFull answers ErrTooManyPendingCodes when either bound is reached.
func pendingCodesFull(q interface {
	QueryRow(string, ...any) *sql.Row
}, source string, now time.Time) error {
	var fromSource, total int
	if err := q.QueryRow(`
		SELECT COUNT(*) FILTER (WHERE requested_from = ?), COUNT(*)
		FROM cli_device_codes WHERE expires_at >= ? AND approved_at IS NULL`,
		source, now).Scan(&fromSource, &total); err != nil {
		return err
	}
	if fromSource >= maxPendingCodesPerSource || total >= maxPendingCodes {
		return ErrTooManyPendingCodes
	}
	return nil
}

// CreateDeviceCode records a new pending request and sweeps whatever has
// expired.
//
// The sweep rides here because this is the only moment anything touches the
// table often enough to keep it small, and a background job for a table that
// holds tens of rows would be machinery with nothing to do. It deletes only
// what is past its expiry — never a live request, and never one that was
// approved seconds ago and is about to be collected.
func (db *DB) CreateDeviceCode(d *DeviceCode, codeHash string) error {
	now := time.Now().UTC()
	if _, err := db.Conn().Exec(
		`DELETE FROM cli_device_codes WHERE expires_at < ?`, now); err != nil {
		return err
	}
	if err := pendingCodesFull(db.Conn(), d.RequestedFrom, now); err != nil {
		return err
	}
	_, err := db.Conn().Exec(`
		INSERT INTO cli_device_codes
			(device_code_hash, user_code, hostname, client_version, client, requested_from, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		codeHash, d.UserCode, d.Hostname, d.ClientVersion, clientOrLP(d.Client), d.RequestedFrom, d.CreatedAt, d.ExpiresAt)
	return err
}

// GetDeviceCodeByUserCode is what the approval screen reads: the request a
// person is being asked to approve, found by the characters on their terminal.
func (db *DB) GetDeviceCodeByUserCode(userCode string) (*DeviceCode, error) {
	d, err := scanDeviceCode(db.Conn().QueryRow(
		`SELECT `+deviceCodeColumns+` FROM cli_device_codes WHERE user_code = ?`, userCode))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

// DecideDeviceCode records an approval or a denial, with the audit row, in one
// transaction — minting a credential is an access grant and must never happen
// unrecorded, and this is the moment the human made the decision.
//
// **The UPDATE is guarded on the request still being pending.** Two browser tabs
// on the same code, or a double-click, must not turn one decision into two: the
// second finds no row to update and is told the request is no longer pending,
// which is true.
func (db *DB) DecideDeviceCode(userCode, userID string, approve bool, audit AuditEntry) (bool, error) {
	tx, err := db.Conn().Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	var res sql.Result
	if approve {
		res, err = tx.Exec(`
			UPDATE cli_device_codes
			   SET approved_user_id = ?, approved_at = ?
			 WHERE user_code = ? AND approved_at IS NULL AND denied_at IS NULL
			   AND expires_at > ?`, userID, now, userCode, now)
	} else {
		res, err = tx.Exec(`
			UPDATE cli_device_codes
			   SET denied_at = ?
			 WHERE user_code = ? AND approved_at IS NULL AND denied_at IS NULL
			   AND expires_at > ?`, now, userCode, now)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// PollDeviceCode reads a request by the code the client holds.
func (db *DB) PollDeviceCode(codeHash string) (*DeviceCode, error) {
	d, err := scanDeviceCode(db.Conn().QueryRow(
		`SELECT `+deviceCodeColumns+` FROM cli_device_codes WHERE device_code_hash = ?`, codeHash))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

// TouchDeviceCode stamps a poll that was **served**, so the next one can be told
// to slow down.
//
// Separate from the read, and called only when the caller was answered, because
// stamping a refused poll would move the window forward every time: a client
// polling faster than the interval would then be told to slow down forever and
// never get its key, however long it eventually waited. Back-off should bound
// the rate, not lock the door.
//
// The stamp is a write on a polling path, which is ordinarily the thing this
// codebase refuses to do. It is one UPDATE on a primary key, on a table with
// tens of rows, at most once every few seconds per pending login — and the
// alternative is an in-memory counter that would forget every restart and
// disagree between replicas.
func (db *DB) TouchDeviceCode(codeHash string) error {
	_, err := db.Conn().Exec(
		`UPDATE cli_device_codes SET last_polled_at = ? WHERE device_code_hash = ?`,
		time.Now().UTC(), codeHash)
	return err
}

// ClaimDeviceCode mints the approved key and destroys the request, atomically.
//
// This is where single-use is enforced, and it is a claim rather than a read
// because two polls can arrive at once: the DELETE either removes the row or
// finds nothing, and only the caller that removed it gets a key. A second poll
// then finds no request at all, which is the honest answer — the credential it
// would have collected has been collected.
//
// The audit row goes in the same transaction as the key, like every other mint.
func (db *DB) ClaimDeviceCode(codeHash string, k *UserAPIKey, audit AuditEntry) (bool, error) {
	tx, err := db.Conn().Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// Approved *and* still inside its ten minutes. An approval collected a day
	// later would be a credential minted for a person who has long since
	// stopped looking; the ten minutes bound the approval, not just the wait.
	res, err := tx.Exec(`
		DELETE FROM cli_device_codes
		 WHERE device_code_hash = ? AND approved_at IS NOT NULL AND expires_at > ?`,
		codeHash, time.Now().UTC())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}

	// **The scope column is absent here on purpose.** `toft login` always mints a
	// full-scope key: it is a write client whose main verbs would 403 under
	// `read`, and a developer signing a laptop in wants their own reach. The
	// insert names its columns, so the column's DEFAULT 'full' is what makes
	// that true without this file knowing the feature exists — and the one path
	// by which a narrow key reaches the CLI stays open, because
	// `toft login --token-stdin` takes a key minted in the browser.
	if _, err := tx.Exec(`
		INSERT INTO user_api_keys (id, user_id, name, key_hash, key_prefix, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.UserID, k.Name, k.KeyHash, k.KeyPrefix, k.CreatedAt, k.ExpiresAt); err != nil {
		return false, err
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// DeleteDeviceCode drops a request that will never be collected — the one a
// poll finds denied or expired. Left undone it would simply be swept later;
// doing it here means a denied login stops being pollable immediately.
func (db *DB) DeleteDeviceCode(codeHash string) error {
	_, err := db.Conn().Exec(
		`DELETE FROM cli_device_codes WHERE device_code_hash = ?`, codeHash)
	return err
}

// clientOrLP is the column's own default, for a caller that named no client.
func clientOrLP(c string) string {
	if c == "" {
		return DeviceClientLP
	}
	return c
}
