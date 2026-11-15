package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"toft/internal/auth"
)

var ErrDuplicateRequest = errors.New("duplicate pending request")

// ErrRequestNotPending is the one refusal a decision makes on purpose: the
// request was already decided. A handler maps it to 409 and everything else
// to 500, because a dropped connection rendered as "already decided" is a
// request that can then be approved twice.
var ErrRequestNotPending = errors.New("request not pending")

// ErrAlreadyOwner is the one refusal a transfer makes on purpose.
var ErrAlreadyOwner = errors.New("target is already owner")

// isUniqueViolation reports whether an insert lost a race against a unique
// index. Checked through the driver's error type rather than by matching the
// message, which is localized and version-dependent.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (db *DB) CreateAccessRequest(ctx context.Context, req *AccessRequest, autoDeny bool, autoReason string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Duplicate check for pending requests with same (requester, object, role),
	// serialised per requester and object: two clicks at once each counted
	// none and both inserted, because no unique index covers object requests
	// (security review 2026-11-14, DB-11). Not an index on the pair — the
	// count lets one person hold a pending viewer and a pending editor ask.
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(('x' || substr(md5(?), 1, 16))::bit(64)::bigint)`,
		"access-request:"+req.RequesterID+":"+req.ObjectID); err != nil {
		return err
	}
	var existing int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM access_requests
		WHERE requester_id=? AND request_type=? AND COALESCE(object_id,'')=COALESCE(?,'') AND COALESCE(requested_role,'')=COALESCE(?,'') AND status='pending'
	`, req.RequesterID, req.RequestType, req.ObjectID, req.RequestedRole).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return ErrDuplicateRequest
	}

	req.ID = uuid.New().String()
	req.CreatedAt = time.Now().UTC()
	status := AccessReqStatusPending
	if autoDeny {
		status = AccessReqStatusDenied
		now := time.Now().UTC()
		req.DecidedAt = &now
		req.Reason = autoReason
	}
	req.Status = status

	if _, err := tx.Exec(`
		INSERT INTO access_requests (id, requester_id, request_type, object_id, requested_role, status, approver_id, approver_group_id, decided_at, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, req.ID, nullIfEmpty(req.RequesterID), req.RequestType, nullIfEmpty(req.ObjectID), req.RequestedRole, req.Status, nullIfEmpty(req.ApproverID), nullIfEmpty(req.ApproverGroupID), req.DecidedAt, req.Reason, req.CreatedAt); err != nil {
		// The advisory lock above serialises the count for one requester and
		// object; the partial unique index (migration 23) backs it for
		// promotion requests. Both paths have to produce the same answer, or
		// the second click 500s where the first one 409s.
		if isUniqueViolation(err) {
			return ErrDuplicateRequest
		}
		return err
	}
	// The target is the request, so the app is said explicitly: an object
	// request is part of its app's history, a promotion of nobody's.
	appID := ""
	if req.RequestType == AccessReqTypeObject {
		appID = req.ObjectID
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: req.RequesterID,
		Action: "access_request.create", TargetType: req.RequestType, TargetID: req.ID,
		AppID: appID,
		After: jsonStr(map[string]interface{}{
			"object_id": req.ObjectID, "requested_role": req.RequestedRole, "status": req.Status,
		}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) GetAccessRequest(id string) (*AccessRequest, error) {
	req := &AccessRequest{}
	var objectID, reqRole, approver, reason sql.NullString
	err := db.conn.QueryRow(`
		SELECT id, COALESCE(requester_id,''), request_type, COALESCE(object_id,''), requested_role, status, COALESCE(approver_id,''), COALESCE(approver_group_id,''), decided_at, reason, created_at
		FROM access_requests WHERE id=?
	`, id).Scan(&req.ID, &req.RequesterID, &req.RequestType, &objectID, &reqRole, &req.Status, &approver, &req.ApproverGroupID, &req.DecidedAt, &reason, &req.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	req.ObjectID = objectID.String
	req.RequestedRole = reqRole.String
	req.ApproverID = approver.String
	req.Reason = reason.String
	return req, nil
}

// ListAccessRequestsForObject is the decider's queue for one app, newest first,
// with the people in it resolved — the same struct the requester's own list
// reads, so the two views of one row cannot disagree.
func (db *DB) ListAccessRequestsForObject(objectID string) ([]ObjectAccessRequestDetail, error) {
	return db.scanObjectRequestDetails(
		objectRequestDetailSelect+` AND r.object_id=? ORDER BY r.created_at DESC`, objectID)
}

// ObjectAccessRequestDetail is one person's own request for a grant on an app,
// with the app resolved.
//
// The requester's own list is the twin of ListPromotionRequestsForUser and
// exists for the same reason: without it somebody can create a request and then
// have no way to see it, which is indistinguishable from the button not having
// worked. What it resolves is the app rather than the people — the requester
// knows who they are, and the decider is named only once there is one.
//
// The join is a LEFT JOIN because an app can be deleted while a request against
// it is still on file. The row survives with an empty slug, which is the honest
// answer: the ask happened, and the thing it was about is gone.
type ObjectAccessRequestDetail struct {
	AccessRequest
	AppSlug string `json:"app_slug"`
	AppName string `json:"app_name"`
	// The requester, as their colleagues know them. The decider's queue reads
	// the same struct, and a queue of ids is a queue nobody can act on: the
	// question an owner asks first is who this is.
	RequesterLogin string `json:"requester_login"`
	RequesterName  string `json:"requester_name"`
	// ApproverLogin is empty on a pending or withdrawn request — nobody decided
	// it.
	ApproverLogin string `json:"approver_login"`
}

// DisplayName is what to call the requester: the name their provider asserted,
// falling back to their login rather than to nothing.
func (d ObjectAccessRequestDetail) DisplayName() string {
	if d.RequesterName != "" {
		return d.RequesterName
	}
	return d.RequesterLogin
}

const objectRequestDetailSelect = `
	SELECT r.id, COALESCE(r.requester_id,''), r.request_type, COALESCE(r.object_id,''), r.requested_role,
	       r.status, COALESCE(r.approver_id,''), COALESCE(r.approver_group_id,''), r.decided_at, r.reason, r.created_at,
	       COALESCE(app.slug,''), COALESCE(app.name,''),
	       COALESCE(u.login,''), COALESCE(u.name,''), COALESCE(a.login,'')
	FROM access_requests r
	LEFT JOIN apps app ON app.id = r.object_id
	LEFT JOIN users u ON u.id = r.requester_id
	LEFT JOIN users a ON a.id = r.approver_id
	WHERE r.request_type='object_access'`

// ListObjectAccessRequestsForUser returns one person's own app access requests,
// newest first — pending and decided alike, because "was I turned down, and
// why" is the question asked second.
func (db *DB) ListObjectAccessRequestsForUser(userID string) ([]ObjectAccessRequestDetail, error) {
	return db.scanObjectRequestDetails(
		objectRequestDetailSelect+` AND r.requester_id=? ORDER BY r.created_at DESC`, userID)
}

// PendingObjectAccessRequest returns this person's open request against one
// app, or nil when there is none.
//
// It exists so the app detail page resolves in one request: the permissions
// payload carries the request itself rather than a boolean, and the gate does
// not flash between "you may ask" and "you already did".
func (db *DB) PendingObjectAccessRequest(userID, objectID string) (*ObjectAccessRequestDetail, error) {
	out, err := db.scanObjectRequestDetails(
		objectRequestDetailSelect+` AND r.requester_id=? AND r.object_id=? AND r.status='pending'
		 ORDER BY r.created_at DESC`, userID, objectID)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

func (db *DB) scanObjectRequestDetails(query string, args ...interface{}) ([]ObjectAccessRequestDetail, error) {
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectAccessRequestDetail
	for rows.Next() {
		var d ObjectAccessRequestDetail
		if err := rows.Scan(&d.ID, &d.RequesterID, &d.RequestType, &d.ObjectID, &d.RequestedRole,
			&d.Status, &d.ApproverID, &d.ApproverGroupID, &d.DecidedAt, &d.Reason, &d.CreatedAt,
			&d.AppSlug, &d.AppName,
			&d.RequesterLogin, &d.RequesterName, &d.ApproverLogin); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PromotionRequestDetail is a promotion request with the people in it resolved.
//
// The bare row carries only ids, which is useless to both readers: an
// administrator deciding on a request needs to know who is asking, and the
// notifier needs an address to send to. Resolving them in the query rather than
// in N follow-up reads also keeps the queue page one round trip.
type PromotionRequestDetail struct {
	AccessRequest
	RequesterLogin string `json:"requester_login"`
	RequesterName  string `json:"requester_name"`
	RequesterEmail string `json:"requester_email"`
	// ApproverLogin is empty on a pending or withdrawn request — nobody
	// decided it.
	ApproverLogin string `json:"approver_login"`
}

// DisplayName is what to call the requester: the name their provider asserted,
// falling back to their login rather than to nothing.
func (d PromotionRequestDetail) DisplayName() string {
	if d.RequesterName != "" {
		return d.RequesterName
	}
	return d.RequesterLogin
}

const promotionDetailSelect = `
	SELECT r.id, COALESCE(r.requester_id,''), r.request_type, COALESCE(r.object_id,''), r.requested_role,
	       r.status, COALESCE(r.approver_id,''), r.decided_at, r.reason, r.created_at,
	       COALESCE(u.login,''), COALESCE(u.name,''), COALESCE(u.email,''), COALESCE(a.login,'')
	FROM access_requests r
	LEFT JOIN users u ON u.id = r.requester_id
	LEFT JOIN users a ON a.id = r.approver_id
	WHERE r.request_type='role_promotion'`

// AccessReqStatusAll is the sentinel for "do not filter by status".
const AccessReqStatusAll = "all"

// ListPromotionRequests returns promotion requests in a status, newest first.
//
// Pending is the working queue; the rest is history, and history is why the
// decided rows are readable at all — "did anyone already turn this person
// down, and why" is the question an administrator asks second.
func (db *DB) ListPromotionRequests(status string) ([]PromotionRequestDetail, error) {
	query := promotionDetailSelect
	var args []interface{}
	if status != "" && status != AccessReqStatusAll {
		query += ` AND r.status=?`
		args = append(args, status)
	}
	query += ` ORDER BY r.created_at DESC`
	return db.scanPromotionDetails(query, args...)
}

// ListPromotionRequestsForUser returns one person's own requests, newest first,
// so a requester can see that the thing they asked for is still waiting rather
// than wondering whether the button worked.
func (db *DB) ListPromotionRequestsForUser(userID string) ([]PromotionRequestDetail, error) {
	return db.scanPromotionDetails(promotionDetailSelect+` AND r.requester_id=? ORDER BY r.created_at DESC`, userID)
}

// GetPromotionRequestDetail returns one request with its people resolved, or
// nil when there is no such promotion request.
func (db *DB) GetPromotionRequestDetail(reqID string) (*PromotionRequestDetail, error) {
	out, err := db.scanPromotionDetails(promotionDetailSelect+` AND r.id=?`, reqID)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

func (db *DB) scanPromotionDetails(query string, args ...interface{}) ([]PromotionRequestDetail, error) {
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PromotionRequestDetail
	for rows.Next() {
		var d PromotionRequestDetail
		if err := rows.Scan(&d.ID, &d.RequesterID, &d.RequestType, &d.ObjectID, &d.RequestedRole,
			&d.Status, &d.ApproverID, &d.DecidedAt, &d.Reason, &d.CreatedAt,
			&d.RequesterLogin, &d.RequesterName, &d.RequesterEmail, &d.ApproverLogin); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ApproveObjectAccessRequest sets the request to approved and creates the grant
// atomically, and reports the role the requester ends up holding.
//
// objectID is the app the caller was authorized against, and the request must
// name that same app. Without it the caller's authorization and the object being
// granted are two different things: the handler checks the app in the URL while
// this function grants against whatever object_id the request row happens to
// carry, so any owner could approve a request pointing at someone else's app and
// have the grant written there.
//
// The role it returns is the one that is now in `object_grants`, which is not
// always the one that was asked for — see the standing-grant read below. It is
// returned rather than re-derived by the caller because the notification says
// what somebody may now do, and a message naming a role the row does not carry
// is worse than no message.
func (db *DB) ApproveObjectAccessRequest(ctx context.Context, reqID, objectID, approverID string, meta RequestMeta) (string, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// Claimed first, with the same `AND status='pending'` deny and withdraw
	// carry: read then written by id alone, a withdrawal or a denial that
	// committed in between was overwritten and the grant written anyway
	// (security review 2026-11-14, DB-6).
	now := time.Now().UTC()
	var requesterID, role string
	err = tx.QueryRow(`
		UPDATE access_requests SET status='approved', approver_id=?, decided_at=?
		WHERE id=? AND request_type='object_access' AND object_id=? AND status=?
		RETURNING COALESCE(requester_id,''), requested_role`,
		nullIfEmpty(approverID), now, reqID, objectID, AccessReqStatusPending).Scan(&requesterID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM access_requests WHERE id=? AND request_type='object_access' AND object_id=?)`,
			reqID, objectID).Scan(&exists); err != nil {
			return "", err
		}
		if exists {
			return "", ErrRequestNotPending
		}
		return "", sql.ErrNoRows
	}
	if err != nil {
		return "", err
	}

	// What they already hold, because **approving may never take access away.**
	// A request is asked for once and decided whenever somebody gets to it: an
	// owner who makes the requester an editor from the Sharing tab and then
	// clears the stale pending `viewer` row weeks later would otherwise demote
	// them, in a transaction the audit log records as a grant. Nobody reads
	// "approved" as "reduced".
	//
	// So the ask is a floor and never a ceiling, resolved through
	// auth.RolePrecedence rather than a second ladder over here. And the write
	// is raise-only against the row as it is *now*, locked: the standing role
	// read without a lock and then upserted over could lower a grant raised —
	// or an ownership transfer's `owner` row written — in the same instant.
	granted := role
	res, err := tx.Exec(`
		INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by, granted_at)
		VALUES (?, 'user', ?, ?, ?, ?)
		ON CONFLICT(object_id, principal_type, principal_id) DO NOTHING`,
		objectID, requesterID, role, nullIfEmpty(approverID), now)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var standing string
		if err := tx.QueryRow(`SELECT role FROM object_grants WHERE object_id=? AND principal_type='user' AND principal_id=? FOR UPDATE`,
			objectID, requesterID).Scan(&standing); err != nil {
			return "", err
		}
		if auth.RolePrecedence(role) > auth.RolePrecedence(standing) {
			if _, err := tx.Exec(`UPDATE object_grants SET role=?, granted_by=?, granted_at=?
				WHERE object_id=? AND principal_type='user' AND principal_id=?`,
				role, nullIfEmpty(approverID), now, objectID, requesterID); err != nil {
				return "", err
			}
		} else {
			// A re-assertion at the same role or less: the row and its stamp
			// stay as they are, and the audit row records the role it holds.
			granted = standing
		}
	}
	// `role` is what the grant row now says, always — the same key it has
	// always carried, and the one an administrator reading the log is asking
	// about. `requested_role` appears only when the two differ, which is the
	// whole of what this record has to be able to say that it could not before:
	// the ask was for less than this person already had, and nothing moved.
	after := map[string]string{"request_id": reqID, "requester_id": requesterID, "role": granted}
	if granted != role {
		after["requested_role"] = role
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: approverID,
		Action: "access_request.approve", TargetType: "app", TargetID: objectID,
		After:     jsonStr(after),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return granted, nil
}

