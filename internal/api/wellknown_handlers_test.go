package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// /.well-known/security.txt.
//
// Two properties and both are about absence: nothing is published until an
// administrator has said who reads the reports, and what is published carries
// an expiry — because the failure mode of this file is that it stays correct
// for a year and then names somebody who left.

type securityTxtFixture struct {
	srv      *Server
	platform http.Handler
	apps     http.Handler
}

func newSecurityTxtFixture(t *testing.T) *securityTxtFixture {
	t.Helper()
	srv := newTestServerWithConfig(t, dbtest.New(t), &config.Config{
		BaseURL: "https://toft.example", AppsBaseURL: "https://apps.example",
		Port: 8080, AppsPort: 8081,
	})
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	return &securityTxtFixture{srv: srv, platform: srv.Router(http.NotFoundHandler()), apps: srv.AppsRouter()}
}

func (f *securityTxtFixture) fetch(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, securityTxtPath, nil))
	return rec
}

func (f *securityTxtFixture) save(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return sendAs(t, f.srv, f.platform, "root", http.MethodPut, "/api/v1/system/settings", body)
}

// Unconfigured is 404 on both origins. A blank file is a promise nobody is
// keeping, and it is worse than not publishing one at all.
func TestSecurityTxtIsAbsentUntilConfigured(t *testing.T) {
	f := newSecurityTxtFixture(t)

	for name, router := range map[string]http.Handler{"platform": f.platform, "apps": f.apps} {
		if rec := f.fetch(t, router); rec.Code != http.StatusNotFound {
			t.Errorf("%s origin = %d, want 404 with nothing configured", name, rec.Code)
		}
	}
}

// Configured, it is served unauthenticated on **both** origins — a researcher
// looking at a deployed app is on the apps origin and has no reason to know
// there is another one.
func TestSecurityTxtIsServedOnBothOrigins(t *testing.T) {
	f := newSecurityTxtFixture(t)
	expiry := time.Now().UTC().AddDate(0, 6, 0).Format("2006-01-02")
	if rec := f.save(t, `{"security_txt_contact":"mailto:security@example.com",
		"security_txt_expires":"`+expiry+`",
		"security_txt_policy":"https://example.com/disclosure",
		"security_txt_languages":"en,de-CH"}`); rec.Code != http.StatusOK {
		t.Fatalf("saving = %d: %s", rec.Code, rec.Body.String())
	}

	for name, router := range map[string]http.Handler{"platform": f.platform, "apps": f.apps} {
		rec := f.fetch(t, router)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s origin = %d, want 200: %s", name, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{
			"Contact: mailto:security@example.com",
			"Expires: " + expiry + "T23:59:59Z",
			"Policy: https://example.com/disclosure",
			"Preferred-Languages: en, de-CH",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s origin is missing %q:\n%s", name, want, body)
			}
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("%s origin content-type = %q, want text/plain", name, ct)
		}
	}
}

// Clearing the contact stops publishing it. That is the only honest way to
// turn this off — the alternative is a file with a blank contact.
func TestSecurityTxtStopsWhenTheContactIsCleared(t *testing.T) {
	f := newSecurityTxtFixture(t)
	expiry := time.Now().UTC().AddDate(0, 6, 0).Format("2006-01-02")
	f.save(t, `{"security_txt_contact":"mailto:sec@example.com","security_txt_expires":"`+expiry+`"}`)
	if rec := f.fetch(t, f.platform); rec.Code != http.StatusOK {
		t.Fatalf("precondition: %d", rec.Code)
	}

	if rec := f.save(t, `{"security_txt_contact":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clearing = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := f.fetch(t, f.platform); rec.Code != http.StatusNotFound {
		t.Errorf("after clearing = %d, want 404", rec.Code)
	}
}

// An expiry that has already passed is refused at save. This is the field that
// makes staleness visible, and one in the past is a control that does nothing.
func TestSecurityTxtRefusesAnExpiryInThePast(t *testing.T) {
	f := newSecurityTxtFixture(t)
	past := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	rec := f.save(t, `{"security_txt_contact":"mailto:sec@example.com","security_txt_expires":"`+past+`"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "future") {
		t.Errorf("the refusal does not say what is wrong: %s", rec.Body.String())
	}
}

// A contact with no expiry is refused rather than published without one, and
// rather than being given one on somebody's behalf. Both would be the platform
// making a commitment nobody made.
func TestSecurityTxtRefusesAContactWithNoExpiry(t *testing.T) {
	f := newSecurityTxtFixture(t)

	rec := f.save(t, `{"security_txt_contact":"mailto:sec@example.com"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if rec := f.fetch(t, f.platform); rec.Code != http.StatusNotFound {
		t.Errorf("a refused save published something: %d", rec.Code)
	}
}

// A bare email address is the mistake everybody makes here, and a file
// carrying one fails every validator that reads it.
func TestSecurityTxtRefusesANonURIContact(t *testing.T) {
	f := newSecurityTxtFixture(t)
	expiry := time.Now().UTC().AddDate(0, 6, 0).Format("2006-01-02")

	for _, contact := range []string{"security@example.com", "http://example.com/security", "ftp://x/y"} {
		rec := f.save(t, `{"security_txt_contact":"`+contact+`","security_txt_expires":"`+expiry+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q = %d, want 400", contact, rec.Code)
		}
	}
}

// A stale expiry is served as stored. The RFC's field is a record of what the
// install committed to, and rolling it forward would forge a commitment
// nobody made — the settings page is where the loud complaint belongs.
func TestSecurityTxtServesAStaleExpiryUnchanged(t *testing.T) {
	stale := "2020-01-01"
	got := renderSecurityTxt(database.PlatformSettings{
		SecurityTxtContact: "mailto:sec@example.com", SecurityTxtExpires: stale,
	})
	if !strings.Contains(got, "Expires: "+stale+"T23:59:59Z") {
		t.Errorf("a passed expiry was not served verbatim:\n%s", got)
	}
}
