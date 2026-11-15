package advisory

import (
	"path/filepath"
	"testing"
	"time"
)

type recordingStore struct{ replaced int }

func (s *recordingStore) ReplaceSnapshot(string, string, []Record, FeedState) error {
	s.replaced++
	return nil
}
func (s *recordingStore) RecordSyncFailure(string, string, string) error { return nil }
func (s *recordingStore) FeedStates() ([]FeedState, error)               { return nil, nil }

// DG-3 (security review 2026-11-14): an archive with no record for the target
// ecosystem — empty, or another ecosystem's — never reaches ReplaceSnapshot,
// which would delete the corpus and record a fresh success.
func TestImportOfTheWrongArchiveNeverReplacesTheCorpus(t *testing.T) {
	dir := t.TempDir()
	pypi := filepath.Join(dir, "pypi.zip")
	writeZip(t, pypi, map[string]string{
		"PYSEC-1.json": `{"id":"PYSEC-1","affected":[{"package":{"ecosystem":"PyPI","name":"flask"},
		  "ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"2.0"}]}]}]}`,
	})
	empty := filepath.Join(dir, "empty.zip")
	writeZip(t, empty, map[string]string{})

	store := &recordingStore{}
	svc := New(store, nil)
	for _, path := range []string{pypi, empty} {
		if _, err := svc.Import(path, EcosystemNPM, time.Now()); err == nil {
			t.Errorf("%s imported as npm without an error", filepath.Base(path))
		}
	}
	if store.replaced != 0 {
		t.Errorf("ReplaceSnapshot called %d time(s), want none", store.replaced)
	}
}
