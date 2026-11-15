package database

import (
	"database/sql"
	"errors"
	"fmt"
	gopath "path"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"toft/internal/runtime"
)

// Durable storage: admin-created resources, and the mappings that put one on an
// app's filesystem.
//
// Deliberately *not* object_grants rows. `object_grants.object_id` is an app id
// with no object-type discriminator, and mapping is admin-only rather than
// delegable — so the grant machinery buys nothing here, and generalizing it
// would cost a migration and a resolver change to express something simpler
// than what it already does.
//
// Every mutation writes audit_log in the same transaction, as every
// authorization-relevant mutation on this platform does.

// Resource kinds.
const (
	// StorageKindVolume is a real POSIX filesystem — an EFS access point. For
	// state: SQLite, caches, working files, anything appended to or renamed.
	StorageKindVolume = "volume"
	// StorageKindStore is an object-store prefix mounted with Mountpoint. For
	// blobs: datasets, model weights, reports. Whole-object semantics, and the
	// mount enforces them rather than documenting them.
	StorageKindStore = "store"
)

// Backings: what is underneath a resource.
//
// The kind is what an app's code experiences; the backing is what the operator
// pointed it at. They were one answer until a file store could be something
// other than EFS, and they are two now — which is why the data plane below
// asks about the bucket rather than about either of them.
const (
	// StorageBackingEFS is an EFS access point: a directory jail below the
	// workload, with no objects behind it and therefore no data plane.
	StorageBackingEFS = "efs"
	// StorageBackingS3Files is an Amazon S3 Files file system — a
	// general-purpose bucket mounted with NFS semantics. A file system to the
	// app, a bucket to the platform.
	StorageBackingS3Files = "s3files"
	// StorageBackingMountpoint is an object-store prefix mounted with Mountpoint
	// for Amazon S3. Spelled rather than left implicit in the kind, so no row
	// claims a backing it does not have — which stopped being a formality when
	// StorageBackingBlob arrived and a store was no longer one thing.
	StorageBackingMountpoint = "mountpoint"
	// StorageBackingHost is a directory under a root the operator mounted, on
	// an install with no cluster. It is what is *known* — the platform cannot
	// see through a mount, and an S3 Files export, an NFS server and a local
	// disk are the same directory from here — so it names no bucket and has no
	// data plane.
	StorageBackingHost = "host"
	// StorageBackingAzureFiles is an Azure Files share, provisioned from
	// KUBE_STORAGE_CLASS exactly as EFS is. It needs no class variable of its
	// own: the per-resource storage_class column already covers an install
	// offering two, and a class variable per cloud is how a configuration
	// surface becomes a matrix.
	//
	// **Its copy carries a condition the platform cannot check.** Azure Files
	// has two protocols, and only NFS 4.1 gives the real flock/fcntl locks the
	// SQLite and DuckDB recipe rests on; SMB does not deliver them for a
	// single-file embedded database under concurrency. The platform cannot see
	// through a mount — the same sentence StorageBackingHost is written about —
	// so it cannot detect which, and must not guess. Copy rather than a fourth
	// backing, because the two protocols are the same Toft code path: a
	// backing is what the operator pointed it at, not how they mounted it.
	StorageBackingAzureFiles = "azurefiles"
	// StorageBackingBlob is an Azure Blob container prefix mounted with
	// blobfuse2 — **the first store that is not a Mountpoint mount**. The kind
	// is still what the app experiences; what changes is which mount.
	//
	// blobfuse2 has no allow-delete and no allow-overwrite: it mounts read-only
	// or read-write, with no flag that permits creating an object while
	// forbidding the clobbering of one. So a `blob` store offers two of the
	// three levels as mappings — see StorageMountableAccessLevels — and `write`
	// stays available over the URL, where a SAS can express it.
	StorageBackingBlob = "blob"
)

// ErrStorageInUse means a resource still has mappings. Distinct from a generic
// failure so the API can answer 409 with the count rather than a 500.
var ErrStorageInUse = errors.New("storage resource still has mappings")

// The two storage caps. **Constants, not settings**: both hold in every mode, so
// an app's mappings deploy the same on a cluster and on a host, and an install
// that could raise them would be one whose apps stop moving between installs.
//
// The per-app cap is also what keeps an app inside NFS on a host root. With
// AUTH_SYS a request carries at most 16 gids, the primary included; toft-apps
// and the account's own group take two, so 14 gates is the protocol's ceiling.
// Past it a server that does not look groups up for itself refuses access
// while every mode bit Toft can check says yes — so a test holds this at 14 or
// under, and a refusal at the mapping is what an app over it gets instead.
//
// Folders inside one partitioned resource are not capped separately: they are
// bounded by the apps that map them and by the gid range.
const (
	MaxStorageResources   = 25
	MaxAppStorageMappings = 5
)

// ErrStorageResourceCap and ErrAppMappingCap are the caps' refusals, distinct
// so the API can answer 422 with the sentence. An install or an app already
// over a cap keeps what it has; only the next one is refused.
var (
	ErrStorageResourceCap = fmt.Errorf("an install holds at most %d storage resources", MaxStorageResources)
	ErrAppMappingCap      = fmt.Errorf("an app holds at most %d storage mappings", MaxAppStorageMappings)
)

// ErrStorageBindingClash is a new resource whose cluster binding name is one an
// existing resource of the other kind already uses: volume reports-full and
// store reports both mean the claim lpv-reports-full (runtime.BindingNamesClash).
// Errors of this kind are *StorageBindingClashError, which names the other.
var ErrStorageBindingClash = errors.New("this name would share a cluster object with another storage resource")

// StorageBindingClashError names the volume and the store that would share a
// binding, whichever of the two is the one being created.
type StorageBindingClashError struct{ Volume, Store, Other string }

func (e *StorageBindingClashError) Error() string {
	return fmt.Sprintf("a volume called %s and the store %s would share one cluster object — choose another name", e.Volume, e.Store)
}
func (e *StorageBindingClashError) Is(target error) bool { return target == ErrStorageBindingClash }

