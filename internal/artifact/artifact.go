// Package artifact stores built release trees so that a release survives the
// machine that built it.
//
// Until now a release was a directory under APPS_DIR, which is durable on a VM
// only by accident: inside a container the apps directory is ephemeral, so
// replacing the task silently destroys every release. Packing a built tree and
// putting it somewhere durable is what makes an ECS-EC2 install supportable,
// and it is the same fetchable thing an isolated workload would need to unpack
// into its own container filesystem.
//
// Object storage was chosen over a shared network filesystem deliberately.
// Dependency trees are tens of thousands of small files — the pathological
// case for NFS, and exactly what Python and Node builds produce. A network
// filesystem makes boot fast and every subsequent read slow; object storage
// plus restore-on-boot makes boot slower and steady-state reads local-disk.
// Boots are rare and reads are constant, so this trades the right way.
package artifact

import (
	"context"
	"errors"
	"time"
)

// The three failure modes a caller must distinguish. Anything else is a bug in
// the store, not a condition to handle.
var (
	// ErrNotFound means the store answered, and does not have this key. A
	// pruned release and a wiped bucket both look like this, and both mean
	// "only a fresh deploy can bring the app back."
	ErrNotFound = errors.New("artifact not found")
	// ErrUnreachable means the store could not be asked. Distinct from
	// ErrNotFound because it is retryable and must never be mistaken for a
	// missing release — treating an outage as "the release is gone" would
	// have the reconciler give up on every app at once.
	ErrUnreachable = errors.New("artifact store unreachable")
	// ErrIntegrity means bytes came back that are not the bytes that were
	// stored. Unpacking them would give an app a corrupt tree that fails in
	// some unrelated way later.
	ErrIntegrity = errors.New("artifact integrity check failed")
)

// Descriptor is what a Put produced: enough to fetch the artifact back and to
// prove it came back intact.
type Descriptor struct {
	// Key addresses the artifact in the store. It is what a release reference
	// carries.
	Key string `json:"key"`
	// Size is the packed byte count, for reporting and for a cheap
	// mismatch check before hashing.
	Size int64 `json:"size"`
	// SHA256 is over the packed bytes. Get verifies against it, which is what
	// makes ErrIntegrity possible at all.
	SHA256 string `json:"sha256"`
	// StoredAt is when the artifact was written, and is what expiry is
	// measured from.
	StoredAt time.Time `json:"stored_at"`
}

// Store holds packed release trees. Implementations are a local filesystem
// (a VM install, which needs no object store, registry, or cloud account) and
// an object store (which is what makes releases durable under containers).
//
// Keys are opaque to callers; build one with ReleaseKey so that expiry and
// listing can rely on their shape.
type Store interface {
	// Put packs dir and stores it under key, replacing anything already there.
	// The returned descriptor is what a caller persists.
	Put(ctx context.Context, key, dir string) (Descriptor, error)
	// Get fetches the artifact, verifies it against the stored digest, and
	// unpacks it into dest. dest is created if absent and is left untouched if
	// anything fails, so a failed restore never leaves a half-tree an app
	// could be started from.
	Get(ctx context.Context, key, dest string) error
	// Stat reports the descriptor without transferring the artifact.
	Stat(ctx context.Context, key string) (Descriptor, error)
	// Delete removes an artifact. Deleting an absent key is not an error —
	// pruning must be idempotent.
	Delete(ctx context.Context, key string) error
	// Expire removes artifacts for an app other than the ones named as kept.
	// It is how release pruning reaches the store, and it takes the keep set
	// rather than an age so that it matches the on-disk prune exactly.
	Expire(ctx context.Context, appID string, keep []string) (int, error)
}

// ReleaseKey addresses one app's release. The app-scoped prefix is what lets
// Expire enumerate an app's artifacts without listing the whole store.
func ReleaseKey(appID, deploymentID string) string {
	return "releases/" + appID + "/" + deploymentID + ".tar.gz"
}

// releasePrefix is the listing prefix for one app's releases.
func releasePrefix(appID string) string {
	return "releases/" + appID + "/"
}
