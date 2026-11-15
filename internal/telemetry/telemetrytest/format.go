package telemetrytest

import (
	stdhex "encoding/hex"
	"strconv"
)

// Rendering helpers, in their own file so collector.go stays about the
// protocol. Everything a test asserts against is a string: a span attribute is
// an int here and a string there depending on which SDK helper produced it, and
// a test that had to know which is a test about the SDK.

func hex(b []byte) string { return stdhex.EncodeToString(b) }

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// ftoa formats without an exponent and without trailing zeros, so 0.5 reads as
// "0.5" rather than "5e-01" in a failure message.
func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