// StorageResource is a durable location an administrator has named.
type StorageResource struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Backing is what is underneath: `efs`, `s3files`, `host` or `azurefiles`
	// for a volume, `mountpoint` or `blob` for a store. Stated by the
	// administrator at creation and
	// never changed afterwards — the bytes of two backings live in two
	// different services, and moving them is the operator's job with the
	// operator's tools.
	Backing string `json:"backing"`
	// StorageClass names the class this resource is provisioned from, and
	// empty means the install's default. It is on the resource because
	// otherwise an install has exactly one filesystem; the operator still
	// creates every class and Toft still creates only bindings.
	StorageClass string `json:"storage_class,omitempty"`
	// Root names the host directory this resource lives under on an install
	// with no cluster. Empty means it is bound through the isolated backend,
	// which is what every resource on a cluster install means.
	Root string `json:"storage_root,omitempty"`
	// Partitioned is a resource mapped only by folder, whose folders never
	// overlap, and on a host root each folder is its own gate. Fixed at
	// creation; see migration 00057.
	Partitioned bool `json:"partitioned"`
	// Bucket and Prefix are the objects behind a resource — every store, and a
	// file store backed by S3 Files. The prefix *is* the isolation boundary,
	// enforced by the mount below the pod.
	Bucket string `json:"bucket,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	// ClaimName is the backend object this resource binds through.
	ClaimName   string `json:"claim_name"`
	Description string `json:"description"`
	CreatedBy   string `json:"created_by,omitempty"`
	// MappingCount is filled by the listing so an admin sees what a resource is
	// used by before deleting it.
	MappingCount int `json:"mapping_count"`
	// GrantUsers and GrantGroups are the other half of that question: a mapping
	// is an app's and a grant is a person's, so a resource used by no app can
	// still be reachable by six people holding `full` from outside. Counted the
	// same way AppSummary counts an app's audience, and rendered in the same
	// words, because "who can reach this" is one question wherever it is asked.
	//
	// Both are 0 on a volume and the listing does not special-case that: grants
	// are stores-only, so the absence is a property of the kind rather than a
	// count that happens to be empty, and the UI says so by printing nothing.
	GrantUsers  int `json:"grant_users"`
	GrantGroups int `json:"grant_groups"`
	// ManagerUsers and ManagerGroups are the third audience, and the newest:
	// who may *attach* this without being an administrator. Counted the same
	// way and for the same reason — a resource nobody mounts and nobody has a
	// grant on can still be attachable by a team, and the row above Delete is
	// where that has to be readable.
	//
	// Unlike the grant counts these are non-zero on a volume: a volume takes no
	// grant because it has no data plane, and is very much attachable.
	ManagerUsers  int       `json:"manager_users"`
	ManagerGroups int       `json:"manager_groups"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// StorageMapping binds one resource to one app at one path.
