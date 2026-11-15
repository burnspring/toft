package telemetry

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// tlsConfigFromFile builds a client TLS configuration trusting one CA bundle.
//
// This is what OTEL_EXPORTER_OTLP_CERTIFICATE is for: a collector behind an
// internal CA, which is the normal shape in a datacentre and the one case where
// an export fails with a certificate error that reads like a network problem.
//
// It stays environment-only rather than joining the settings document, and the
// reason is the argument type. The endpoint is a URL — a thing an administrator
// names. This is a *path on the platform's own disk*, and a settings field that
// let a browser point the platform at an arbitrary local file is a different
// question with a worse answer.
//
// The system pool is loaded first and the bundle added to it, rather than
// replacing it: an install that trusts an internal CA has not stopped trusting
// the public ones, and a collector that moved to a public certificate should
// not start failing because of a setting nobody remembers.
func tlsConfigFromFile(path string) (*tls.Config, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the CA bundle: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s contains no PEM certificate", path)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
