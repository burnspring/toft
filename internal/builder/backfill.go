package builder

import (
	"context"
	"errors"
	"log"
	"time"

	"toft/internal/artifact"
)

// backfillTimeout bounds one Stat against the artifact store. A backfill is
// housekeeping — if the store is slow or down, the number stays unknown and
// the next boot tries again, which is strictly better than holding the pass
// open behind an outage.
const backfillTimeout = 10 * time.Second

// BackfillReleaseSizes measures releases deployed before sizes were recorded,
// so an install that upgrades into this feature does not have to redeploy every
// app to find out what its disk is going to.
//
// Deliberately best-effort and out of band. A release is measured from
// whichever of its two homes still answers — the directory on this host, the
// artifact in the store — and a release that has neither is left unmeasured
// rather than credited with zero. Unmeasured rows are retried on the next boot,
// which costs one cheap existence check each; only rows that actually published
// an artifact cost a store round-trip.
func (b *Builder) BackfillReleaseSizes() {
	deploys, err := b.db.DeploymentsMissingSizes()
	if err != nil {
		log.Printf("release sizes: listing unmeasured deployments: %v", err)
		return
	}
	if len(deploys) == 0 {
		return
	}

	measured := 0
	for _, d := range deploys {
		disk := measureReleaseDir(b.opts.AppsDir, d.AppID, d.ID)

		var packed *int64
		// An empty key means this deployment published nothing — an install
		// with no artifact store, or a rollback, which serves the artifact its
		// source published rather than one of its own. Neither is worth asking
		// the store about.
		if b.artifacts != nil && d.ArtifactKey != "" {
			ctx, cancel := context.WithTimeout(context.Background(), backfillTimeout)
			desc, err := b.artifacts.Stat(ctx, d.ArtifactKey)
			cancel()
			switch {
			case err == nil:
				packed = &desc.Size
			case errors.Is(err, artifact.ErrNotFound):
				// Genuinely gone. Nothing to measure, and nothing wrong.
			default:
				// Unreachable is not "absent" — say so once and move on, so an
				// outage does not read as a fleet of vanished releases.
				log.Printf("release sizes: reading %s from the store: %v", d.ArtifactKey, err)
			}
		}

		if disk == nil && packed == nil {
			continue
		}
		if err := b.db.SetDeploymentSizes(d.ID, disk, packed); err != nil {
			log.Printf("release sizes: recording %s: %v", d.ID, err)
			continue
		}
		measured++
	}

	if measured > 0 {
		log.Printf("Measured %d previously unrecorded release(s)", measured)
	}
}
