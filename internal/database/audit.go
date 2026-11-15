package database

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"toft/internal/textutil"
)

type AuditEntry struct {
	ActorType  string
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Before     string
	After      string
	OccurredAt time.Time
	RequestID  string
	IP         string
	UserAgent  string
	// APIKeyID names the credential that acted, when it was not a browser
	// session. Empty means interactive. Without it, "alice deleted this app"
	// reads the same whether alice did it or a token she minted last year did
	// — and that is the sentence an incident review turns on.
	APIKeyID string
	// AppID is the app this row is part of the history of. Left empty it is
	// derived — the target when the target is an app, else the actor when an
	// app acted — which covers every app-scoped writer but the access-request
	// ones, whose target is the request. audit_scope.go says which actions
	// must carry one, and every database test checks it at cleanup.
	AppID string
}

// auditAppID is the app a row belongs to: what the writer said, else what the
// row already says.
func auditAppID(e AuditEntry) string {
	switch {
	case e.AppID != "":
		return e.AppID
	case e.TargetType == "app":
		return e.TargetID
	case e.ActorType == PrincipalTypeApp:
		return e.ActorID
	}
	return ""
}

// WriteAuditEntry inserts an audit log row using the provided transaction.
// The transaction is always used — this function never opens its own tx.
func (db *DB) WriteAuditEntry(ctx context.Context, tx *Tx, e AuditEntry) error {
	return writeAuditTx(tx, e)
}

// WriteAuditTx is the package-level equivalent of WriteAuditEntry — exposed for
// callers that have a *Tx from db.Conn().Begin().
func WriteAuditTx(tx *Tx, e AuditEntry) error {
	return writeAuditTx(tx, e)
}

func writeAuditTx(tx *Tx, e AuditEntry) error {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	// The floor for every caller: request-supplied text that is not valid
	// UTF-8, or holds a NUL, fails the INSERT — and for the rows written after
	// the fact the action then succeeds with no record (LB-7).
	e.UserAgent = textutil.Clip(e.UserAgent, 512)
	e.TargetID = textutil.Clean(e.TargetID)
	e.RequestID = textutil.Clean(e.RequestID)
	e.IP = textutil.Clean(e.IP)
	id := uuid.New().String()
	_, err := tx.Exec(`
		INSERT INTO audit_log (id, actor_type, actor_id, action, target_type, target_id, before, after, occurred_at, request_id, ip, user_agent, api_key_id, app_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, e.ActorType, nullIfEmpty(e.ActorID), e.Action, e.TargetType, e.TargetID, nullIfEmpty(e.Before), nullIfEmpty(e.After), e.OccurredAt, e.RequestID, e.IP, e.UserAgent, nullIfEmpty(e.APIKeyID), auditAppID(e))
	return err
}

// CountAuditEntries is a test helper — count rows in the audit_log table.
func (db *DB) CountAuditEntries() (int, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n)
	return n, err
}

// RequestMetadata is a key set for retrieving request-scoped metadata from context.
type requestMetaKeyType struct{}

var requestMetaKey = requestMetaKeyType{}

type RequestMeta struct {
	RequestID string
	IP        string
	UserAgent string
	// APIKeyID is the credential that made the request, empty for a browser
	// session. It rides with the request metadata rather than being looked up
	// at each audit site so that "which token did this" is answered by the
	// same plumbing that already answers "from which IP".
	APIKeyID string
}

func WithRequestMeta(ctx context.Context, m RequestMeta) context.Context {
	return context.WithValue(ctx, requestMetaKey, m)
}

func GetRequestMeta(ctx context.Context) RequestMeta {
	if v, ok := ctx.Value(requestMetaKey).(RequestMeta); ok {
		return v
	}
	return RequestMeta{}
}

func jsonStr(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// execAudited runs one statement and, when an audit entry is given, writes it in
// the same transaction — so a setter can offer "and record it" to the handler
// that knows who acted without the change and its record being able to part.
// With no entry it is a plain Exec, which is what the tests' fixtures want.
func (db *DB) execAudited(audit []AuditEntry, query string, args ...any) error {
	if len(audit) == 0 {
		_, err := db.conn.Exec(query, args...)
		return err
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(query, args...); err != nil {
		return err
	}
	for _, e := range audit {
		if err := writeAuditTx(tx, e); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WriteAuditRow writes one audit entry outside a transaction.
//
// Every *authorization-relevant* mutation still writes its audit entry inside
// the transaction that made the change — that rule is unchanged and is what
// stops the record for a disputed change going missing. This is for the other
// kind: an action already committed whose audit trail is valuable but whose
// failure must not undo it. Refusing to create a job because the audit write
// failed helps nobody.
func (db *DB) WriteAuditRow(e AuditEntry) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeAuditTx(tx, e); err != nil {
		return err
	}
	return tx.Commit()
}