type StorageMapping struct {
	ID         string `json:"id"`
	ResourceID string `json:"resource_id"`
	AppID      string `json:"app_id"`
	Path       string `json:"path"`
	Access     string `json:"access"`
	// Folder is which directory of the resource is mounted, and empty is the
	// resource's root — what every mapping written before migration 00017
	// means. It is part of the mapping's identity, so an app may mount two
	// folders of one resource at two paths and two levels.
	Folder string `json:"folder,omitempty"`
	// Authority is which delegation wrote this row — `admin`, `user:<id>` or
	// `group:<id>`. It exists so a revocation can collect exactly its own
	// mappings and nothing else; see internal/database/storage_managers.go.
	// An administrator's write stamps `admin` and adopts what it touches.
	Authority string `json:"authority"`
	// Source is MappingSourceConsole or MappingSourceManifest: who owns the
	// row's lifetime. A manifest mapping is written and removed by the deploy
	// whose toft.toml declares it; the console's is a person's until a person
	// detaches it.
	Source    string `json:"source"`
	CreatedBy string `json:"created_by,omitempty"`
	// Denormalized for display: a mappings list is read from both ends, and
	// neither view is useful without the other side's name.
	ResourceSlug string `json:"resource_slug,omitempty"`
	ResourceName string `json:"resource_name,omitempty"`
	ResourceKind string `json:"resource_kind,omitempty"`
	// ResourceRoot is which operator mount the resource lives under, carried
	// here so a shared-mode start reads one row rather than looking the
	// resource up again at the moment it is about to point a workload at it.
	ResourceRoot string `json:"resource_root,omitempty"`
	// ResourcePartitioned says each folder of the resource is its own gate.
	ResourcePartitioned bool   `json:"resource_partitioned,omitempty"`
	AppSlug             string `json:"app_slug,omitempty"`
	AppName             string `json:"app_name,omitempty"`
	// AuthorityName is the delegate's or group's name behind Authority, so the
	// admin table can say who attached it without a second lookup. Empty on an
	// `admin` stamp, and empty on a subject that has since been deleted — which
	// cannot happen while the row lives, since deleting a subject collects its
	// mappings too.
	AuthorityName string    `json:"authority_name,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// --- Resources ---

func (db *DB) ListStorageResources() ([]StorageResource, error) {
	rows, err := db.conn.Query(`
		SELECT r.id, r.slug, r.name, r.kind, r.backing, r.storage_class, r.storage_root, r.partitioned, r.bucket, r.prefix, r.claim_name, r.description,
		       COALESCE(r.created_by,''), r.created_at, r.updated_at,
		       (SELECT COUNT(*) FROM storage_mappings m WHERE m.resource_id = r.id),
		       (SELECT COUNT(*) FROM store_grants g
		         WHERE g.resource_id = r.id AND g.subject_type = 'user'),
		       (SELECT COUNT(*) FROM store_grants g
		         WHERE g.resource_id = r.id AND g.subject_type = 'group'),
		       (SELECT COUNT(*) FROM storage_managers sm
		         WHERE sm.resource_id = r.id AND sm.subject_type = 'user'),
		       (SELECT COUNT(*) FROM storage_managers sm
		         WHERE sm.resource_id = r.id AND sm.subject_type = 'group')
		FROM storage_resources r ORDER BY r.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StorageResource{}
	for rows.Next() {
		var r StorageResource
		if err := rows.Scan(&r.ID, &r.Slug, &r.Name, &r.Kind, &r.Backing, &r.StorageClass, &r.Root, &r.Partitioned, &r.Bucket, &r.Prefix, &r.ClaimName,
			&r.Description, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &r.MappingCount,
			&r.GrantUsers, &r.GrantGroups, &r.ManagerUsers, &r.ManagerGroups); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) GetStorageResource(id string) (*StorageResource, error) {
	r := &StorageResource{}
	err := db.conn.QueryRow(`
		SELECT r.id, r.slug, r.name, r.kind, r.backing, r.storage_class, r.storage_root, r.partitioned, r.bucket, r.prefix, r.claim_name, r.description,
		       COALESCE(r.created_by,''), r.created_at, r.updated_at,
		       (SELECT COUNT(*) FROM storage_mappings m WHERE m.resource_id = r.id),
		       (SELECT COUNT(*) FROM store_grants g
		         WHERE g.resource_id = r.id AND g.subject_type = 'user'),
		       (SELECT COUNT(*) FROM store_grants g
		         WHERE g.resource_id = r.id AND g.subject_type = 'group'),
		       (SELECT COUNT(*) FROM storage_managers sm
		         WHERE sm.resource_id = r.id AND sm.subject_type = 'user'),
		       (SELECT COUNT(*) FROM storage_managers sm
		         WHERE sm.resource_id = r.id AND sm.subject_type = 'group')
		FROM storage_resources r WHERE r.id = ?`, id).
		Scan(&r.ID, &r.Slug, &r.Name, &r.Kind, &r.Backing, &r.StorageClass, &r.Root, &r.Partitioned, &r.Bucket, &r.Prefix, &r.ClaimName,
			&r.Description, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &r.MappingCount,
			&r.GrantUsers, &r.GrantGroups, &r.ManagerUsers, &r.ManagerGroups)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// CreateStorageResource records a resource and audits it. The caller creates
// the backend's binding objects *after* this succeeds: a row with no PVC is
// repairable, a PVC with no row is invisible.
func (db *DB) CreateStorageResource(r *StorageResource, actorID, requestID, ip, userAgent string) error {
	r.ID = uuid.New().String()
	// A caller that names no backing gets the one its kind has always had, so
	// the column arriving does not change what an existing caller creates. The
	// CHECK behind this refuses the combination rather than trusting it, which
	// is what makes the default safe to apply here rather than in the handler.
	if r.Backing == "" {
		r.Backing = DefaultStorageBacking(r.Kind)
	}
	r.CreatedAt = time.Now().UTC()
	r.UpdatedAt = r.CreatedAt
	r.CreatedBy = actorID
	// The base binding name, for display and for an operator inspecting the
	// namespace by hand. A store's further access levels get their own bindings
	// derived from the same slug; see runtime.BindingName.
	r.ClaimName = runtime.BindingName(r.Slug, r.Kind, runtime.AccessRead, "")

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Counted under a lock that only another create takes, so two creates at
	// the 25th cannot both see room. A mapping's row lock on a resource does
	// not conflict with it.
	if _, err := tx.Exec(`LOCK TABLE storage_resources IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM storage_resources`).Scan(&count); err != nil {
		return err
	}
	if count >= MaxStorageResources {
		return ErrStorageResourceCap
	}
	// A volume's claim is lpv-<slug> and a store's lpv-<slug>-<level>[-<hex>],
	// in one namespace, so a volume named for a store's level would be that
	// store's binding and each ensure would adopt the other's claim. Checked
	// here, under the lock, against every resource of the other kind, so two
	// creates cannot race past it. Stores' names cannot collide with each other.
	otherRows, err := tx.Query(`SELECT slug FROM storage_resources WHERE kind <> ?`, r.Kind)
	if err != nil {
		return err
	}
	var others []string
	for otherRows.Next() {
		var slug string
		if err := otherRows.Scan(&slug); err != nil {
			otherRows.Close()
			return err
		}
		others = append(others, slug)
	}
	otherRows.Close()
	if err := otherRows.Err(); err != nil {
		return err
	}
	for _, other := range others {
		volume, store := r.Slug, other
		if r.Kind == StorageKindStore {
			volume, store = other, r.Slug
		}
		if runtime.BindingNamesClash(volume, store) {
			return &StorageBindingClashError{Volume: volume, Store: store, Other: other}
		}
	}

	// Under the same lock a delete's tombstone is written beside: a slug whose
	// previous resource's volumes the cluster is still deleting is not free,
	// because the new claim would provision onto the same directory.
	var kept bool
	switch err := tx.QueryRow(`SELECT kept FROM storage_deletions WHERE slug = ?`, r.Slug).Scan(&kept); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case kept:
		return ErrStorageDirectoryKept
	default:
		return ErrStorageDeleting
	}

	// One name: the slug is written into both columns, which the schema
	// requires (storage_resources_name_is_slug). The column stays so every
	// reader of `name` keeps reading the string apps and the CLI use.
	r.Name = r.Slug
	if _, err := tx.Exec(`
		INSERT INTO storage_resources (id, slug, name, kind, backing, storage_class, storage_root, partitioned, bucket, prefix, claim_name, description, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Slug, r.Name, r.Kind, r.Backing, r.StorageClass, r.Root, r.Partitioned, r.Bucket, r.Prefix, r.ClaimName, r.Description,
		nullIfEmpty(r.CreatedBy), r.CreatedAt, r.UpdatedAt); err != nil {
		return err
	}
	// A resource under a host root is gated by a group of its own, and its
	// number is decided here, in the same transaction, rather than by whichever
	// groupadd runs first on whichever host. See storage_gates.go. A
	// partitioned one has no gate of its own — its top level is traversable by
	// anyone and writable by nobody — and each folder gets one when it is first
	// mapped.
	if r.Root != "" && !r.Partitioned {
		if _, err := db.ensureGateTx(tx, resourceGateName(r.Slug), r.ID, ""); err != nil {
			return err
		}
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "storage_resource.create",
		TargetType: "storage_resource", TargetID: r.ID,
		After:     jsonStr(r),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	// The install's default delegations, materialised onto the new resource
	// in its own transaction (storage_defaults.go).
	if err := insertDefaultDelegationsTx(tx, r.ID, actorID, requestID, ip, userAgent); err != nil {
		return err
	}
	return tx.Commit()
}

// RenameStorageResource changes what a resource is *called* — its display name
// and its description — and nothing else.
//
// **The slug is not here, and that is the decision rather than an omission.**
// It is the stem of the binding name every mounted claim is derived from
// (`runtime.BindingName`), the directory under a shared storage root, and the
// permalink the browser page is reached by. Changing it would leave live
// workloads bound to claims named after a string the row no longer carries —
// data that is still on the disk and no longer reachable through the platform
// that put it there. An app's slug can move because the proxy resolves it per
// request; a volume's cannot, because a pod was told the name at start.
//
// So the two names diverge on purpose: the slug is the identity and the name is
// the label, and this renames the label. The page has to say so, because
// "rename" that leaves a visible identifier alone is a promise half kept if
// nobody explains which half.
//
// Audited whole, before and after, like the create above: what a store is
// called is what an administrator reads in a mapping dialog before granting
// somebody access to it.
func (db *DB) RenameStorageResource(id, name, description, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var before StorageResource
	if err := tx.QueryRow(
		`SELECT name, description FROM storage_resources WHERE id = ?`, id).
		Scan(&before.Name, &before.Description); err != nil {
		return err
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(
		`UPDATE storage_resources SET name=?, description=?, updated_at=? WHERE id=?`,
		name, description, now, id); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "storage_resource.rename",
		TargetType: "storage_resource", TargetID: id,
		Before:    jsonStr(map[string]string{"name": before.Name, "description": before.Description}),
		After:     jsonStr(map[string]string{"name": name, "description": description}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteStorageResource removes the record, refusing while mappings exist.
//
// It never touches the data. reclaimPolicy: Retain is what makes that true at
// the cluster level; refusing here is what stops an admin discovering it the
// hard way with a live app mid-read.
func (db *DB) DeleteStorageResource(id, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// The resource row first, locked. Every mapping insert or update takes
	// this same lock in its trigger (enforce_storage_mapping_partition), so
	// an attach or a deploy flip committing a mapping is waited for and then
	// seen by the check below — rather than cascaded away by the DELETE with
	// no audit row, and its bytes destroyed (security review 2026-11-14, DB-7).
	var locked int
	if err := tx.QueryRow(`SELECT 1 FROM storage_resources WHERE id=? FOR UPDATE`, id).Scan(&locked); err != nil {
		return err
	}

	// Named, not counted. Until delegation existed, an administrator reading
	// "3 app(s) still mount it" had mapped all three themselves; now they may
	// have mapped none of them, and a bare count reads as a bug rather than as
	// work somebody else did.
	rows, err := tx.Query(`
		SELECT apps.name, COALESCE(NULLIF(u.name,''), NULLIF(u.email,''), u.login, gr.name, '')
		FROM storage_mappings m
		JOIN apps ON apps.id = m.app_id
		LEFT JOIN users  u  ON m.authority = 'user:'  || u.id
		LEFT JOIN groups gr ON m.authority = 'group:' || gr.id
		WHERE m.resource_id = ? ORDER BY apps.name`, id)
	if err != nil {
		return err
	}
	var mounts []string
	for rows.Next() {
		var appName, by string
		if err := rows.Scan(&appName, &by); err != nil {
			rows.Close()
			return err
		}
		if by != "" {
			appName += " (attached by " + by + ")"
		}
		mounts = append(mounts, appName)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(mounts) > 0 {
		return fmt.Errorf("%w: %d app(s) still mount it — %s", ErrStorageInUse,
			len(mounts), strings.Join(mounts, ", "))
	}

	// The tombstone, in the same transaction as the row's delete: the slug is
	// held from the moment the resource stops existing, and the claims move
	// with it because the row's own go with the cascade. A second delete of
	// the same slug in flight is impossible — the row is gone — so the insert
	// cannot conflict.
	if _, err := tx.Exec(`
		INSERT INTO storage_deletions (slug, resource_id, kind, backing, storage_class, storage_root, claims, predates_claims, actor_id)
		SELECT r.slug, r.id, r.kind, r.backing, r.storage_class, r.storage_root,
		       COALESCE((SELECT jsonb_agg(c.claim ORDER BY c.claim) FROM storage_claims c WHERE c.resource_id = r.id), '[]'::jsonb),
		       r.created_at < COALESCE((SELECT min(g.tstamp) FROM goose_db_version g
		                                 WHERE g.version_id = 72 AND g.is_applied), '-infinity'),
		       -- A person, or nobody: the column is a foreign key to users.
		       (SELECT u.id FROM users u WHERE u.id = ?)
		  FROM storage_resources r WHERE r.id = ?`, actorID, id); err != nil {
		return err
	}

	// What was deleted, in the row's own before column: once the resource is
	// gone the Activity page cannot join to it, and this is the only place its
	// name and what it held are still written down.
	var before string
	if err := tx.QueryRow(`
		SELECT json_build_object('slug', d.slug, 'kind', d.kind, 'backing', d.backing,
		                         'storage_class', d.storage_class, 'storage_root', d.storage_root,
		                         'claims', d.claims)::text
		  FROM storage_deletions d WHERE d.resource_id = ?`, id).Scan(&before); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	deleted, err := tx.Exec(`DELETE FROM storage_resources WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := deleted.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err := retireGatesTx(tx, id); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "storage_resource.delete",
		TargetType: "storage_resource", TargetID: id,
		Before:    before,
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// --- Mappings ---

// ListStorageMappingsForResource answers "which apps mount this".
func (db *DB) ListStorageMappingsForResource(resourceID string) ([]StorageMapping, error) {
	return db.listMappings(`
		SELECT m.id, m.resource_id, m.app_id, m.path, m.access, m.folder, m.authority, m.source,
		       COALESCE(m.created_by,''), m.created_at, m.updated_at,
		       r.slug, r.name, r.kind, r.storage_root, r.partitioned, apps.slug, apps.name,
		       COALESCE(NULLIF(u.name,''), NULLIF(u.email,''), u.login, gr.name, '')
		FROM storage_mappings m
		JOIN storage_resources r ON r.id = m.resource_id
		JOIN apps ON apps.id = m.app_id
		LEFT JOIN users  u  ON m.authority = 'user:'  || u.id
		LEFT JOIN groups gr ON m.authority = 'group:' || gr.id
		WHERE m.resource_id = ? ORDER BY apps.name, m.folder`, resourceID)
}

// ListStorageMappingsForApp answers "what does this app mount", which is both
// the owner-facing read and what the builder puts on a workload.
// The two values of storage_mappings.source.
const (
	MappingSourceConsole  = "console"
	MappingSourceManifest = "manifest"
)

func (db *DB) ListStorageMappingsForApp(appID string) ([]StorageMapping, error) {
	return db.listMappings(`
		SELECT m.id, m.resource_id, m.app_id, m.path, m.access, m.folder, m.authority, m.source,
		       COALESCE(m.created_by,''), m.created_at, m.updated_at,
		       r.slug, r.name, r.kind, r.storage_root, r.partitioned, apps.slug, apps.name,
		       COALESCE(NULLIF(u.name,''), NULLIF(u.email,''), u.login, gr.name, '')
		FROM storage_mappings m
		JOIN storage_resources r ON r.id = m.resource_id
		JOIN apps ON apps.id = m.app_id
		LEFT JOIN users  u  ON m.authority = 'user:'  || u.id
		LEFT JOIN groups gr ON m.authority = 'group:' || gr.id
		WHERE m.app_id = ? ORDER BY m.path`, appID)
}

func (db *DB) listMappings(query string, args ...any) ([]StorageMapping, error) {
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StorageMapping{}
	for rows.Next() {
		var m StorageMapping
		if err := rows.Scan(&m.ID, &m.ResourceID, &m.AppID, &m.Path, &m.Access, &m.Folder, &m.Authority, &m.Source,
			&m.CreatedBy, &m.CreatedAt, &m.UpdatedAt, &m.ResourceSlug, &m.ResourceName,
			&m.ResourceKind, &m.ResourceRoot, &m.ResourcePartitioned, &m.AppSlug, &m.AppName, &m.AuthorityName); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// StorageMappingHasRoom is the app's mapping cap asked before an attach
// makes cluster objects: ErrAppMappingCap when the (resource, app, folder)
// mapping is new and the app already holds the most it may. The binding a
// mapping needs is created before PutStorageMapping writes the row — and is
// never collected when that write is refused — so without this a delegate
// calling PUT with folder f1, f2, … made one PVC (on EFS, one access point)
// per call while every row was refused (API-PROXY-3). PutStorageMapping's own
// count under the app's row lock stays the authority; this is the bound on
// what a refused attach can leave behind, which is no more than the attaches
// racing it.
func (db *DB) StorageMappingHasRoom(resourceID, appID, folder string) error {
	var exists bool
	var count int
	if err := db.conn.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM storage_mappings WHERE resource_id=? AND app_id=? AND folder=?),
		       (SELECT count(*) FROM storage_mappings WHERE app_id=?)`,
		resourceID, appID, folder, appID).Scan(&exists, &count); err != nil {
		return err
	}
	if !exists && count >= MaxAppStorageMappings {
		return ErrAppMappingCap
	}
	return nil
}

// PutStorageMapping creates or updates the mapping of one folder of one
// resource onto one app. The (resource, app, folder) triple is the identity,
// which is what makes the PUT idempotent; the path, the access level and the
// authority are what it carries.
//
// The pair used to be the identity, and that was the constraint the folder
// exists to lift: an app that reads `raw/` and writes `out/` could hold one
// mapping, so it was given the whole store at the higher of the two levels.
//
// **The stamp is the authority of this write**, and an empty one is `admin` —
// which is the fact rather than a fallback: mapping was administrator-only
// until migration 00011, and an administrator's write still adopts whatever it
// touches, so a mapping an administrator has edited never disappears with
// somebody else's delegation.
//
// A delegate's write re-reads its delegation **inside this transaction, with
// SELECT … FOR UPDATE**, and that is not belt-and-braces: without it an attach
// that read its authorization before a revocation committed could land a
// mapping stamped to a delegation that no longer exists, which no later
// revocation would ever collect. The lock is on the same row the revocation
// deletes first, so the two orders serialize — either this commits and the
// revocation collects it, or the revocation commits and this finds nothing to
// act under.
func (db *DB) PutStorageMapping(m *StorageMapping, actorID, requestID, ip, userAgent string) error {
	now := time.Now().UTC()
	if m.Authority == "" {
		m.Authority = AuthorityAdmin
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// The app row first, for a new mapping and an existing one alike, then the
	// delegation: the order the flip's ReconcileManifestStorage takes them in.
	// Taken the other way round, a delegate's attach at the instant of a flip
	// was a deadlock Postgres broke by aborting one side (AP-5). NO KEY, so the
	// lock does not stop rows elsewhere that only reference the app; held, it
	// is also what counts a new mapping against the app's cap below.
	if _, err := tx.Exec(`SELECT 1 FROM apps WHERE id = ? FOR NO KEY UPDATE`, m.AppID); err != nil {
		return err
	}

	if m.Authority != AuthorityAdmin {
		subjectType, subjectID, ok := strings.Cut(m.Authority, ":")
		if !ok {
			return fmt.Errorf("%w: %q is not an authority", ErrStorageNotManaged, m.Authority)
		}
		var ceiling string
		err := tx.QueryRow(`SELECT max_access FROM storage_managers WHERE resource_id=? AND subject_type=? AND subject_id=? FOR UPDATE`,
			m.ResourceID, subjectType, subjectID).Scan(&ceiling)
		if err == sql.ErrNoRows {
			return ErrStorageNotManaged
		}
		if err != nil {
			return err
		}
		if !StoreAccessAtLeast(ceiling, m.Access) {
			return fmt.Errorf("%w: %s at most, not %s", ErrStorageCeiling, ceiling, m.Access)
		}
	}

	var before StorageMapping
	var beforeBy sql.NullString
	existing := true
	// The row's own creation is read too, as the store grant's upsert does: a
	// re-put mapping is answered with the row it is, and without these the
	// response said 0001-01-01 and nobody.
	err = tx.QueryRow(`SELECT id, path, access, authority, created_by, created_at FROM storage_mappings WHERE resource_id=? AND app_id=? AND folder=?`,
		m.ResourceID, m.AppID, m.Folder).Scan(&before.ID, &before.Path, &before.Access, &before.Authority, &beforeBy, &before.CreatedAt)
	switch {
	case err == sql.ErrNoRows:
		existing = false
	case err != nil:
		return err
	}

	if existing {
		m.ID = before.ID
		m.CreatedBy, m.CreatedAt = beforeBy.String, before.CreatedAt
		if _, err := tx.Exec(`UPDATE storage_mappings SET path=?, access=?, authority=?, updated_at=? WHERE id=?`,
			m.Path, m.Access, m.Authority, now, m.ID); err != nil {
			return err
		}
	} else {
		// A new mapping is counted against the app's cap with the app's row
		// held (taken at the top), so two attaches at the fifth cannot both
		// see room.
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM storage_mappings WHERE app_id = ?`, m.AppID).Scan(&count); err != nil {
			return err
		}
		if count >= MaxAppStorageMappings {
			return ErrAppMappingCap
		}
		m.ID = uuid.New().String()
		m.CreatedBy = actorID
		m.CreatedAt = now
		if _, err := tx.Exec(`
			INSERT INTO storage_mappings (id, resource_id, app_id, path, access, folder, authority, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.ResourceID, m.AppID, m.Path, m.Access, m.Folder, m.Authority,
			nullIfEmpty(m.CreatedBy), now, now); err != nil {
			return err
		}
	}
	m.UpdatedAt = now

	// A folder of a partitioned resource under a host root is a gate of its
	// own, numbered the first time anything maps it — here, in the mapping's
	// transaction, by the one function every attach path goes through.
	var slug, root string
	var partitioned bool
	if err := tx.QueryRow(`SELECT slug, storage_root, partitioned FROM storage_resources WHERE id = ?`,
		m.ResourceID).Scan(&slug, &root, &partitioned); err != nil {
		return err
	}
	if root != "" && partitioned {
		if _, err := db.ensureGateTx(tx, runtime.FolderGateName(slug, m.Folder), m.ResourceID, m.Folder); err != nil {
			return err
		}
	}

	// The denormalised display fields, read back from the row that was just
	// written rather than assembled from what the caller happened to hold.
	//
	// A mapping's JSON promises `resource_slug`, `resource_kind` and the rest
	// wherever it appears, and a listing supplies them from exactly this join
	// (`listMappings`). This function did not, so the two attach routes returned
	// a mapping that was missing them — invisible only because both UI call
	// sites refetch instead of rendering the response. One shape with two
	// fillings is how a reader comes to default an absent kind, and the two
	// readers that did defaulted it in opposite directions.
	if err := tx.QueryRow(`
		SELECT r.slug, r.name, r.kind, apps.slug, apps.name,
		       COALESCE(NULLIF(u.name,''), NULLIF(u.email,''), u.login, gr.name, ''), m.source
		FROM storage_mappings m
		JOIN storage_resources r ON r.id = m.resource_id
		JOIN apps ON apps.id = m.app_id
		LEFT JOIN users  u  ON m.authority = 'user:'  || u.id
		LEFT JOIN groups gr ON m.authority = 'group:' || gr.id
		WHERE m.id = ?`, m.ID).Scan(&m.ResourceSlug, &m.ResourceName, &m.ResourceKind,
		&m.AppSlug, &m.AppName, &m.AuthorityName, &m.Source); err != nil {
		return err
	}

	action := "storage_mapping.create"
	var beforeJSON string
	if existing {
		action = "storage_mapping.update"
		beforeJSON = jsonStr(map[string]string{
			"path": before.Path, "access": before.Access, "folder": m.Folder,
			"authority": before.Authority})
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     action,
		TargetType: "app", TargetID: m.AppID,
		Before: beforeJSON,
		After: jsonStr(map[string]string{
			"resource_id": m.ResourceID, "path": m.Path, "access": m.Access,
			"folder": m.Folder, "authority": m.Authority}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteStorageMapping unmaps a resource from an app. The data is untouched;
// the app stops seeing it on its next deployment.
//
// It does not care which authority wrote the row. A delegate who may detach at
// all manages the resource and may edit the app, and could re-attach it in the
// next request — refusing them an administrator's row here would be a control
// on the smallest of the things they can already do. What the row carried is
// recorded, so the audit says whose mapping went.
// **A nil folder means every folder of this resource on this app**, and a
// non-nil one means exactly that mapping — including the empty string, which is
// the root mapping and not "unspecified". The two have to stay tellable apart:
// before folders, one DELETE detached the resource from the app, and that is
// what a caller written against the old route meant. Deleting only the root
// would leave the app holding mounts its caller believed were gone.
//
// It returns what it removed, so the ambiguity is never silent — the route says
// how many went, and the audit carries one row per mapping.
func (db *DB) DeleteStorageMapping(resourceID, appID string, folder *string, actorID, requestID, ip, userAgent string) ([]StorageMapping, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	query := `SELECT id, path, access, folder, authority, source FROM storage_mappings WHERE resource_id=? AND app_id=?`
	args := []any{resourceID, appID}
	if folder != nil {
		query += ` AND folder=?`
		args = append(args, *folder)
	}
	rows, err := tx.Query(query+` ORDER BY folder`, args...)
	if err != nil {
		return nil, err
	}
	removed := []StorageMapping{}
	for rows.Next() {
		var m StorageMapping
		if err := rows.Scan(&m.ID, &m.Path, &m.Access, &m.Folder, &m.Authority, &m.Source); err != nil {
			rows.Close()
			return nil, err
		}
		m.ResourceID, m.AppID = resourceID, appID
		removed = append(removed, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(removed) == 0 {
		return nil, sql.ErrNoRows
	}

	for _, m := range removed {
		if _, err := tx.Exec(`DELETE FROM storage_mappings WHERE id=?`, m.ID); err != nil {
			return nil, err
		}
		if err := writeAuditTx(tx, AuditEntry{
			ActorType: PrincipalTypeUser, ActorID: actorID,
			Action:     "storage_mapping.delete",
			TargetType: "app", TargetID: appID,
			Before: jsonStr(map[string]string{
				"resource_id": resourceID, "path": m.Path, "access": m.Access,
				"folder": m.Folder, "authority": m.Authority}),
			RequestID: requestID, IP: ip, UserAgent: userAgent,
		}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return removed, nil
}

// CountMappingsForFolder answers "is anything still mounting this folder of
// this resource", which is what decides whether its binding may be released.
//
// Across every level, because a binding exists per level *and* the question the
// sweep asks is about the folder: releasing the `write` binding while another
// app holds `read` on the same folder would be correct and pointless, and
// getting the level wrong would release one somebody is mounting.
func (db *DB) CountMappingsForFolder(resourceID, folder string) (int, error) {
	var n int
	err := db.conn.QueryRow(
		`SELECT COUNT(*) FROM storage_mappings WHERE resource_id=? AND folder=?`,
		resourceID, folder).Scan(&n)
	return n, err
}

// AppMounts is what a workload is given: the app's mappings, in the seam's
// vocabulary. One function, used by both the deploy path and the job
// dispatcher, so a job and its app can never see different storage.
func (db *DB) AppMounts(appID string) ([]runtime.Mount, error) {
	mappings, err := db.ListStorageMappingsForApp(appID)
	if err != nil {
		return nil, err
	}
	gates, err := db.appGates(appID)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Mount, 0, len(mappings))
	for _, m := range mappings {
		out = append(out, mountOf(m, gates))
	}
	return out, nil
}

// mountOf is one mapping as a workload mounts it, its gate looked up in gates.
func mountOf(m StorageMapping, gates map[string]runtime.Gate) runtime.Mount {
	return runtime.Mount{
		Name: m.ResourceSlug,
		// The binding, not the resource. A store's access flags are mount
		// options and mount options live on the binding object, so the level
		// is part of *which* object this mount attaches to — using the
		// resource's base name here would silently give every app the
		// first-created level's permissions.
		// A store's binding is per (resource, folder, level): a folder-scoped
		// mount is a different object with a different prefix. A volume's is
		// its one claim whatever the folder, which the pod mounts as a subPath
		// (BindingName, K8-D1).
		Claim:  runtime.BindingName(m.ResourceSlug, m.ResourceKind, m.Access, m.Folder),
		Path:   m.Path,
		Kind:   m.ResourceKind,
		Access: m.Access,
		Folder: m.Folder,
		Root:   m.ResourceRoot,
		// The group and number the host is to gate this directory with,
		// read from the row rather than from /etc/group; zero on a cluster.
		Gate:        gates[gateKey(m.ResourceID, gateFolder(m))],
		Partitioned: m.ResourcePartitioned,
	}
}

// gateFolder is which gate a mapping passes through: its folder's on a
// partitioned resource, and the resource's own (”) otherwise — where a folder
// is a subdirectory behind the one gate.
func gateFolder(m StorageMapping) string {
	if m.ResourcePartitioned {
		return m.Folder
	}
	return ""
}

func gateKey(resourceID, folder string) string { return resourceID + "\x00" + folder }

// appGates is the live gate behind every mapping of one app, by (resource,
// gate folder).
func (db *DB) appGates(appID string) (map[string]runtime.Gate, error) {
	rows, err := db.conn.Query(`
		SELECT DISTINCT g.resource_id, g.folder, g.group_name, g.gid
		  FROM storage_mappings m
		  JOIN storage_resources r ON r.id = m.resource_id
		  JOIN storage_gates g ON g.resource_id = m.resource_id AND g.retired_at IS NULL
		   AND g.folder = CASE WHEN r.partitioned THEN m.folder ELSE '' END
		 WHERE m.app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]runtime.Gate{}
	for rows.Next() {
		var id, folder string
		var g runtime.Gate
		if err := rows.Scan(&id, &folder, &g.Group, &g.GID); err != nil {
			return nil, err
		}
		out[gateKey(id, folder)] = g
	}
	return out, rows.Err()
}

// --- Validation ---

// ValidStorageKind reports whether a kind is one of the two.
func ValidStorageKind(kind string) bool {
	return kind == StorageKindVolume || kind == StorageKindStore
}

// ValidStorageBacking reports whether a backing is one this kind can have. The
// database says the same thing in a CHECK; this is here so the API can refuse
// with a sentence naming what would have worked.
func ValidStorageBacking(kind, backing string) bool {
	for _, b := range StorageBackings(kind) {
		if b == backing {
			return true
		}
	}
	return false
}

// StorageBackings lists what a kind can be backed by, in the order a form
// should offer them.
func StorageBackings(kind string) []string {
	if kind == StorageKindVolume {
		return []string{
			StorageBackingEFS, StorageBackingS3Files, StorageBackingHost,
			StorageBackingAzureFiles,
		}
	}
	return []string{StorageBackingMountpoint, StorageBackingBlob}
}

// NeedsStorageRoot reports whether a backing lives under one of the operator's
// host roots. Only `host` does by definition; `s3files` may, which is the case
// the shared-mode half exists for — an S3 Files file system is an NFS endpoint
// an ordinary box can mount — and that is the administrator's call rather than
// a property of the backing.
func NeedsStorageRoot(backing string) bool { return backing == StorageBackingHost }

// BackingNeedsBucket reports whether a backing is addressed by a bucket — an S3
// bucket, an Azure container — and therefore takes a prefix as well.
//
// **This is one predicate because it was two lists, and they diverged.** The
// create form asked for a container and a prefix on a `blob` store and the
// handler kept neither: its condition named `mountpoint` and `s3files` and was
// not extended when the backing landed. What came out was a store with no
// container to mount and, worse, no prefix — and the prefix *is* the
// containment on both drivers, so the resource that survived was the one shape
// this product refuses to create.
//
// Every **store** backing is here by definition: a store with no bucket has no
// data plane, takes no grant and cannot be mounted. A volume is here only when
// it happens to be a bucket underneath, which is `s3files` and nothing else.
func BackingNeedsBucket(backing string) bool {
	switch backing {
	case StorageBackingMountpoint, StorageBackingBlob, StorageBackingS3Files:
		return true
	}
	return false
}

// DefaultStorageBacking is what a kind is backed by when a caller names
// nothing — the backing that kind has always had, so a client written before
// this column existed keeps creating exactly what it created.
func DefaultStorageBacking(kind string) string {
	if kind == StorageKindVolume {
		return StorageBackingEFS
	}
	return StorageBackingMountpoint
}

// HasDataPlane reports whether this resource has objects behind it, and it is
// the *only* test of that anywhere — the handler's refusal, the grant write and
// the reach query all ask this question and no other.
//
// A resource has a data plane when it names a bucket — the rule migration
// 00018 replaced the kind test with — **or when it lives under a host root**
// (migration 00058). A bucket's bytes never touch the platform; a host
// directory's have no other server that could carry them, so Toft does
// (internal/hoststore). An EFS-backed file store on a cluster names neither and
// is refused exactly as it was: the platform's pod does not mount it.
//
// An s3files resource under a root keeps its bucket path, because a path that
// bypasses the platform beats one that does not; HasBucket is that choice.
func (r StorageResource) HasDataPlane() bool { return r.HasBucket() || r.OnHostRoot() }

// HasBucket reports whether this resource's bytes are reached through an
// object store. Where it is true the bucket signer is used even under a root.
func (r StorageResource) HasBucket() bool { return r.Bucket != "" }

// OnHostRoot reports whether this resource is a directory under one of the
// operator's host roots, which is what the host data plane serves.
func (r StorageResource) OnHostRoot() bool {
	return r.Backing == StorageBackingHost && r.Root != ""
}

// Storage names: what apps pass to `toft.storage(...)` and people type in
// `toft store`, so the one string a resource is called by everywhere.
//
// **The name is the identifier, and its format is the schema's.** It used to be
// a free-form label with a slug derived from it, and the slug — never shown as
// the thing to type — was what both the SDK and the CLI wanted. Lowercase
// letters, digits and single hyphens, a letter first: a string that survives a
// shell, a `toft://` path, a Kubernetes object name and a directory unchanged.
// Forty at most because every binding is `lpv-<name>-<level>-<folder digest>`
// inside a 63-character DNS label.
const (
	MinStorageNameLen = 3
	MaxStorageNameLen = 40
)

var storageNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ValidateStorageName says what is wrong with a proposed name, in a sentence an
// administrator can act on, or nil. It is asked at create only: a resource
// created before the format existed keeps the name it has, because a name is
// what its live bindings and its apps' code are spelled with.
func ValidateStorageName(name string) error {
	switch {
	case len(name) < MinStorageNameLen || len(name) > MaxStorageNameLen:
		return fmt.Errorf("a name is %d to %d characters", MinStorageNameLen, MaxStorageNameLen)
	case !storageNamePattern.MatchString(name):
		return fmt.Errorf("a name is lowercase letters, digits and single hyphens, starting with a " +
			"letter and not ending with a hyphen, like sales-data — apps and the CLI use it as typed")
	}
	return nil
}

// ValidStorageAccess reports whether an access level is valid *for that kind*,
// as a mapping or a delegate's ceiling. A grant is reached over a signed URL or
// the host data plane on either kind, and validates against the store's list.
//
// One scale, read < write < full, and one meaning per token: `write` is
// create-only on every path that offers it, `full` adds replacing and removing.
// A volume mapping offers read and full only, because a filesystem cannot
// refuse a rename to a process that can write — a create-only volume mount
// would be advisory, and nothing above the mount re-checks it.
func ValidStorageAccess(kind, access string) bool {
	for _, a := range StorageAccessLevels(kind) {
		if a == access {
			return true
		}
	}
	return false
}

// StorageAccessLevels lists what a kind offers, for an error message that says
// what would have worked. Nil for a kind that does not exist.
func StorageAccessLevels(kind string) []string {
	switch kind {
	case StorageKindVolume:
		return []string{"read", "full"}
	case StorageKindStore:
		return []string{"read", "write", "full"}
	}
	return nil
}

// StorageAccessRefusal is the sentence after "a <kind> offers <levels>" when a
// request named a level that is not one of them. `overwrite` was merged into
// `full` (migration 00074) and is refused rather than aliased: accepting it as
// `full` would grant more than was asked for.
func StorageAccessRefusal(access string) string {
	if access == "overwrite" {
		return " — overwrite was merged into full (Read & write); send full"
	}
	return ""
}

// StorageMountableAccessLevels lists the levels a *mapping* may use — the ones
// the driver underneath this backing can actually express as a mount.
//
// It is a narrowing of StorageAccessLevels and never a widening, and the two
// differ in exactly one place: a `blob` store. blobfuse2 has no `allow-delete`
// and no `allow-overwrite`; it mounts read-only or read-write, and there is no
// flag that permits creating an object while forbidding the clobbering of one.
// So `write` cannot be expressed on the mount side of an Azure store, and it is
// **refused rather than approximated**.
//
// The alternatives were to emulate it somewhere above the mount, which breaks
// the rule that makes the ladder worth having — nothing above the mount
// re-checks it — or to quietly promote `write` to `full`, which is a permission
// escalation dressed as a convenience. What is left is a refusal that names the
// driver, names the two levels that do work, and says `write` remains available
// over the URL: a SAS *can* express it, so `toft store put` is how an app that
// needs create-only-into-a-shared-dataset gets it.
func StorageMountableAccessLevels(kind, backing string) []string {
	if kind == StorageKindStore && backing == StorageBackingBlob {
		return []string{"read", "full"}
	}
	return StorageAccessLevels(kind)
}

// ValidStorageMappingAccess reports whether a level can be *mounted* on this
// backing. The API refuses at attach time with a sentence naming what would
// have worked; nothing below re-checks it, which is why the refusal is here and
// not in a handler's own list.
func ValidStorageMappingAccess(kind, backing, access string) bool {
	for _, a := range StorageMountableAccessLevels(kind, backing) {
		if a == access {
			return true
		}
	}
	return false
}

// ValidateMountPath checks a mount path before it can reach a pod spec.
//
// A relative path, a path with a traversal segment, or one landing on a
// directory the runner owns is rejected here rather than at container
// creation, where it surfaces as an unexplained CreateContainerError — or,
// worse, succeeds and shadows the release tree.
func ValidateMountPath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("mount path must be absolute, got %q", path)
	}
	if strings.HasSuffix(path, "/") && path != "/" {
		return fmt.Errorf("mount path must not end in a slash, got %q", path)
	}
	if path == "/" {
		return errors.New("mount path must not be the container root")
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("mount path must not contain %q segments, got %q", seg, path)
		}
	}
	// Exactly its clean form: `/data//x` beside `/data/x` passed the UNIQUE
	// key and the nesting check as two strings and resolved to one link, so
	// writes meant for one resource landed in the other (code review
	// 2026-11-11, B-6).
	if gopath.Clean(path) != path {
		return fmt.Errorf("mount path must not contain empty segments, got %q — write it as %q", path, gopath.Clean(path))
	}
	// The runner unpacks the release into the work directory and execs out of
	// it. Mounting over that replaces the app's own code with a filesystem,
	// which fails at exec with an error nobody traces back to a storage
	// mapping.
	for _, reserved := range []string{"/workspace", "/etc", "/usr", "/bin", "/sbin", "/lib", "/proc", "/sys", "/dev"} {
		if path == reserved || strings.HasPrefix(path, reserved+"/") {
			return fmt.Errorf("mount path %q is inside %s, which the runner owns — mount somewhere else, such as /data/%s",
				path, reserved, strings.TrimPrefix(path, "/"))
		}
	}
	return nil
}

