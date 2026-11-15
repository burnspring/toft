package builder

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"toft/internal/artifact"
)

// blockingStore's Put never returns -- a read stuck inside one file on a
// network file system, which no context check can interrupt.
type blockingStore struct{ artifact.Store }

func (blockingStore) Put(ctx context.Context, key, dir string) (artifact.Descriptor, error) {
	select {}
}

// A publish that never finishes fails the deploy at its deadline, naming it.
// It used to leave the deployment "building" for good, with a log that ended
// at "Publishing release to the artifact store..." (2026-10-22, trytoft.dev).
func TestAPublishThatNeverFinishesFailsAtItsDeadline(t *testing.T) {
	was := publishTimeout
	publishTimeout = 200 * time.Millisecond
	t.Cleanup(func() { publishTimeout = was })

	b := &Builder{artifacts: blockingStore{}}
	start := time.Now()
	_, _, err := b.publishRelease("app", "dep", t.TempDir(), func(string) {})
	if err == nil {
		t.Fatal("a publish that never finished reported success")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("the failure does not say it timed out: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("returned after %s, well past its deadline", took)
	}
}
