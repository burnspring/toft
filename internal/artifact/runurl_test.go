package artifact

import (
	"strings"
	"testing"
	"time"
)

// A run's URLs live as long as the run can use them, and not six hours past it.
func TestRunURLTTLFollowsTheRun(t *testing.T) {
	cases := []struct {
		timeout, want time.Duration
	}{
		{30 * time.Second, 30*time.Second + runURLMargin},
		{time.Hour, time.Hour + runURLMargin},
		{0, PresignTTL},
		{24 * time.Hour, PresignTTL},
	}
	for _, c := range cases {
		if got := RunURLTTL(c.timeout); got != c.want {
			t.Errorf("RunURLTTL(%v) = %v, want %v", c.timeout, got, c.want)
		}
	}
}

// The secret halves of a presigned URL never reach a stored log line; the
// rest of it stays readable, and a line that only looks similar is untouched.
func TestMaskSignedURLs(t *testing.T) {
	line := "TOFT_OUTPUT_URI=https://b.s3.amazonaws.com/runs/a/r/outputs/output?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=ASIAX%2F20261021%2Fus-east-1&X-Amz-Date=20261021T215300Z&X-Amz-Expires=600" +
		"&X-Amz-Security-Token=IQoJb3JpZ2lu%2Bxyz&X-Amz-SignedHeaders=host&X-Amz-Signature=abc123def"
	got := MaskSignedURLs(line)
	for _, secret := range []string{"ASIAX", "IQoJb3JpZ2lu", "abc123def"} {
		if strings.Contains(got, secret) {
			t.Errorf("masked line still holds %q: %s", secret, got)
		}
	}
	for _, kept := range []string{"runs/a/r/outputs/output", "X-Amz-Expires=600", "X-Amz-Signature=REDACTED"} {
		if !strings.Contains(got, kept) {
			t.Errorf("masked line lost %q: %s", kept, got)
		}
	}
	sas := "https://acct.blob.core.windows.net/c/k?sv=2024&se=2026&sig=SECRETSAS%3D&sp=r"
	if m := MaskSignedURLs(sas); strings.Contains(m, "SECRETSAS") {
		t.Errorf("SAS signature kept: %s", m)
	}
	for _, plain := range []string{"tick 3", "config sig=abc is fine", "a=b"} {
		if MaskSignedURLs(plain) != plain {
			t.Errorf("%q was rewritten to %q", plain, MaskSignedURLs(plain))
		}
	}
}
