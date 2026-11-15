package database

import (
	"database/sql"
	"time"
)

// The gallery's half of the schema: the cached catalog document, and the
// provenance columns on apps.
//
// Two rules shape everything here. The catalog is *cached*, not owned — one
// row holding the document as it was fetched, so an item that leaves the
// catalog is answered by looking it up and not finding it rather than by a
// diffing pass nobody would get right. And "installed" is always derived from
// live app rows, never stored as a flag, so an app that is deleted stops being
// installed the moment it is gone.

// Sync statuses, in the order of how much they explain. Every one of these is
// shown to an administrator verbatim, which is why "unreachable" and "empty"
// are different words: an empty gallery and an unreachable one must never look
// alike to the person trying to work out why nothing is listed.
const (
	GallerySyncOK          = "ok"
	GallerySyncUnreachable = "unreachable"
	GallerySyncUnparseable = "unparseable"
	GallerySyncTooNew      = "too_new"
)

// GalleryCatalogCache is the cached document and everything known about the
// last attempt to refresh it.
type GalleryCatalogCache struct {
	Document     string     `json:"-"`
	ETag         string     `json:"-"`
	Status       string     `json:"status"`
	Error        string     `json:"error,omitempty"`
	ItemCount    int        `json:"item_count"`
	DroppedCount int        `json:"dropped_count"`
	FetchedAt    *time.Time `json:"fetched_at,omitempty"`
}

// GetGalleryCatalog reads the cache. A never-synced install returns a zero
// value rather than an error — that is a fresh install, not a fault.
func (db *DB) GetGalleryCatalog() (GalleryCatalogCache, error) {
	var c GalleryCatalogCache
	var fetched sql.NullTime
	err := db.conn.QueryRow(`
		SELECT document, etag, status, error, item_count, dropped_count, fetched_at
		FROM gallery_catalog WHERE id = 1
	`).Scan(&c.Document, &c.ETag, &c.Status, &c.Error, &c.ItemCount, &c.DroppedCount, &fetched)
	if err == sql.ErrNoRows {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if fetched.Valid {
		t := fetched.Time.UTC()
		c.FetchedAt = &t
	}
	return c, nil
}

// SaveGalleryCatalog stores a document that parsed.
func (db *DB) SaveGalleryCatalog(document, etag string, itemCount, droppedCount int) error {
	_, err := db.conn.Exec(`
		INSERT INTO gallery_catalog (id, document, etag, status, error, item_count, dropped_count, fetched_at)
		VALUES (1, ?, ?, ?, '', ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			document=excluded.document, etag=excluded.etag, status=excluded.status,
			error='', item_count=excluded.item_count, dropped_count=excluded.dropped_count,
			fetched_at=excluded.fetched_at`,
		document, etag, GallerySyncOK, itemCount, droppedCount, time.Now().UTC())
	return err
}

// SaveGalleryCatalogFailure records a failed refresh **without touching the
// document**. The last good catalog keeps serving with its age beside it; the
// alternative — clearing it — would turn a network blip into an empty gallery
// that looks like a catalog with nothing in it.
func (db *DB) SaveGalleryCatalogFailure(status, message string) error {
	_, err := db.conn.Exec(`
		INSERT INTO gallery_catalog (id, status, error, fetched_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			status=excluded.status, error=excluded.error, fetched_at=excluded.fetched_at`,
		status, message, time.Now().UTC())
	return err
}

// TouchGalleryCatalog records a 304: the document is unchanged and current, so
// only its age moves.
func (db *DB) TouchGalleryCatalog() error {
	_, err := db.conn.Exec(`
		UPDATE gallery_catalog SET status=?, error='', fetched_at=? WHERE id=1`,
		GallerySyncOK, time.Now().UTC())
	return err
}

// --- provenance on apps ---

// ClearAppGallery detaches an app from the catalog. Its own setter rather than
// a field on UpdateApp for the reason every other app setter here has one: the
// builder holds an App loaded before a long build, and anything UpdateApp
// writes can be silently reverted by a deploy that overlaps the change.
func (db *DB) ClearAppGallery(id string) error {
	_, err := db.conn.Exec(`
		UPDATE apps SET gallery_item_id='', gallery_kind='', gallery_version='', updated_at=?
		WHERE id=?`, time.Now().UTC(), id)
	return err
}

// SetAppGalleryVersion records the catalog version an app is *running*. Called
// by the builder when a deploy succeeds, and by nothing else: a version written
// at enqueue would survive a failed build as a claim about code that never
// started.
func (db *DB) SetAppGalleryVersion(id, version string) error {
	_, err := db.conn.Exec(`
		UPDATE apps SET gallery_version=?, updated_at=? WHERE id=? AND gallery_item_id <> ''`,
		version, time.Now().UTC(), id)
	return err
}

// ListAppsByGalleryItem returns every app deployed from one catalog item,
// install-wide and regardless of who may read them. The caller decides what to
// show: an extension's instances are counted for an administrator and linked
// only where the viewer may actually read them, and an example's are filtered
// to the viewer's own.
func (db *DB) ListAppsByGalleryItem(itemID string) ([]App, error) {
	rows, err := db.conn.Query(
		"SELECT "+appColumns+" FROM apps WHERE apps.gallery_item_id = ? ORDER BY apps.created_at",
		itemID)
	if err != nil {
		return nil, err
	}
	return scanAppRows(rows)
}

// ListGalleryApps returns every app attached to the catalog, of either kind.
// Used to answer "is anything behind" for the whole install in one query rather
// than one per item.
func (db *DB) ListGalleryApps() ([]App, error) {
	rows, err := db.conn.Query(
		"SELECT " + appColumns + " FROM apps WHERE apps.gallery_item_id <> '' ORDER BY apps.created_at")
	if err != nil {
		return nil, err
	}
	return scanAppRows(rows)
}