// DenyObjectAccessRequest denies a request for access to a specific app. As with
// approval, the request must belong to the app the caller was authorized
// against — otherwise any app owner can deny any pending request on any app.
func (db *DB) DenyObjectAccessRequest(ctx context.Context, reqID, objectID, approverID, reason string, meta RequestMeta) error {
	return db.denyAccessRequest(ctx, reqID, approverID, reason, meta,
		`AND request_type=? AND object_id=?`, AccessReqTypeObject, objectID)
}

// DenyPromotionRequest denies a system-role promotion request. Promotion
// requests carry no object, and this endpoint is admin-only; the request_type
// filter keeps the two deny endpoints from reaching each other's rows.
func (db *DB) DenyPromotionRequest(ctx context.Context, reqID, approverID, reason string, meta RequestMeta) error {
	return db.denyAccessRequest(ctx, reqID, approverID, reason, meta,
		`AND request_type=?`, AccessReqTypePromotion)
}

func (db *DB) denyAccessRequest(ctx context.Context, reqID, approverID, reason string, meta RequestMeta, scope string, scopeArgs ...interface{}) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	args := append([]interface{}{approverID, time.Now().UTC(), reason, reqID}, scopeArgs...)
	appID, err := decidedRequestApp(tx.QueryRow(`UPDATE access_requests SET status='denied', approver_id=?, decided_at=?, reason=? WHERE id=? AND status='pending' `+scope+
		` RETURNING request_type, COALESCE(object_id, '')`, args...))
	if err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: approverID,
		Action: "access_request.deny", TargetType: "access_request", TargetID: reqID,
		AppID:     appID,
		After:     jsonStr(map[string]string{"reason": reason}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) WithdrawAccessRequest(ctx context.Context, reqID, requesterID string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	appID, err := decidedRequestApp(tx.QueryRow(`UPDATE access_requests SET status='withdrawn', decided_at=? WHERE id=? AND requester_id=? AND status='pending'
		RETURNING request_type, COALESCE(object_id, '')`, time.Now().UTC(), reqID, requesterID))
	if err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: requesterID,
		Action: "access_request.withdraw", TargetType: "access_request", TargetID: reqID,
		AppID:     appID,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ApprovePromotionRequest promotes the requester to publisher atomically.
//
// Both writes are guarded, because the read they follow can be stale: an
// administrator promoting the same person through PATCH /users/{id}/role
// resolves this request in a transaction that may commit after ours began.
// The request is claimed with `AND status='pending'`, which waits on that
// transaction's row lock and then finds the row no longer pending; and the
// role is written only over `viewer`, so an approval never lowers anyone —
// the outcome was an administrator demoted to publisher without the
// last-administrator predicate ever being asked (DB1-9).
func (db *DB) ApprovePromotionRequest(ctx context.Context, reqID, approverID string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var requesterID string
	err = tx.QueryRow(`UPDATE access_requests SET status='approved', approver_id=?, decided_at=?
		WHERE id=? AND request_type='role_promotion' AND status=?
		RETURNING COALESCE(requester_id,'')`,
		nullIfEmpty(approverID), now, reqID, AccessReqStatusPending).Scan(&requesterID)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM access_requests WHERE id=? AND request_type='role_promotion')`,
			reqID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrRequestNotPending
		}
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	// The token_version bump is what makes the promotion take effect. A JWT
	// carries the role it was minted with, so without this the newly-promoted
	// user keeps a viewer session until it expires — they are told they may
	// publish and then refused when they try. Bumped in this transaction rather
	// than by a separate call so the role and the sessions that may use it can
	// never disagree. The cost is that approval signs the requester out; the
	// approval email says so.
	res, err := tx.Exec(`UPDATE users SET system_role=?, token_version = token_version + 1 WHERE id=? AND system_role=?`,
		SystemRolePublisher, requesterID, SystemRoleViewer)
	if err != nil {
		return err
	}
	after := map[string]string{"system_role": SystemRolePublisher, "via_request": reqID, "sessions": "invalidated"}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already publisher or above by another door: the request is answered,
		// and the role and the sessions are left as they are.
		after = map[string]string{"system_role": "unchanged", "via_request": reqID}
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: approverID,
		Action: "user.role_change", TargetType: "user", TargetID: requesterID,
		After:     jsonStr(after),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// TransferOwnership moves an app to a new owning principal — a user or a group
// — reassigning apps.owner_user_id/owner_group_id and the owner grant row
// together, demoting the previous owner principal to editor, and
// re-routing pending access requests. Atomic.
//
// The owner grant row is why this stays one function rather than two. Ownership
// is denormalised: it is a column on apps *and* a row in object_grants, and
// those two must never disagree — a group-owned app is readable by its members
// only through that grant row. Everything that writes either one writes both,
// in one transaction, here.
//
// `newOwnerType` is PrincipalTypeUser or PrincipalTypeGroup. Eligibility of the
// target — a user who is neither locked nor a system viewer, a group that
// exists and has at least one member who is neither — is the caller's to check;
// this is the write, not the policy.
//
// **Storage declared in toft.toml moves with the owner, or the transfer is
// refused** (ADR § Storage somebody else can attach): each manifest mapping's
// stamp is re-pointed at the new owner's delegation in this transaction, and
// a resource the new owner cannot attach at the mapped level refuses the whole
// transfer with *StorageNotAttachableByNewOwnerError. Console mappings keep
// their stamps.
func (db *DB) TransferOwnership(ctx context.Context, appID, newOwnerType, newOwnerID, actorID string, meta RequestMeta) error {
	return db.TransferOwnershipWith(ctx, appID, newOwnerType, newOwnerID, actorID, meta, TransferOptions{})
}

// TransferOptions are a transfer's choices beyond the new owner.
type TransferOptions struct {
	// KeepStorageAsAdmin stamps a manifest mapping the new owner cannot attach
	// `admin` instead of refusing — an administrator's write adopting it — so
	// an offboarding transfer does not stall on a missing delegation.
	KeepStorageAsAdmin bool
}

// TransferOwnershipWith is TransferOwnership with its options.
func (db *DB) TransferOwnershipWith(ctx context.Context, appID, newOwnerType, newOwnerID, actorID string, meta RequestMeta, opts TransferOptions) error {
	if newOwnerType != PrincipalTypeUser && newOwnerType != PrincipalTypeGroup {
		return fmt.Errorf("unknown owner principal type %q", newOwnerType)
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var oldUser, oldGroup sql.NullString
	if err := tx.QueryRow(`SELECT owner_user_id, owner_group_id FROM apps WHERE id=? FOR UPDATE`, appID).Scan(&oldUser, &oldGroup); err != nil {
		return err
	}
	oldType, oldID := PrincipalTypeUser, oldUser.String
	if oldGroup.String != "" {
		oldType, oldID = PrincipalTypeGroup, oldGroup.String
	}
	if oldType == newOwnerType && oldID == newOwnerID {
		return ErrAlreadyOwner
	}

	now := time.Now().UTC()

	// Exactly one column set, always. Writing both in one statement is what
	// keeps apps_one_owner satisfied at every instant rather than only at
	// commit — the CHECK is not deferrable, so clearing one and setting the
	// other in two statements would fail on the first.
	var newUser, newGroup interface{}
	if newOwnerType == PrincipalTypeGroup {
		newGroup = newOwnerID
	} else {
		newUser = newOwnerID
	}
	if _, err := tx.Exec(`UPDATE apps SET owner_user_id=?, owner_group_id=?, updated_at=? WHERE id=?`,
		newUser, newGroup, now, appID); err != nil {
		return err
	}

	// Remove the old owner's grant, then re-add it as editor below. Two
	// steps rather than one UPDATE because the old and new principal may be the
	// same row when only the type changes.
	if _, err := tx.Exec(`DELETE FROM object_grants WHERE object_id=? AND principal_type=? AND principal_id=?`, appID, oldType, oldID); err != nil {
		return err
	}
	// granted_by names a person, so it is the actor here — never the new owner,
	// which for a group is not a person at all.
	if _, err := tx.Exec(`
		INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by, granted_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(object_id, principal_type, principal_id) DO UPDATE SET
			-- The stamp moves only when the role does: a re-save at the same
			-- role is a re-assertion the audit row records, not a new grant.
			granted_by = CASE WHEN object_grants.role = excluded.role THEN object_grants.granted_by ELSE excluded.granted_by END,
			granted_at = CASE WHEN object_grants.role = excluded.role THEN object_grants.granted_at ELSE excluded.granted_at END,
			role = excluded.role
	`, appID, newOwnerType, newOwnerID, ObjectRoleOwner, nullIfEmpty(actorID), now); err != nil {
		return err
	}
	// Demote the previous owner principal to editor — unless there wasn't
	// one, which a row written outside CreateApp can produce.
	if oldID != "" {
		if _, err := tx.Exec(`
			INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by, granted_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(object_id, principal_type, principal_id) DO UPDATE SET
			-- The stamp moves only when the role does: a re-save at the same
			-- role is a re-assertion the audit row records, not a new grant.
			granted_by = CASE WHEN object_grants.role = excluded.role THEN object_grants.granted_by ELSE excluded.granted_by END,
			granted_at = CASE WHEN object_grants.role = excluded.role THEN object_grants.granted_at ELSE excluded.granted_at END,
			role = excluded.role
		`, appID, oldType, oldID, ObjectRoleEditor, nullIfEmpty(actorID), now); err != nil {
			return err
		}
	}

	// Re-route pending object access requests to whoever now decides them. One
	// column or the other, never both: approver_group_id says "any member of
	// this group may decide", and approver_id is left NULL until one of them
	// actually does — it records who decided, not who was asked.
	var routeGroup interface{}
	if newOwnerType == PrincipalTypeGroup {
		routeGroup = newOwnerID
	}
	if _, err := tx.Exec(`
		UPDATE access_requests SET approver_id=NULL, approver_group_id=?
		WHERE object_id=? AND status='pending'`, routeGroup, appID); err != nil {
		return err
	}

	if err := restampManifestStorageTx(tx, appID, newOwnerType, newOwnerID, actorID, meta, opts); err != nil {
		return err
	}

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "app.transfer_ownership", TargetType: "app", TargetID: appID,
		Before: jsonStr(map[string]string{"owner_type": oldType, "owner_id": oldID}),
		After: jsonStr(map[string]string{
			"owner_type": newOwnerType, "owner_id": newOwnerID,
			"previous_owner_demoted_to": ObjectRoleEditor,
		}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolvePendingPromotionTx closes out a pending publisher request for a user
// whose role has just been set directly.
//
// Two doors reach one outcome: the queue on /admin/publisher-requests, and the
// role `<Select>` on /people and /people/:id. Only the first knew about the
// other, so promoting somebody from the directory left their request sitting
// pending for a role they already held — and an administrator working the queue
// then approved a promotion that had already happened.
//
// Which resolution depends on what the administrator did, not on what was
// asked: a role that grants publishing **approves** the request, and one that
// does not **denies** it. There is no fifth status for "settled elsewhere"; the
// audit row is what records that this was decided from the directory, and the
// approver is the administrator who acted.
//
// Silent when there is nothing pending, which is the ordinary case.
func ResolvePendingPromotionTx(tx *Tx, targetID, approverID, newRole string, meta RequestMeta) error {
	status, verb := AccessReqStatusDenied, "deny"
	if newRole == SystemRoleAdmin || newRole == SystemRolePublisher {
		status, verb = AccessReqStatusApproved, "approve"
	}
	var reqID string
	err := tx.QueryRow(
		`SELECT id FROM access_requests
		  WHERE requester_id=? AND request_type='role_promotion' AND status='pending'
		  ORDER BY created_at DESC LIMIT 1`, targetID).Scan(&reqID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Claimed, not just written: a decision from the queue that committed
	// after the read above is not overwritten, and gets no second audit row.
	res, err := tx.Exec(
		`UPDATE access_requests SET status=?, approver_id=?, decided_at=? WHERE id=? AND status='pending'`,
		status, nullIfEmpty(approverID), time.Now().UTC(), reqID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return nil
	}
	return writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: approverID,
		// The existing vocabulary, not a fifth verb — this is the same
		// decision reached through the other door.
		Action: "access_request." + verb, TargetType: "access_request", TargetID: reqID,
		After: jsonStr(map[string]string{
			"via": "role_change", "system_role": newRole,
		}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	})
}

// decidedRequestApp reads what a deciding UPDATE … RETURNING request_type,
// object_id gave back: the app an object request was about, "" for a
// promotion, and sql.ErrNoRows when no pending request matched.
func decidedRequestApp(row *sql.Row) (string, error) {
	var reqType, objectID string
	if err := row.Scan(&reqType, &objectID); err != nil {
		return "", err
	}
	if reqType == AccessReqTypeObject {
		return objectID, nil
	}
	return "", nil
}
