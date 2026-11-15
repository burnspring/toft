package database

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Telemetry settings live in the same `settings` table as PlatformSettings but
// are read and written as their own document, for the reason the email document
// is separate: `SetPlatformSettings` audits the full before/after JSON of every
// key it owns, and the collector credential must never appear in an audit row.
// The cheapest way to guarantee that is for the credential to live in a
// document that blob never touches.
//
// **This is a settings row rather than only an environment variable, and that
// reverses the original decision.** ADR §31 D3 argued telemetry was deployment
// configuration in the class of DATABASE_URL. Two of its three claims did not
// survive contact: a collector *does* move independently of the deployment that
// placed it, and on a single-VM install the operator and the administrator are
// the same person. What survives is the third — the OTEL_* variable names are
// an industry standard — so the environment stays, as the bootstrap.
//
// The precedence is the opposite of sign-in providers', deliberately. An
// environment-configured provider *wins* over a database row because it exists
// to solve "nobody can sign in to fix the database row", a bootstrap problem
// telemetry does not have: nobody is ever locked out of a collector. So here a
// saved document wins over the environment, and an install that has never saved
// one runs on OTEL_* exactly as before.

const (
	// SettingTelemetryConfigured records that an administrator has saved this
	// document at least once. It is what makes "never touched, so use the
	// environment" distinguishable from "deliberately switched off", which are
	// different states that would otherwise both read as `enabled=false`.
	SettingTelemetryConfigured  = "telemetry.configured"
	SettingTelemetryEnabled     = "telemetry.enabled"
	SettingTelemetryEndpoint    = "telemetry.endpoint"
	SettingTelemetryHeaders     = "telemetry.headers"
	SettingTelemetryTraces      = "telemetry.traces"
	SettingTelemetryMetrics     = "telemetry.metrics"
	SettingTelemetrySampleRatio = "telemetry.sample_ratio"
	SettingTelemetryTemporality = "telemetry.temporality"
)

// Metric temporality. Cumulative is what a Prometheus-shaped backend wants and
// is the default; delta is what a Datadog-shaped one wants.
const (
	TelemetryTemporalityCumulative = "cumulative"
	TelemetryTemporalityDelta      = "delta"
)

// ValidTelemetryTemporality reports whether a preference is one the exporter
// knows.
func ValidTelemetryTemporality(t string) bool {
	switch t {
	case TelemetryTemporalityCumulative, TelemetryTemporalityDelta:
		return true
	}
	return false
}

// TelemetrySettings is the export document.
//
// It carries the collector and nothing about the scrape endpoint. METRICS_ADDR
// and METRICS_TOKEN stay environment-only and are deliberately absent here: they
// open a network listener, and an administrator who could open one from a
// browser is a different security question from an administrator who can
// redirect an outbound export. See ADR §31 D3.
type TelemetrySettings struct {
	// Configured reports that this document has been saved. False means the
	// environment is the whole configuration.
	Configured bool `json:"configured"`
	Enabled    bool `json:"enabled"`
	// Endpoint is the collector's base URL — the exporters append /v1/traces
	// and /v1/metrics themselves.
	Endpoint string `json:"endpoint"`
	// Headers is the stored ciphertext of the collector credential, in the
	// `key=value,key=value` form OTEL_EXPORTER_OTLP_HEADERS uses. Never
	// marshalled to an API client — the handler builds its own DTO — and never
	// written to audit.
	Headers     string  `json:"-"`
	Traces      bool    `json:"traces"`
	Metrics     bool    `json:"metrics"`
	SampleRatio float64 `json:"sample_ratio"`
	Temporality string  `json:"temporality"`
}

// DefaultTelemetrySettings is what an unsaved document reads as. Everything off,
// because the *environment* decides for an install that has never saved one and
// this struct must not accidentally look like a decision somebody made.
func DefaultTelemetrySettings() TelemetrySettings {
	return TelemetrySettings{
		Traces:      true,
		Metrics:     true,
		SampleRatio: 0.05,
		Temporality: TelemetryTemporalityCumulative,
	}
}

// GetTelemetrySettings reads the document, filling anything unset from the
// defaults.
func (db *DB) GetTelemetrySettings() (TelemetrySettings, error) {
	s := DefaultTelemetrySettings()

	rows, err := db.conn.Query(
		`SELECT key, value FROM settings WHERE key LIKE 'telemetry.%'`)
	if err != nil {
		return s, err
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return s, err
		}
		switch key {
		case SettingTelemetryConfigured:
			s.Configured = value == "true"
		case SettingTelemetryEnabled:
			s.Enabled = value == "true"
		case SettingTelemetryEndpoint:
			s.Endpoint = value
		case SettingTelemetryHeaders:
			s.Headers = value
		case SettingTelemetryTraces:
			s.Traces = value == "true"
		case SettingTelemetryMetrics:
			s.Metrics = value == "true"
		case SettingTelemetrySampleRatio:
			if f, err := strconv.ParseFloat(value, 64); err == nil && f >= 0 && f <= 1 {
				s.SampleRatio = f
			}
		case SettingTelemetryTemporality:
			if ValidTelemetryTemporality(value) {
				s.Temporality = value
			}
		}
	}
	return s, rows.Err()
}

