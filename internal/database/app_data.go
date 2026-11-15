package database

import (
	"database/sql"
	"errors"
	"time"
)

// The app's one JSON document — see migration 42.
//
// Everything in this file treats the document as opaque bytes. Shape and size
// are checked at the edge (`api.handleAppDataPut`), meaning is never read here
// or anywhere else, and nothing in the platform ever writes a field into it.
// The moment something does, this stops being the app's own store and becomes a
// place where the platform and the app can disagree about the same key.

// A cleared document leaves its row behind as a tombstone: document '' and the
// version the clear moved it to. Never a DELETE of the row, because the version
// is the compare-and-set token and must never come round again — a counter that
// restarted at 1 after a clear would let a writer holding version 1 from before
// the clear overwrite a different document that happens to be version 1 now
// (BB-005, the ABA case). '' can never be a stored document: validateAppDocument
// refuses an empty body, so the tombstone is unambiguous. Every reader treats a
// tombstone as absent, which is still version 0 as far as the app is concerned.
const appDataTombstone = ""

// ErrAppDataConflict reports a failed precondition: the document changed under
// a writer that named the version it meant to replace.
//
// Its own error rather than a `false` return, because the caller has to tell it
// apart from "nothing was there" — one is a 412 an app retries after re-reading,
// the other is an ordinary success. A boolean would collapse them, and the
// collapse would look like the write working.
var ErrAppDataConflict = errors.New("app data version conflict")

// AppData is the document and the two facts about it that are the platform's:
// which write it came from, and when.
type AppData struct {
	AppID string `json:"-"`
	// Document is what the app PUT, byte for byte. Not `json.RawMessage`,
	// because that would tempt a caller into unmarshalling it, and no caller in
	// this product has any business doing so.
	Document  string    `json:"-"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Bytes is the size of the stored document, which is the figure every cap and
// every read-back is expressed in.
func (d *AppData) Bytes() int { return len(d.Document) }

// GetAppData returns this app's document, or nil when it has never written one.
//
// nil rather than an empty AppData: "never written" and "written and then
// emptied to {}" are different states, an app legitimately branches on the
// difference at startup, and a zero value would make them indistinguishable.
func (db *DB) GetAppData(appID string) (*AppData, error) {
	d := &AppData{AppID: appID}
	err := db.conn.QueryRow(`
		SELECT document, version, updated_at FROM app_data
		 WHERE app_id = ? AND document <> ''`, appID).
		Scan(&d.Document, &d.Version, &d.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// PutAppData replaces the document and returns what is now stored.
//
// ifVersion is the caller's precondition: nil means last-write-wins, a value
// means "only if the stored version is still this", and 0 means "only if there
// is nothing there" — absent is version 0, so create-only needs no second route.
// A precondition that does not hold returns ErrAppDataConflict.
//
// One statement whichever precondition applies, and never a read followed by a
// write: two writers can both read version 3 and both then write version 4, and
// the loser never learns it lost. Each statement below makes the comparison and
// the write the same atomic act, and each returns no row — writing nothing —
// when the precondition does not hold.
func (db *DB) PutAppData(appID, document string, ifVersion *int64) (*AppData, error) {
	now := time.Now().UTC()
	out := &AppData{AppID: appID, Document: document}

	var err error
	switch {
	case ifVersion == nil:
		// Last write wins, which is the single-writer app's case and the
		// default for that reason.
		err = db.conn.QueryRow(`
			INSERT INTO app_data (app_id, document, version, updated_at)
			VALUES (?, ?, 1, ?)
			ON CONFLICT (app_id) DO UPDATE
			   SET document = excluded.document,
			       version = app_data.version + 1,
			       updated_at = excluded.updated_at
			RETURNING version, updated_at`,
			appID, document, now).Scan(&out.Version, &out.UpdatedAt)

	case *ifVersion == 0:
		// "Only if nothing is stored." A guarded update that only a tombstone
		// passes: a live document must not be touched, and the absent RETURNING
		// row is what reports that one was there. A tombstone is nothing stored,
		// and writing over it continues its version rather than restarting at 1.
		err = db.conn.QueryRow(`
			INSERT INTO app_data (app_id, document, version, updated_at)
			VALUES (?, ?, 1, ?)
			ON CONFLICT (app_id) DO UPDATE
			   SET document = excluded.document,
			       version = app_data.version + 1,
			       updated_at = excluded.updated_at
			 WHERE app_data.document = ''
			RETURNING version, updated_at`,
			appID, document, now).Scan(&out.Version, &out.UpdatedAt)

	default:
		// "Only if the stored version is still this." An UPDATE and not an
		// upsert, deliberately: a caller naming version 3 against a row that is
		// no longer there is wrong in the same way as one naming it against
		// version 4, and an INSERT here would quietly grant the first and refuse
		// the second.
		err = db.conn.QueryRow(`
			UPDATE app_data
			   SET document = ?, version = version + 1, updated_at = ?
			 WHERE app_id = ? AND version = ? AND document <> ''
			RETURNING version, updated_at`,
			document, now, appID, *ifVersion).Scan(&out.Version, &out.UpdatedAt)
	}
	if err == sql.ErrNoRows {
		return nil, ErrAppDataConflict
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteAppData removes the document, returning whether one was there. The row
// stays as a tombstone carrying the next version (appDataTombstone).
//
// ifVersion carries the same meaning as in PutAppData, and absent is still
// version 0 — so `DELETE` with a precondition of 0 against nothing is a success
// that removed nothing, which is what idempotent deletion means.
func (db *DB) DeleteAppData(appID string, ifVersion *int64) (bool, error) {
	if ifVersion == nil {
		res, err := db.conn.Exec(`
			UPDATE app_data SET document = ?, version = version + 1, updated_at = ?
			 WHERE app_id = ? AND document <> ''`, appDataTombstone, time.Now().UTC(), appID)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		return n > 0, err
	}

	// With a precondition the two failure shapes have to stay apart, and a
	// bare DELETE reports both as "no rows": gone already, versus changed under
	// you. FOR UPDATE holds the row for the length of the decision.
	tx, err := db.conn.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var current int64
	var document string
	err = tx.QueryRow(`SELECT version, document FROM app_data WHERE app_id = ? FOR UPDATE`, appID).
		Scan(&current, &document)
	if err == sql.ErrNoRows || (err == nil && document == appDataTombstone) {
		current = 0
	} else if err != nil {
		return false, err
	}
	if current != *ifVersion {
		return false, ErrAppDataConflict
	}
	if current == 0 {
		return false, tx.Commit()
	}
	if _, err := tx.Exec(`
		UPDATE app_data SET document = ?, version = version + 1, updated_at = ?
		 WHERE app_id = ?`, appDataTombstone, time.Now().UTC(), appID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
