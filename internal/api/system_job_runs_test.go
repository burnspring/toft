package api

import (
	"net/http"
	"testing"

	"toft/internal/database"
)

// The install-wide run list names every app with work in flight, which is the
// estate — the same enumeration /system/processes and /system/queue are
// admin-only for.
func TestSystemJobRuns_AdminOnly(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "publisher", database.SystemRolePublisher)

	const path = "/api/v1/system/job-runs"
	if rec := getAs(t, srv, router, "", path); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d, want 401", rec.Code)
	}
	if rec := getAs(t, srv, router, "publisher", path); rec.Code != http.StatusForbidden {
		t.Errorf("publisher: %d, want 403", rec.Code)
	}
	if rec := getAs(t, srv, router, "admin", path); rec.Code != http.StatusOK {
		t.Errorf("admin: %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// A filter that silently does nothing is how an operator concludes there were
// no failures. An unknown status is refused by name instead.
func TestSystemJobRuns_UnknownStatusIsRefused(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)

	rec := getAs(t, srv, router, "admin", "/api/v1/system/job-runs?status=broken")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=broken: %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}

	// Every shown status, and the pseudo-status the page defaults to, are all
	// accepted — the vocabulary and the filter must not drift apart.
	for _, status := range []string{
		database.JobRunInFlight, database.JobRunUnsuccessful,
		database.JobRunQueued,
		database.JobRunRunning, database.JobRunSucceeded, database.JobRunFailed,
		database.JobRunCancelled, database.JobRunTimedOut,
		database.JobRunOutOfMemory, database.JobRunSkipped,
	} {
		rec := getAs(t, srv, router, "admin", "/api/v1/system/job-runs?status="+status)
		if rec.Code != http.StatusOK {
			t.Errorf("status=%s: %d, want 200 (body=%s)", status, rec.Code, rec.Body.String())
		}
	}
}
