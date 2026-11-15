package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/internal/artifact"
)

// splitStore is an Uploader whose upload waits for release, so a test can
// show the deploy goes on while the upload is still running.
type splitStore struct {
	artifact.Store
	staging string
	release chan error
}

func (s splitStore) Stage(ctx context.Context, dir string) (*artifact.Staged, error) {
	return artifact.StageTo(ctx, s.staging, dir)
}

func (s splitStore) Upload(ctx context.Context, key string, st *artifact.Staged) (artifact.Descriptor, error) {
	if err := <-s.release; err != nil {
		return artifact.Descriptor{}, err
	}
	return artifact.Descriptor{Key: key, Size: st.Size, SHA256: st.SHA256}, nil
}

func TestAPublishBehindPacksFirstAndUploadsWhileTheCallerGoesOn(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "app.py"), []byte("print(1)"), 0644); err != nil {
		t.Fatal(err)
	}
	store := splitStore{staging: t.TempDir(), release: make(chan error, 1)}
	b := &Builder{artifacts: store}

	p, err := b.publishBehind("app", "dep", tree, func(string) {})
	if err != nil || p == nil {
		t.Fatalf("publishBehind = %v, %v", p, err)
	}
	// It returned with the upload still blocked: the start can go ahead.
	// And the tree is already packed, so a write the started app makes now is
	// not in the artifact.
	if p.key != artifact.ReleaseKey("app", "dep") {
		t.Errorf("key = %q", p.key)
	}
	store.release <- nil
	desc, err := p.wait(func(string) {})
	if err != nil || desc.Size == 0 || desc.SHA256 == "" {
		t.Fatalf("wait = %+v, %v", desc, err)
	}
	if left, _ := os.ReadDir(store.staging); len(left) != 0 {
		t.Errorf("the packed file was left in staging: %v", left)
	}
}

