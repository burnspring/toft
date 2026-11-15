//go:build unix

package deps

import (
	"context"
	"testing"
	"time"
)

// A resolver that leaves a grandchild holding its output pipe must not keep the
// build worker past the timeout: the whole group is killed, and WaitDelay
// bounds whatever is left.
func TestAResolverTimeoutEndsItsWholeTree(t *testing.T) {
	oldTimeout, oldDelay := resolveTimeout, waitDelay
	resolveTimeout, waitDelay = 300*time.Millisecond, 2*time.Second
	t.Cleanup(func() { resolveTimeout, waitDelay = oldTimeout, oldDelay })

	start := time.Now()
	// The background sleep inherits stdout, which is the pipe Run waits on.
	_, err := output(context.Background(), t.TempDir(), "sh", "-c", "sleep 60 & sleep 60")
	if err == nil {
		t.Fatal("a command past its timeout returned no error")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("output returned after %s, want the tree ended at the timeout", took)
	}
}