// MountPathsNest reports whether one mount path lies strictly inside the other
// — /storage/v and /storage/v/f. One app never holds two such mounts: in shared
// mode each is a symlink under apps/<id>/storage, and the inner one would have
// to be made inside the outer one's link, which points into the resource — a
// path os.Root refuses as escaping, so every start failed (BB-048, fifth
// 2026-11-03 black-box list). In a cluster the inner mount would shadow part of
// the outer one. Equal paths are not nesting; that is storage_path_taken's
// other half, held by UNIQUE (app_id, path).
func MountPathsNest(a, b string) bool {
	if a == b {
		return false
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// PartitionConflict is the folder already mapped on a partitioned resource that
// contains, or is contained by, folder — with the apps on it — or "" when there
// is none. The trigger is what holds the rule; this is what lets a handler
// refuse with a sentence naming the folder in the way, before it tries.
func (db *DB) PartitionConflict(resourceID, folder, exceptManifestOf string) (string, []string, error) {
	rows, err := db.conn.Query(`
		SELECT m.folder, apps.name FROM storage_mappings m
		  JOIN apps ON apps.id = m.app_id
		 WHERE m.resource_id = ? AND m.folder <> ?
		   AND (starts_with(m.folder, ? || '/') OR starts_with(?, m.folder || '/'))
		   AND NOT (m.app_id = ? AND m.source = ?)
		 ORDER BY m.folder, apps.name`, resourceID, folder, folder, folder, exceptManifestOf, MappingSourceManifest)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var other string
	var apps []string
	for rows.Next() {
		var f, app string
		if err := rows.Scan(&f, &app); err != nil {
			return "", nil, err
		}
		if other == "" {
			other = f
		}
		if f == other {
			apps = append(apps, app)
		}
	}
	return other, apps, rows.Err()
}
