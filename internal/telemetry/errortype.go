package telemetry

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"syscall"
)

// The closed enum of error classes.
//
// `error.type` is a metric attribute, so its domain has to be finite and known
// in advance. The obvious implementations are both wrong: err.Error() is
// unbounded free text and would also carry secrets onto a metric, where nothing
// scrubs it; the concrete Go type name is bounded only by however many error
// types the dependency tree defines, which is not a bound anybody has counted.
//
// So: fifteen classes an operator can actually act on. "Which of my calls are
// timing out" is a question worth a series. "Which of the eleven ways
// net/http spells a broken pipe occurred" is not.
const (
	ErrNone              = ""
	ErrTimeout           = "timeout"
	ErrCanceled          = "canceled"
	ErrConnectionRefused = "connection_refused"
	ErrConnectionReset   = "connection_reset"
	ErrDNS               = "dns"
	ErrTLS               = "tls"
	ErrNetwork           = "network"
	ErrEOF               = "eof"
	ErrNotFound          = "not_found"
	ErrPermission        = "permission"
	ErrExists            = "exists"
	ErrNoSpace           = "no_space"
	ErrHTTPStatus        = "http_status"
	ErrOther             = "other"
)

// errorTypeValues is the enum, for the cardinality budget and the snapshot.
var errorTypeValues = []string{
	ErrTimeout, ErrCanceled, ErrConnectionRefused, ErrConnectionReset, ErrDNS,
	ErrTLS, ErrNetwork, ErrEOF, ErrNotFound, ErrPermission, ErrExists,
	ErrNoSpace, ErrHTTPStatus, ErrOther,
}

// ErrorType classifies an error into the enum above. Never the error's text,
// and never its type name.
func ErrorType(err error) string {
	if err == nil {
		return ErrNone
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return ErrTimeout
	case errors.Is(err, context.Canceled):
		return ErrCanceled
	case errors.Is(err, syscall.ECONNREFUSED):
		return ErrConnectionRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return ErrConnectionReset
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return ErrNoSpace
	case errors.Is(err, fs.ErrNotExist):
		return ErrNotFound
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES):
		return ErrPermission
	case errors.Is(err, fs.ErrExist):
		return ErrExists
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return ErrEOF
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ErrDNS
	}
	// Checked after the sentinels above so a timeout that happens to be wrapped
	// in a net.Error still classifies as a timeout rather than as "network".
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ErrTimeout
		}
		return ErrNetwork
	}
	if isTLSError(err) {
		return ErrTLS
	}
	return ErrOther
}

// StatusErrorType is the HTTP flavour: a response is an error at 5xx, and a 4xx
// is the caller's problem rather than the platform's.
//
// It is separate from ErrorType because a handler that returns 500 usually
// returns no Go error at all — it has already written the response — and a
// series that only counted transport failures would report a platform serving
// nothing but 500s as perfectly healthy.
func StatusErrorType(status int) string {
	if status >= 500 {
		return ErrHTTPStatus
	}
	return ErrNone
}
