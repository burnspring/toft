package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

// The app access-request workflow, end to end through the real router: somebody
// holding the `list` rung asks, the app's owners are told, one of them decides,
// and the requester is told what was decided.
//
// The twin of promotion_flow_test.go, and the difference between them is the
// whole point of these tests: a publisher request goes to the install's
// administrators, and this goes to the owners of one app.

// newAccessEnv seeds the list-rung fixture on a server with a mailer, plus the
// owner grant.
//
// The grant is written here because seedListRungApp goes through CreateApp,
// which writes no owner row — only CreateAppTx does, which is the path every
// app in production takes. Without it there is no owner to write to and the
// notification correctly goes nowhere, which would make these tests pass for
// the wrong reason.
func newAccessEnv(t *testing.T) (*Server, http.Handler, *fakeMailer, *database.App) {
	t.Helper()
	srv, router, fake := newMailTestServer(t)
	app := seedListRungApp(t, srv)
	if err := srv.db.UpsertGrant(context.Background(), app.ID, database.PrincipalTypeUser,
		"owner", database.ObjectRoleOwner, "owner", database.RequestMeta{}); err != nil {
		t.Fatalf("owner grant: %v", err)
	}
	return srv, router, fake, app
}

func askForAccess(t *testing.T, srv *Server, router http.Handler, app *database.App, who, body string) string {
	t.Helper()
	rec := sendJSONAs(t, srv, router, who, http.MethodPost,
		"/api/v1/apps/"+app.ID+"/access-requests", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create access request = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var req database.AccessRequest
	if err := json.Unmarshal(rec.Body.Bytes(), &req); err != nil {
		t.Fatal(err)
	}
	return req.ID
}

// Who is told, and who is not. Before this existed the ask reached nobody: it
// sat on the app's Access tab waiting for the owner to think to look, which is
// the "people route around it into chat" outcome the note field exists to
// prevent.
func TestAccessRequest_TellsTheOwnerAndNobodyElse(t *testing.T) {
	srv, router, fake, app := newAccessEnv(t)

	// An editor on the app and an administrator of the install. Neither is
	// written to: approving is owner-only, so an editor cannot act on this, and
	// the administrator — who can, through the short-circuit in Authorize — is
	// not who it was routed to.
	seedUser(t, srv.db, "helper", database.SystemRolePublisher)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	if err := srv.db.UpsertGrant(context.Background(), app.ID, database.PrincipalTypeUser,
		"helper", database.ObjectRoleEditor, "owner", database.RequestMeta{}); err != nil {
		t.Fatalf("editor grant: %v", err)
	}

	askForAccess(t, srv, router, app, "lister",
		`{"role":"viewer","note":"I own the quarterly close and this is the source"}`)

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages, want one addressed to the app's owner", len(msgs))
	}
	if len(msgs[0].To) != 1 || msgs[0].To[0] != "owner@example.com" {
		t.Fatalf("the request went to %v, want the owner alone", msgs[0].To)
	}
	for _, unwanted := range []string{"helper@example.com", "root@example.com", "lister@example.com"} {
		if strings.Contains(strings.Join(msgs[0].To, " "), unwanted) {
			t.Errorf("%s was written to about a decision that is not theirs", unwanted)
		}
	}
	if !strings.Contains(msgs[0].Text, "I own the quarterly close and this is the source") {
		t.Error("the notification drops the requester's note, which is the thing an owner answers")
	}
	if !strings.Contains(msgs[0].Text, "Payroll") {
		t.Error("the notification does not name the app it is about")
	}
	// The deep link is the queue, not the app's front page: `access` is the
	// tab's name (`sharing` is the pre-ADR §33 spelling) and the section is the
	// anchor.
	//
	// **And it is keyed by the app's id.** The slug this used to carry is the
	// apps origin's identifier, not the SPA's: on a split-origin install the
	// platform origin recognises a real slug and redirects it away, and on a
	// single-origin one the proxy claims it before the SPA is served — so the
	// button reached the owner's own running app, never the queue.
	wantLink := "/apps/" + app.ID + "/access#access-requests"
	if !strings.Contains(msgs[0].HTML, wantLink) {
		t.Errorf("the notification does not link to the queue at %s: %s", wantLink, msgs[0].HTML)
	}
	if strings.Contains(msgs[0].HTML, "/apps/"+app.Slug+"/") {
		t.Errorf("the notification links by slug, which addresses the running app: %s", msgs[0].HTML)
	}
}

