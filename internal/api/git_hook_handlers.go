package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/database"
	"toft/internal/telemetry"
)

// The webhook, which buys one minute and is the only unauthenticated route in
// this feature.
//
// **A verified delivery marks apps for an immediate poll and enqueues
// nothing.** The poller then does what it has always done — the source policy,
// the credential, `ls-remote`, the guarded claim — so a delivery that never
// arrives, arrives twice, or fails verification costs the install exactly the
// minute it was buying. That is why an install that cannot receive an inbound
// request needs no configuration here and no reachability query: it degrades to
// the behaviour it had before this route existed.
//
// It is last on purpose. Everything above works without it, and it is the one
// place on this platform where an unauthenticated request reaches a handler
// that touches app rows.

// maxHookBody caps what an unauthenticated caller may make this process read.
// A push payload from either host is far under this.
const maxHookBody = 1 << 20

// AutodeployNudger is the auto-deploy poller, as this route sees it. Nil on an
// install wired without one, and a nil one means a verified delivery is counted
// and does nothing — which is the same outcome as no delivery at all.
type AutodeployNudger interface {
	PollNow(appIDs []string)
}

// SetAutodeploy supplies the poller so a verified webhook can shorten the wait
// for a push. Absent, every install still deploys on the ordinary interval.
//
// A setter rather than a ServerOption, and that is forced rather than a
// preference: the poller takes the builder, the builder is constructed before
// the Server, and the Server is what the poller's route hangs off — so one of
// the three has to be wired after construction, and this is the one whose
// absence changes nothing.
func (s *Server) SetAutodeploy(n AutodeployNudger) { s.autodeploy = n }

// handleGitHook takes a push delivery.
//
// Unauthenticated by design — the host has no session and no key — and HMAC
// verified against the connection's own `webhook_secret`, which the platform
// minted rather than asking anybody to choose.
func (s *Server) handleGitHook(w http.ResponseWriter, r *http.Request) {
	connectionID := chi.URLParam(r, "connectionID")

	conn, err := s.db.GetGitConnection(connectionID)
	if errors.Is(err, database.ErrGitConnectionNotFound) {
		// 204 rather than 404. An unauthenticated endpoint that tells the
		// internet which connection ids exist is an oracle, and there is
		// nothing a real host would do differently with the answer.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		log.Printf("git hook: reading connection %s: %v", connectionID, err)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxHookBody))
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	secret, err := s.encryptor.Decrypt(conn.EncryptedWebhookSecret())
	if err != nil || secret == "" {
		telemetry.RecordGitHookDelivery(r.Context(), telemetry.GitHookUnverified)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !hookSignatureValid(r, body, secret) {
		// **Counted and not recorded per delivery.** An unauthenticated
		// endpoint that writes a row per request is a way to fill a table from
		// the internet.
		telemetry.RecordGitHookDelivery(r.Context(), telemetry.GitHookUnverified)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// **Only a push is acted on.** An App's webhook receives every event it
	// subscribes to — issues, pull requests, stars — and almost all of them
	// name the repository, so each became an ls-remote per matching app
	// (security review 2026-11-14, DG-8). Both hosts always name the event.
	if !isPushDelivery(r) {
		telemetry.RecordGitHookDelivery(r.Context(), telemetry.GitHookIgnored)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	repo := deliveredRepository(body)
	if repo == "" {
		telemetry.RecordGitHookDelivery(r.Context(), telemetry.GitHookIgnored)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// **A delivery writes nothing about the connection's health.** last_error
	// and last_ok_at mean the outbound clone credential, which only Test
	// exercises: a delivery proves the webhook secret and inbound reach, and
	// clearing a credential failure on it showed a connection as healthy
	// while every clone through it failed (security review 2026-11-14, DG-7).

	appIDs, err := s.db.AppsOnConnectionForRepo(conn.ID, repo)
	if err != nil {
		log.Printf("git hook: finding apps for %s on %s: %v", repo, conn.DisplayName, err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(appIDs) == 0 {
		// A repository no app deploys from. Nothing to mark, and nothing
		// written — a delivery is not a reason to record anything.
		telemetry.RecordGitHookDelivery(r.Context(), telemetry.GitHookIgnored)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	telemetry.RecordGitHookDelivery(r.Context(), telemetry.GitHookAccepted)
	if s.autodeploy != nil {
		s.autodeploy.PollNow(appIDs)
	}
	w.WriteHeader(http.StatusNoContent)
}

// isPushDelivery reports whether the host says this delivery is a push:
// `X-GitHub-Event: push`, or GitLab's `X-Gitlab-Event: Push Hook`.
func isPushDelivery(r *http.Request) bool {
	return r.Header.Get("X-GitHub-Event") == "push" || r.Header.Get("X-Gitlab-Event") == "Push Hook"
}

// hookSignatureValid checks the delivery against the connection's secret.
//
// Both hosts sign the raw body: GitHub with HMAC-SHA256 under
// `X-Hub-Signature-256`, GitLab with a shared token compared whole under
// `X-Gitlab-Token`. Both comparisons are constant-time.
func hookSignatureValid(r *http.Request, body []byte, secret string) bool {
	if token := r.Header.Get("X-Gitlab-Token"); token != "" {
		return hmac.Equal([]byte(token), []byte(secret))
	}
	signature := r.Header.Get("X-Hub-Signature-256")
	if signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(want))
}

// deliveredRepository pulls "owner/name" out of a push payload.
//
// Both hosts put it in the same place under different spellings, and a payload
// this cannot read is one the delivery is ignored for — guessing at a
// repository from an unauthenticated body is how a webhook becomes a way to
// poll somebody else's app.
func deliveredRepository(body []byte) string {
	var payload struct {
		Repository struct {
			FullName          string `json:"full_name"`
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"repository"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	for _, candidate := range []string{
		payload.Repository.FullName,
		payload.Repository.PathWithNamespace,
		payload.Project.PathWithNamespace,
	} {
		if v := strings.Trim(strings.TrimSpace(candidate), "/"); v != "" && strings.Contains(v, "/") {
			return v
		}
	}
	return ""
}
