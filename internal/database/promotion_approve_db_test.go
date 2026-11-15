package database_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// DB1-9: the approval wrote system_role='publisher' and status='approved'
// with no guard, after a plain read. An administrator who promoted the same
// person by another door meanwhile was overwritten — demoted — and an
// answered request answered twice. Both writes are guarded now.
func TestApprovingAPromotionNeverLowersARole(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	requester := mkUser(t, db, "promo-requester", database.SystemRoleViewer, time.Now(), nil, nil, false)

	req := &database.AccessRequest{RequesterID: requester, RequestType: database.AccessReqTypePromotion}
	if err := db.CreateAccessRequest(ctx, req, false, "", database.RequestMeta{}); err != nil {
		t.Fatalf("CreateAccessRequest: %v", err)
	}
	// What a concurrent PATCH /users/{id}/role leaves behind, seen from a
	// transaction that read the request while it was still pending.
	if _, err := db.Conn().Exec(`UPDATE users SET system_role=? WHERE id=?`, database.SystemRoleAdmin, requester); err != nil {
		t.Fatal(err)
	}

	if err := db.ApprovePromotionRequest(ctx, req.ID, "builtin-admin", database.RequestMeta{}); err != nil {
		t.Fatalf("ApprovePromotionRequest: %v", err)
	}
	var role string
	if err := db.Conn().QueryRow(`SELECT system_role FROM users WHERE id=?`, requester).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != database.SystemRoleAdmin {
		t.Fatalf("approval wrote %q over system_admin", role)
	}

	if err := db.ApprovePromotionRequest(ctx, req.ID, "builtin-admin", database.RequestMeta{}); !errors.Is(err, database.ErrRequestNotPending) {
		t.Errorf("approving an answered request = %v, want ErrRequestNotPending", err)
	}
	if err := db.ApprovePromotionRequest(ctx, "no-such-request", "builtin-admin", database.RequestMeta{}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("approving a missing request = %v, want sql.ErrNoRows", err)
	}
}

func TestApprovingAPromotionPromotesAViewer(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	requester := mkUser(t, db, "promo-viewer", database.SystemRoleViewer, time.Now(), nil, nil, false)
	req := &database.AccessRequest{RequesterID: requester, RequestType: database.AccessReqTypePromotion}
	if err := db.CreateAccessRequest(ctx, req, false, "", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApprovePromotionRequest(ctx, req.ID, "builtin-admin", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	var role, status string
	db.Conn().QueryRow(`SELECT system_role FROM users WHERE id=?`, requester).Scan(&role)
	db.Conn().QueryRow(`SELECT status FROM access_requests WHERE id=?`, req.ID).Scan(&status)
	if role != database.SystemRolePublisher || status != database.AccessReqStatusApproved {
		t.Fatalf("role %q, status %q", role, status)
	}
}
