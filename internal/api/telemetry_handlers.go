package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/entitlements"
	"toft/internal/telemetry"
)

// The telemetry admin surface: read what is in force, change where this install
// exports to, and prove the collector answers.
//
// Admin-only and 404 to everyone else, like /system/settings and
// /admin/email-settings: which collector this install exports to is
// infrastructure detail, and the credential beside it is an operator's.
//
// The scrape endpoint is deliberately **not** editable here. It appears in the
// read as a fact about the deployment, and changing it means changing
// METRICS_ADDR and restarting — because it opens a network listener, and an
// administrator who could open one from a browser is a different security
// question from one who can redirect an outbound export (ADR §31 D3).

type telemetryStatusResponse struct {
	// Configured reports that an administrator has saved the document. False
	// means the environment is the whole configuration — the state every
	// install starts in and most stay in.
	Configured bool `json:"configured"`
	// Source is `settings` or `environment`: which of the two produced what is
	// actually running. It exists so the page can say so, rather than leave an
	// administrator wondering why the endpoint they see is not the one in their
	// unit file.
	Source string `json:"source"`

	Enabled    bool   `json:"enabled"`
	Endpoint   string `json:"endpoint"`
	HeadersSet bool   `json:"headers_set"`
	// HeadersUnreadable is a stored collector credential this install's keys
	// cannot decrypt — a lost or changed ENCRYPTION_KEY. See credentials.go.
	// Never true of the environment fallback below, which is not encrypted.
	HeadersUnreadable bool    `json:"headers_unreadable"`
	Traces            bool    `json:"traces"`
	Metrics           bool    `json:"metrics"`
	SampleRatio       float64 `json:"sample_ratio"`
	Temporality       string  `json:"temporality"`

	// EnvEndpoint is what the environment would give if the document were
	// cleared. Shown beside the form so "hand it back to the environment" is a
	// decision with a visible consequence rather than a leap.
	EnvEndpoint string `json:"env_endpoint"`
	EnvPresent  bool   `json:"env_present"`

	// Deployment configuration, read-only here.
	ScrapeEnabled  bool   `json:"scrape_enabled"`
	ScrapeAddr     string `json:"scrape_addr"`
	ScrapeTokenSet bool   `json:"scrape_token_set"`

	// Health is absent when nothing is exporting.
	Health *telemetryHealthDTO `json:"health,omitempty"`
}

type telemetryHealthDTO struct {
	SpansExported  int64 `json:"spans_exported"`
	SpansDropped   int64 `json:"spans_dropped"`
	ExportFailures int64 `json:"export_failures"`
	MetricExports  int64 `json:"metric_exports"`
	MetricFailures int64 `json:"metric_failures"`

	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	// Since is when the running configuration took effect, not process start.
	// An administrator who has just re-pointed the collector is asking about
	// the new one, and carrying the old one's failures forward would answer a
	// question nobody asked.
	Since time.Time `json:"since"`
}

type telemetryRequest struct {
	Enabled     bool    `json:"enabled"`
	Endpoint    string  `json:"endpoint"`
	Traces      bool    `json:"traces"`
	Metrics     bool    `json:"metrics"`
	SampleRatio float64 `json:"sample_ratio"`
	Temporality string  `json:"temporality"`
	// Headers is the collector credential. Absent means "keep what is stored",
	// which is what lets the form render a placeholder instead of a secret;
	// present-and-empty clears it. The same contract as the SMTP password.
	Headers *string `json:"headers,omitempty"`
	// UseEnvironment abandons the document and hands the install back to
	// OTEL_*. A field rather than a DELETE, because it is one of the choices
	// the form offers and belongs in the same audited write as the others.
	UseEnvironment bool `json:"use_environment"`
}

