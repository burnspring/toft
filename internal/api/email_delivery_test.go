package api

import (
	"net/http"
	"testing"
	"time"

	"toft/internal/authprovider"
	"toft/internal/database"
)

// The email check reads the relay's verdict from the newest finished send: a
// relay that rejected its own credential used to pass as "configured" while
// every send failed (trytoft, 2026-10-23).
func TestTheEmailCheckReadsTheNewestFinishedSend(t *testing.T) {
	s := func(status, err string) database.IntegrationSend {
		return database.IntegrationSend{Status: status, Error: err}
	}
	cases := []struct {
		name  string
		sends []database.IntegrationSend // newest first
		want  string
	}{
		{"nothing sent", nil, ""},
		{"the newest failed", []database.IntegrationSend{s("failed", "535 Authentication Credentials Invalid"), s("sent", "")}, "535 Authentication Credentials Invalid"},
		{"delivered since the failure", []database.IntegrationSend{s("sent", ""), s("failed", "535")}, ""},
		{"queued and blocked say nothing", []database.IntegrationSend{s("queued", ""), s("blocked", "not attached"), s("failed", "dial failed")}, "dial failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := refusedLast(tc.sends)
			if (got == nil) != (tc.want == "") || (got != nil && got.Error != tc.want) {
				t.Errorf("refusedLast = %+v, want error %q", got, tc.want)
			}
		})
	}
}

// A replay of a failed send says why it failed, as notify.status does.
func TestAReplayedSendCarriesItsFailure(t *testing.T) {
	got := replayedSend(&database.IntegrationSend{ID: "s1", Status: "failed", Error: "535 Authentication Credentials Invalid"})
	if got["error"] != "535 Authentication Credentials Invalid" || got["duplicate"] != true || got["send_id"] != "s1" {
		t.Errorf("replay = %v, want the send's id, duplicate and its error", got)
	}
	if _, ok := replayedSend(&database.IntegrationSend{ID: "s2", Status: "queued"})["error"]; ok {
		t.Error("a send with no failure carries an error key")
	}
}

// A forwarded request carries its cut-off, and a WebSocket upgrade does not:
// the connection outlives any deadline, and a Streamlit or marimo session's
// headers are its handshake's, so a deadline there went stale for the session.
func TestAnUpgradeCarriesNoRequestDeadline(t *testing.T) {
	now := time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC)
	plain, _ := http.NewRequest(http.MethodGet, "http://app/", nil)
	setRequestDeadline(plain, now, 5*time.Minute)
	if got := plain.Header.Get(RequestDeadlineHeader); got != "2026-10-23T16:05:00Z" {
		t.Errorf("plain request deadline = %q", got)
	}
	ws, _ := http.NewRequest(http.MethodGet, "http://app/_stcore/stream", nil)
	ws.Header.Set("Connection", "keep-alive, Upgrade")
	ws.Header.Set("Upgrade", "websocket")
	ws.Header.Set(RequestDeadlineHeader, "forged by the client")
	setRequestDeadline(ws, now, 5*time.Minute)
	if got := ws.Header.Get(RequestDeadlineHeader); got != "" {
		t.Errorf("an upgrade carried a deadline: %q", got)
	}
}

// A claim mapping names a provider the install has and one that can assert a
// claim: an unknown id matched nobody and kept the directory check warning that
// the IdP was not sending its claim (trytoft, 2026-10-23).
func TestAClaimMappingNeedsAProviderThatCanSendTheClaim(t *testing.T) {
	if r := claimProviderRefusal(nil, errNoSuchProvider); r == nil || r["code"] != "unknown_provider" {
		t.Errorf("unknown provider: %v", r)
	}
	if r := claimProviderRefusal(&authprovider.Provider{ID: "local", Kind: database.ProviderKindPassword}, nil); r == nil || r["code"] != "provider_asserts_no_claims" {
		t.Errorf("password provider: %v", r)
	}
	if r := claimProviderRefusal(&authprovider.Provider{ID: "okta", Kind: "oidc"}, nil); r != nil {
		t.Errorf("an OIDC provider was refused: %v", r)
	}
}
