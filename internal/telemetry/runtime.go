package telemetry

import (
	"net/url"
	"strings"
	"time"

	"toft/internal/config"
	"toft/internal/database"
)

// Runtime is the *resolved* configuration: what the pipeline is actually built
// from, after the environment and the saved settings document have been
// reconciled. Nothing downstream of Resolve knows which of the two it came from
// except through Source, which exists only so a screen can say so.
type Runtime struct {
	// Endpoint is the collector's base URL. The signal path (/v1/traces,
	// /v1/metrics) is appended by the exporters, not stored here.
	Endpoint string
	Headers  map[string]string
	// Certificate is a CA bundle path for a collector behind a private CA.
	// Environment-only: it names a file on the host, and a settings field that
	// pointed the platform at an arbitrary path on its own disk is a different
	// question from one that names a URL.
	Certificate string
	Timeout     time.Duration

	Traces      bool
	Metrics     bool
	SampleRatio float64

	MetricInterval time.Duration
	Temporality    string

	RecordEnduserID   bool
	AppCardinalityCap int

	// Source is `settings` or `environment` — which of the two produced this,
	// for the admin card and the startup log. Empty when nothing is configured.
	Source string
}

// Where a resolved configuration came from.
const (
	SourceSettings    = "settings"
	SourceEnvironment = "environment"
)

// Enabled reports whether anything is exported at all.
func (r Runtime) Enabled() bool { return r.Endpoint != "" && (r.Traces || r.Metrics) }

// Resolve reconciles the environment with the saved document.
//
// **A saved document wins.** That is the opposite of the sign-in providers'
// precedence, where the environment shadows a database row, and the difference
// is not an inconsistency: an environment-configured provider wins because it
// exists to solve "nobody can sign in to fix the database row", a bootstrap
// problem telemetry does not have. Nobody is ever locked out of a collector, so
// the surface an administrator can actually reach is the one that should decide.
//
// An install that has never saved the document runs on OTEL_* exactly as it did
// before the document existed, which is what keeps every existing deployment
// working and every runbook true.
//
// headers is the *decrypted* credential, or "" — only the API layer holds the
// encryptor, so this function never sees ciphertext.
func Resolve(env config.Telemetry, doc database.TelemetrySettings, headers string) Runtime {
	// Carried from the environment whichever source wins. These four are not on
	// the document at all: two are host facts (a CA bundle path, the export
	// timeout), one is a privacy choice an operator makes once, and one is a
	// capacity bound that belongs with the deployment that sized the install.
	base := Runtime{
		Certificate:       env.Certificate,
		Timeout:           env.Timeout,
		MetricInterval:    env.MetricExportInterval,
		RecordEnduserID:   env.RecordEnduserID,
		AppCardinalityCap: env.AppCardinalityCap,
	}

	if doc.Configured {
		r := base
		r.Source = SourceSettings
		if !doc.Enabled {
			// Off means off, and it means off *despite* the environment. An
			// administrator who switches export off and then finds it still
			// exporting because a variable is set has been lied to by a switch.
			return r
		}
		r.Endpoint = doc.Endpoint
		r.Headers = parseHeaders(headers)
		r.Traces = doc.Traces
		r.Metrics = doc.Metrics
		r.SampleRatio = doc.SampleRatio
		r.Temporality = doc.Temporality
		return r
	}

	r := base
	r.Source = SourceEnvironment
	r.Endpoint = env.Endpoint
	r.Headers = parseHeaders(env.Headers)
	r.Traces = env.TracesEnabled()
	r.Metrics = env.MetricsEnabled()
	r.SampleRatio = env.SamplerArg
	r.Temporality = env.Temporality
	return r
}

// parseHeaders reads the `key=value,key=value` form OTEL_EXPORTER_OTLP_HEADERS
// uses, which is also what the settings field stores — one format, so a
// credential pasted from a runbook works in either place.
//
// A malformed entry is skipped rather than failing the whole set: this runs on
// a settings save and on every restart, and refusing to export because one
// header had no `=` in it would be a worse outcome than exporting without it.
func parseHeaders(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// signalURL appends a signal path to a collector base URL.
//
// The exporters' WithEndpointURL takes a *full* URL and sets the path to "/"
// when it is given none — so handing it a bare base would POST every span to
// the collector's root and get a 404 that reads like a network problem. Joining
// here keeps the stored value the thing an operator recognises (the base URL,
// which is what OTEL_EXPORTER_OTLP_ENDPOINT is) rather than a per-signal URL
// they would have to know to write.
func signalURL(base, path string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	// A base that already carries a path is honoured — some collectors sit
	// behind a prefix — and the signal path is appended to it.
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	return u.String()
}

// EndpointHost renders a collector URL as scheme://host:port.
//
// The path is dropped, and so is everything after it. What an administrator
// needs from a status line is "which box are we talking to" — enough to
// recognise the wrong one — and every extra character is a chance to put
// something on a screen that has no business being there.
func EndpointHost(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host
}