// SetTelemetrySettings writes the document and audits the change in the same
// transaction.
//
// s.Headers must already be ciphertext; the caller encrypts, because only the
// API layer holds the encryptor. The audit blob replaces the credential with two
// booleans — whether one is set, and whether this write changed it — so the
// question the log exists to answer ("who pointed this install's telemetry at a
// different collector, and did they rotate the credential") is answerable
// without the credential.
func (db *DB) SetTelemetrySettings(s *TelemetrySettings, actorID string, meta RequestMeta) error {
	before, err := db.GetTelemetrySettings()
	if err != nil {
		return err
	}

	s.Endpoint = strings.TrimSpace(s.Endpoint)
	if !ValidTelemetryTemporality(s.Temporality) {
		s.Temporality = TelemetryTemporalityCumulative
	}
	if s.SampleRatio < 0 || s.SampleRatio > 1 {
		s.SampleRatio = before.SampleRatio
	}
	// Saving *is* configuring. An administrator who opens the page, changes
	// nothing and presses save has still taken the install off the environment
	// and onto this document, and the page says so before they do.
	s.Configured = true

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	write := func(key, value string) error {
		_, err := tx.Exec(`
			INSERT INTO settings (key, value, updated_at, updated_by)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET
				value=excluded.value, updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
			key, value, now, nullIfEmpty(actorID))
		return err
	}

	for _, kv := range []struct{ key, value string }{
		{SettingTelemetryConfigured, boolText(true)},
		{SettingTelemetryEnabled, boolText(s.Enabled)},
		{SettingTelemetryEndpoint, s.Endpoint},
		{SettingTelemetryHeaders, s.Headers},
		{SettingTelemetryTraces, boolText(s.Traces)},
		{SettingTelemetryMetrics, boolText(s.Metrics)},
		{SettingTelemetrySampleRatio, strconv.FormatFloat(s.SampleRatio, 'g', -1, 64)},
		{SettingTelemetryTemporality, s.Temporality},
	} {
		if err := write(kv.key, kv.value); err != nil {
			return err
		}
	}

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "telemetry_settings.update",
		TargetType: "platform", TargetID: "telemetry",
		Before:    jsonStr(telemetryAudit(before, false)),
		After:     jsonStr(telemetryAudit(*s, s.Headers != before.Headers)),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing telemetry settings change: %w", err)
	}

	return tx.Commit()
}

// telemetryAudit is the redacted shape written to audit_log. Built by hand
// rather than by marshalling TelemetrySettings so that adding a field to the
// struct cannot silently start leaking it.
func telemetryAudit(s TelemetrySettings, headersChanged bool) map[string]interface{} {
	return map[string]interface{}{
		"configured": s.Configured,
		"enabled":    s.Enabled,
		// In the clear, and deliberately: this is not a credential, and "who
		// redirected this install's telemetry, and where to" is precisely the
		// question this log exists to answer. Redacting it would leave the
		// change visible and its substance invisible.
		"endpoint":        s.Endpoint,
		"traces":          s.Traces,
		"metrics":         s.Metrics,
		"sample_ratio":    s.SampleRatio,
		"temporality":     s.Temporality,
		"headers_set":     s.Headers != "",
		"headers_changed": headersChanged,
	}
}

// ClearTelemetrySettings hands the install back to the environment.
//
// It deletes the rows rather than writing `configured=false` over them, and the
// difference matters: a stored endpoint sitting behind a cleared flag is a
// landmine for whoever opens this page next, who would see a form seeded from
// the environment, press Save, and silently re-point the install at a collector
// somebody chose months ago and nobody remembers. Absent means absent.
//
// The credential goes with them, for the same reason and one more: a ciphertext
// that no longer has an endpoint to go with is a secret being retained for no
// purpose.
func (db *DB) ClearTelemetrySettings(actorID string, meta RequestMeta) error {
	before, err := db.GetTelemetrySettings()
	if err != nil {
		return err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM settings WHERE key LIKE 'telemetry.%'`); err != nil {
		return err
	}

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "telemetry_settings.update",
		TargetType: "platform", TargetID: "telemetry",
		Before: jsonStr(telemetryAudit(before, false)),
		// The after state is not a document at all, and saying so plainly beats
		// an audit row full of zeroes that reads like "somebody switched
		// everything off" — which is a different decision with different
		// consequences.
		After:     jsonStr(map[string]interface{}{"configured": false, "source": "environment"}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing telemetry settings change: %w", err)
	}

	return tx.Commit()
}