func (s *Server) handleGetTelemetrySettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireTelemetryAdmin(w, r) {
		return
	}
	doc, err := s.db.GetTelemetrySettings()
	if err != nil {
		log.Printf("reading telemetry settings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.telemetryDTO(doc))
}

// handleUpdateTelemetrySettings saves the document and applies it immediately.
//
// "Immediately" is the whole point of this endpoint existing. The pipeline is
// rebuilt and swapped in before the response is written, so an administrator
// who fixes a collector address and presses Save can watch the failure counter
// stop climbing — rather than being told to find somebody who can restart the
// platform.
func (s *Server) handleUpdateTelemetrySettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireTelemetryAdmin(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())

	var body telemetryRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	// The license gate, asked after the body is read because the body is what
	// says whether this write configures or removes. Handing the install back
	// to OTEL_* and switching the exporter off are removals, and removal is
	// never gated: a license that trapped a collector in place after a trial
	// ended would make the install worse to own than no license at all.
	// Configuring — a collector, on — needs one.
	if !body.UseEnvironment && body.Enabled && s.refusedUnlicensed(w, r, entitlements.FeatureTelemetry) {
		return
	}

	current, err := s.db.GetTelemetrySettings()
	if err != nil {
		log.Printf("reading telemetry settings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if body.UseEnvironment {
		// Handing the install back to OTEL_*. The endpoint and the credential go
		// with it: leaving a stored collector address behind a cleared
		// `configured` flag is a landmine for whoever saves this page next, who
		// would silently re-point the install at a collector they never chose.
		if err := s.db.ClearTelemetrySettings(p.ID, requestMeta(r.Context())); err != nil {
			log.Printf("clearing telemetry settings: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	} else {
		doc := current
		doc.Enabled = body.Enabled
		doc.Endpoint = strings.TrimSpace(body.Endpoint)
		doc.Traces = body.Traces
		doc.Metrics = body.Metrics
		doc.SampleRatio = body.SampleRatio
		doc.Temporality = body.Temporality

		if err := validateTelemetryRequest(doc); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "correct the field the sentence names and save again — nothing was saved"})
			return
		}
		// Absent means keep; present-and-empty means clear. The stored value is
		// ciphertext and is never sent to a browser, so "keep" cannot be
		// expressed by echoing it back.
		if body.Headers != nil {
			if v := strings.TrimSpace(*body.Headers); v == "" {
				doc.Headers = ""
			} else {
				enc, err := s.encryptor.Encrypt(v)
				if err != nil {
					log.Printf("encrypting the collector credential: %v", err)
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
					return
				}
				doc.Headers = enc
			}
		}
		if err := s.db.SetTelemetrySettings(&doc, p.ID, requestMeta(r.Context())); err != nil {
			log.Printf("saving telemetry settings: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}

	// Re-read rather than trusting the struct just written: Set normalises, and
	// the response has to describe what is stored rather than what was asked
	// for.
	saved, err := s.db.GetTelemetrySettings()
	if err != nil {
		log.Printf("reading telemetry settings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := s.applyTelemetry(r.Context(), saved); err != nil {
		// The document is saved and the pipeline is not. Reported rather than
		// rolled back: the setting is what the administrator decided, and a
		// restart would apply it — so the honest answer is "saved, and here is
		// why it is not running yet", not a silent revert of their decision.
		log.Printf("applying telemetry settings: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"settings": s.telemetryDTO(saved),
			"warning": "The settings were saved but could not be applied: " + err.Error() +
				". They will take effect on the next restart.",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"settings": s.telemetryDTO(saved)})
}

// handleTestTelemetry exports one span through the *saved* configuration and
// returns the collector's own error.
//
// It uses the stored settings rather than a submitted form, for the reason
// mail's test send does: what it proves is that the configuration this install
// is actually running works. And it is synchronous, unlike every other export
// in this package — the administrator pressing the button is waiting for
// exactly that answer, which is the one case where blocking on the collector is
// the correct behaviour.
func (s *Server) handleTestTelemetry(w http.ResponseWriter, r *http.Request) {
	if !s.requireTelemetryAdmin(w, r) {
		return
	}
	// Not gated. A license gates configuration, never operation, and this
	// writes nothing: it asks the collector that is already configured whether
	// it is listening, which an unlicensed install running a collector from a
	// trial is exactly the install that needs to know.
	if !telemetry.Enabled() {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "telemetry is not switched on, so there is nothing to test",
			"hint":  `PUT /api/v1/admin/telemetry-settings with an endpoint and "enabled": true, then test again`})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := telemetry.TestExport(ctx); err != nil {
		// The collector's own words, scrubbed. An administrator debugging a 404
		// on a wrong path or a 401 on a stale token needs the text, and a
		// generic failure would send them to the logs for something the button
		// already knows.
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// requireTelemetryAdmin is the gate. 404 rather than 403 to everyone else, on
// the same rule as /system/settings.
func (s *Server) requireTelemetryAdmin(w http.ResponseWriter, r *http.Request) bool {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return false
	}
	return true
}

// applyTelemetry resolves the document against the environment and rebuilds the
// pipeline.
//
// One place, so the boot path and the settings path cannot disagree about
// precedence — the same reason ResolveRetention and ResolveAttachment exist.
func (s *Server) applyTelemetry(ctx context.Context, doc database.TelemetrySettings) error {
	headers, err := s.decryptTelemetryHeaders(doc)
	if err != nil {
		return err
	}
	return telemetry.Apply(ctx, telemetry.Resolve(s.cfg.Telemetry, doc, headers))
}

// decryptTelemetryHeaders turns the stored ciphertext into the credential the
// exporter needs. Only the API layer holds the encryptor, which is why the
// database and the telemetry package each deal in one direction only.
func (s *Server) decryptTelemetryHeaders(doc database.TelemetrySettings) (string, error) {
	if doc.Headers == "" || s.encryptor == nil {
		return "", nil
	}
	return s.encryptor.Decrypt(doc.Headers)
}

func (s *Server) telemetryDTO(doc database.TelemetrySettings) telemetryStatusResponse {
	env := s.cfg.Telemetry
	out := telemetryStatusResponse{
		Configured:        doc.Configured,
		Enabled:           doc.Enabled,
		Endpoint:          doc.Endpoint,
		HeadersSet:        doc.Headers != "",
		HeadersUnreadable: s.credentialUnreadable(doc.Headers),
		Traces:            doc.Traces,
		Metrics:           doc.Metrics,
		SampleRatio:       doc.SampleRatio,
		Temporality:       doc.Temporality,

		EnvEndpoint: telemetry.EndpointHost(env.Endpoint),
		EnvPresent:  env.Endpoint != "",

		ScrapeEnabled:  env.ScrapeEnabled(),
		ScrapeAddr:     env.MetricsAddr,
		ScrapeTokenSet: env.MetricsToken != "",
	}
	// With the document never saved, the form is seeded from the environment
	// rather than left blank: an administrator taking the install off OTEL_*
	// should start from what it is doing now, not from nothing.
	if !doc.Configured {
		out.Enabled = env.Endpoint != ""
		out.Endpoint = env.Endpoint
		out.Traces = env.TracesEnabled()
		out.Metrics = env.MetricsEnabled()
		out.SampleRatio = env.SamplerArg
		out.Temporality = env.Temporality
		out.HeadersSet = env.Headers != ""
		// The environment's headers are plaintext, so this half of the form
		// can never be unreadable however lost the key is.
		out.HeadersUnreadable = false
	}

	if rt, appliedAt, ok := telemetry.Current(); ok {
		out.Source = rt.Source
		if h := telemetry.Health(); h != nil {
			dto := telemetryHealthDTO{
				SpansExported:  h.SpansExported.Load(),
				SpansDropped:   h.SpansDropped.Load(),
				ExportFailures: h.ExportFailures.Load(),
				MetricExports:  h.MetricExports.Load(),
				MetricFailures: h.MetricFailures.Load(),
				Since:          appliedAt,
			}
			if msg, at := h.LastError(); msg != "" {
				dto.LastError = msg
				dto.LastErrorAt = &at
			}
			out.Health = &dto
		}
	}
	return out
}

// validateTelemetryRequest refuses what the exporter could not use, naming the
// field rather than failing at the next export — which would be minutes later,
// in a log, on a page the administrator has already left.
func validateTelemetryRequest(doc database.TelemetrySettings) error {
	if doc.Enabled {
		if doc.Endpoint == "" {
			return errTelemetry("a collector endpoint is required to switch telemetry on")
		}
		u, err := url.Parse(doc.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errTelemetry("the endpoint must be a URL like http://collector.internal:4318")
		}
		if !doc.Traces && !doc.Metrics {
			return errTelemetry("switch on traces, metrics, or both — telemetry with neither exports nothing")
		}
	}
	if doc.SampleRatio < 0 || doc.SampleRatio > 1 {
		return errTelemetry("the sampling ratio must be between 0 and 1")
	}
	if !database.ValidTelemetryTemporality(doc.Temporality) {
		return errTelemetry("temporality must be cumulative or delta")
	}
	return nil
}

// errTelemetry is a validation message that is safe to show a person: it names
// the field and what would work, and carries nothing from the request.
type errTelemetry string

func (e errTelemetry) Error() string { return string(e) }