func TestAPublishBehindThatFailsToUploadIsTheDeploysFailure(t *testing.T) {
	store := splitStore{staging: t.TempDir(), release: make(chan error, 1)}
	b := &Builder{artifacts: store}
	p, err := b.publishBehind("app", "dep", t.TempDir(), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	store.release <- errors.New("bucket gone")
	if _, err := p.wait(func(string) {}); err == nil || !strings.Contains(err.Error(), "bucket gone") {
		t.Errorf("wait = %v, want the upload's error", err)
	}
}

func TestAPublishBehindThatNeverFinishesFailsAtItsDeadline(t *testing.T) {
	was := publishTimeout
	publishTimeout = 200 * time.Millisecond
	t.Cleanup(func() { publishTimeout = was })
	store := splitStore{staging: t.TempDir(), release: make(chan error)}
	b := &Builder{artifacts: store}
	p, err := b.publishBehind("app", "dep", t.TempDir(), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.wait(func(string) {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wait = %v, want the deadline", err)
	}
}

// A store with no network half (or none at all) publishes as before.
func TestAPublishBehindWithAStoreThatCannotSplitDeclines(t *testing.T) {
	b := &Builder{artifacts: blockingStore{}}
	if p, err := b.publishBehind("app", "dep", t.TempDir(), func(string) {}); p != nil || err != nil {
		t.Errorf("publishBehind = %v, %v, want nil, nil", p, err)
	}
}

// The upload goroutine cancels its context as it returns, so a finished upload
// is a done channel and a done context at once. Run enough times that a random
// pick between the two would lose.
func TestAFinishedUploadIsNotReportedAsItsDeadline(t *testing.T) {
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		p := &pendingPublish{key: "k", began: time.Now(), done: make(chan publishResult, 1), ctx: ctx}
		p.done <- publishResult{desc: artifact.Descriptor{Key: "k"}}
		cancel()
		if _, err := p.wait(func(string) {}); err != nil {
			t.Fatalf("run %d: a finished upload failed: %v", i, err)
		}
	}
}

// stallingStage is an Uploader whose Stage blocks past the deadline — a read
// stuck inside one file on a network file system — and then finishes.
type stallingStage struct {
	artifact.Store
	staging string
	release chan struct{}
	staged  chan string
}

func (s stallingStage) Stage(ctx context.Context, dir string) (*artifact.Staged, error) {
	<-s.release
	st, err := artifact.StageTo(context.Background(), s.staging, dir)
	if err == nil {
		s.staged <- st.SHA256
	}
	return st, err
}

func (s stallingStage) Upload(ctx context.Context, key string, st *artifact.Staged) (artifact.Descriptor, error) {
	return artifact.Descriptor{Key: key}, nil
}

// A Stage that never answers fails the deploy at the publish deadline rather
// than leaving it building for good, and the file it packs afterwards is
// removed (AS-6).
func TestAPublishBehindWhosePackingStallsFailsAtItsDeadline(t *testing.T) {
	was := publishTimeout
	publishTimeout = 200 * time.Millisecond
	t.Cleanup(func() { publishTimeout = was })
	store := stallingStage{staging: t.TempDir(), release: make(chan struct{}), staged: make(chan string, 1)}
	b := &Builder{artifacts: store}

	began := time.Now()
	_, err := b.publishBehind("app", "dep", t.TempDir(), func(string) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("publishBehind = %v, want the deadline", err)
	}
	if time.Since(began) > 5*time.Second {
		t.Errorf("returned after %s", time.Since(began))
	}

	close(store.release)
	<-store.staged
	deadline := time.Now().Add(5 * time.Second)
	for {
		left, _ := os.ReadDir(store.staging)
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the late packed file was left in staging: %v", left)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// keyedStore is a splitStore that remembers what landed, so a test can see a
// failed deploy's object removed.
type keyedStore struct {
	splitStore
	mu   *sync.Mutex
	keys map[string]bool
}

func (s keyedStore) Upload(ctx context.Context, key string, st *artifact.Staged) (artifact.Descriptor, error) {
	select {
	case err := <-s.release:
		if err != nil {
			return artifact.Descriptor{}, err
		}
	case <-ctx.Done():
		return artifact.Descriptor{}, ctx.Err()
	}
	s.mu.Lock()
	s.keys[key] = true
	s.mu.Unlock()
	return artifact.Descriptor{Key: key, Size: st.Size, SHA256: st.SHA256}, nil
}

func (s keyedStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.keys[key] {
		return artifact.ErrNotFound
	}
	delete(s.keys, key)
	return nil
}

func (s keyedStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[key]
}

// A deploy that fails before its flip removes the artifact it published: the
// row records a key only on success, so nothing else ever would while
// auto-delete is off (security review 2026-11-14, PB-7). Both when the upload
// had landed and when it was still running.
func TestADiscardedPublishLeavesNoObject(t *testing.T) {
	newStore := func() keyedStore {
		return keyedStore{splitStore: splitStore{staging: t.TempDir(), release: make(chan error, 1)},
			mu: &sync.Mutex{}, keys: map[string]bool{}}
	}
	key := artifact.ReleaseKey("app", "dep")

	t.Run("landed", func(t *testing.T) {
		store := newStore()
		b := &Builder{artifacts: store}
		p, err := b.publishBehind("app", "dep", t.TempDir(), func(string) {})
		if err != nil || p == nil {
			t.Fatalf("publishBehind = %v, %v", p, err)
		}
		store.release <- nil
		if _, err := p.wait(func(string) {}); err != nil {
			t.Fatal(err)
		}
		if !store.has(key) {
			t.Fatal("the upload did not land")
		}
		b.discardPublished(p, "")
		if store.has(key) {
			t.Error("the failed deploy's object is still in the store")
		}
	})

	t.Run("still uploading", func(t *testing.T) {
		store := newStore()
		b := &Builder{artifacts: store}
		p, err := b.publishBehind("app", "dep", t.TempDir(), func(string) {})
		if err != nil || p == nil {
			t.Fatalf("publishBehind = %v, %v", p, err)
		}
		b.discardPublished(p, "")
		store.release <- nil // too late: the upload was cancelled
		if store.has(key) {
			t.Error("an object landed after the discard")
		}
	})

	t.Run("published synchronously", func(t *testing.T) {
		store := newStore()
		store.keys[key] = true
		b := &Builder{artifacts: store}
		b.discardPublished(nil, key)
		if store.has(key) {
			t.Error("the synchronously published object is still in the store")
		}
	})
}
