package api

import (
	"context"
	"log"
	"time"
)

// storageDeletionEvery is how often a delete in flight is looked at. The
// cluster's own deletion is usually minutes; a finished job pod can hold a
// claim for an hour. Thirty seconds frees a slug soon after the cluster lets
// go without asking it anything worth noticing.
const storageDeletionEvery = 30 * time.Second

// FinishStorageDeletions is the reconciler for storage_deletions (ADR §12a):
// at boot and then every storageDeletionEvery, each delete in flight is either
// dispatched — the destroy never reached the cluster, or was cut short — or
// checked, and finished once every volume it flipped is gone. main starts it;
// it returns only when the process does.
func (s *Server) FinishStorageDeletions() {
	if s.storage == nil {
		return
	}
	s.finishStorageDeletionsOnce()
	for range time.Tick(storageDeletionEvery) {
		s.finishStorageDeletionsOnce()
	}
}

func (s *Server) finishStorageDeletionsOnce() {
	all, err := s.db.ListStorageDeletions()
	if err != nil {
		log.Printf("storage deletions: listing: %v", err)
		return
	}
	for _, d := range all {
		// Finished, and holding its name only because its files were kept.
		if d.Kept {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), storageCallTimeout)
		// No report means the destroy never finished being dispatched: run it
		// again, which is safe because every step of it is.
		if d.Report == nil {
			if _, _, err := s.dispatchStorageDeletion(ctx, d.Slug); err != nil {
				log.Printf("storage deletions: destroying %s: %v", d.Slug, err)
			}
			cancel()
			continue
		}
		gone, err := s.storage.VolumesGone(ctx, d.Volumes)
		cancel()
		if err != nil {
			log.Printf("storage deletions: checking %s: %v", d.Slug, err)
			continue
		}
		if !gone {
			continue
		}
		if err := s.db.FinishStorageDeletion(d.Slug); err != nil {
			log.Printf("storage deletions: finishing %s: %v", d.Slug, err)
		}
	}
}