// A request that was decided before it was stored has nobody waiting on it.
func TestAccessRequest_AnAutoDeniedAskTellsNobody(t *testing.T) {
	srv, router, fake, app := newAccessEnv(t)
	seedUser(t, srv.db, "vic", database.SystemRoleViewer)
	if err := srv.db.UpsertGrant(context.Background(), app.ID, database.PrincipalTypeUser,
		"vic", database.ObjectRoleList, "owner", database.RequestMeta{}); err != nil {
		t.Fatalf("list grant: %v", err)
	}

	reqID := askForAccess(t, srv, router, app, "vic", `{"role":"editor"}`)

	req, err := srv.db.GetAccessRequest(reqID)
	if err != nil || req == nil {
		t.Fatalf("GetAccessRequest: %v", err)
	}
	if req.Status != database.AccessReqStatusDenied {
		t.Fatalf("a system viewer asking for editor was stored as %q, want denied", req.Status)
	}
	if msgs := fake.messages(); len(msgs) != 0 {
		t.Errorf("sent %d messages about a request the platform had already decided", len(msgs))
	}
}

// The answer, to the person who asked — and the role it names is the one they
// hold rather than the one they asked for.
func TestAccessApprove_TellsTheRequesterTheRoleTheyHold(t *testing.T) {
	t.Run("the role that was asked for", func(t *testing.T) {
		srv, router, fake, app := newAccessEnv(t)
		reqID := askForAccess(t, srv, router, app, "lister", `{"role":"viewer"}`)

		rec := sendJSONAs(t, srv, router, "owner", http.MethodPost,
			"/api/v1/apps/"+app.ID+"/access-requests/"+reqID+"/approve", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("approve = %d, want 204 (%s)", rec.Code, rec.Body.String())
		}

		msgs := fake.messages()
		if len(msgs) != 2 {
			t.Fatalf("sent %d messages, want the owner's notice and the approval", len(msgs))
		}
		approval := msgs[1]
		if len(approval.To) != 1 || approval.To[0] != "lister@example.com" {
			t.Fatalf("the approval went to %v, want the requester", approval.To)
		}
		if !strings.Contains(approval.Text, "viewer") {
			t.Error("the approval does not say what the requester may now do")
		}
		if !strings.Contains(approval.Text, "owner") {
			t.Error("the approval does not say who decided it")
		}
	})

	// The stale request. They asked to be a viewer, the owner made them an
	// editor in the meantime, and clearing the old row must neither demote them
	// nor tell them it did.
	t.Run("the role they already had", func(t *testing.T) {
		srv, router, fake, app := newAccessEnv(t)
		reqID := askForAccess(t, srv, router, app, "lister", `{"role":"viewer"}`)
		if err := srv.db.UpsertGrant(context.Background(), app.ID, database.PrincipalTypeUser,
			"lister", database.ObjectRoleEditor, "owner", database.RequestMeta{}); err != nil {
			t.Fatalf("editor grant: %v", err)
		}

		rec := sendJSONAs(t, srv, router, "owner", http.MethodPost,
			"/api/v1/apps/"+app.ID+"/access-requests/"+reqID+"/approve", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("approve = %d, want 204 (%s)", rec.Code, rec.Body.String())
		}

		msgs := fake.messages()
		approval := msgs[len(msgs)-1]
		if !strings.Contains(approval.Text, "editor") {
			t.Errorf("the approval names the smaller role, telling somebody they were "+
				"demoted when nothing moved: %s", approval.Text)
		}
	})
}

// A refusal with no reason is the version of this workflow people route around
// by asking in chat, so the reason travels with it.
func TestAccessDeny_EmailsTheReason(t *testing.T) {
	srv, router, fake, app := newAccessEnv(t)
	reqID := askForAccess(t, srv, router, app, "lister", `{"role":"viewer"}`)

	rec := sendJSONAs(t, srv, router, "owner", http.MethodPost,
		"/api/v1/apps/"+app.ID+"/access-requests/"+reqID+"/deny",
		`{"reason":"this holds salary data; ask your manager to sponsor it"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deny = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}

	msgs := fake.messages()
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want the owner's notice and the refusal", len(msgs))
	}
	refusal := msgs[1]
	if len(refusal.To) != 1 || refusal.To[0] != "lister@example.com" {
		t.Fatalf("the refusal went to %v, want the requester", refusal.To)
	}
	if !strings.Contains(refusal.Text, "this holds salary data; ask your manager to sponsor it") {
		t.Errorf("the refusal drops the reason: %s", refusal.Text)
	}
}

// Withdrawing is the requester's own act. Mailing them what they just did, or
// telling the owner about a request that has left the queue, are both noise.
func TestAccessWithdraw_TellsNobody(t *testing.T) {
	srv, router, fake, app := newAccessEnv(t)
	reqID := askForAccess(t, srv, router, app, "lister", `{"role":"viewer"}`)
	before := len(fake.messages())

	rec := sendJSONAs(t, srv, router, "lister", http.MethodDelete,
		"/api/v1/apps/"+app.ID+"/access-requests/"+reqID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("withdraw = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if got := len(fake.messages()); got != before {
		t.Errorf("withdrawing sent %d message(s)", got-before)
	}
}
