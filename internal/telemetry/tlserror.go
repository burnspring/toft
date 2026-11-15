package telemetry

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
)

// isTLSError recognises the certificate and handshake failures an operator
// pointing Toft at a collector behind a private CA will actually hit.
//
// Its own file because it is the one classification in errortype.go that needs
// crypto/tls and crypto/x509, and because it is the one an operator sees most:
// OTEL_EXPORTER_OTLP_CERTIFICATE exists precisely for the install where this
// returns true.
func isTLSError(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	var recordHeader tls.RecordHeaderError
	var alert tls.AlertError
	return errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostname) ||
		errors.As(err, &certInvalid) ||
		errors.As(err, &recordHeader) ||
		errors.As(err, &alert)
}
