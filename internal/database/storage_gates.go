package database

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"toft/internal/runtime"
)

// Gates: the host system groups behind host-root storage, and their numbers.
//
// **The database is the source of a gate's gid, and the host's /etc/group is a
// cache of it.** Autocreated groups live in a container's /etc/group and die
// with it; the file system they gate does not, and several tasks may mount one
// EFS. A group recreated by whichever groupadd ran first comes back at a
// different number, and a directory still carrying the old one then belongs to
// whatever gate was created into it. So a gid is allocated here once, and every
// start (runtime.SharedStorage.Attach) recreates the group at exactly it.

// The default gate range: above every SYS_GID_MAX and UID_MIN a stock
// distribution ships, below nobody's 65534, and a thousand gates wide.
const (
	DefaultGateGIDLow  = 62000
	DefaultGateGIDHigh = 62999
)

// ErrGateRangeExhausted means every gid in the install's range is held,
// retired gates included. Distinct so the API can answer 422 naming the setting.
var ErrGateRangeExhausted = errors.New("no gid left in SHARED_STORAGE_GID_RANGE for another storage gate")

// ErrGateNameTaken means the host group a folder's gate would be named is
// already another folder's: the 48-bit name collides. Refused rather than
// shared, because sharing it would hand that folder's gid to this one.
var ErrGateNameTaken = errors.New("this folder's gate name is already used by another folder — choose a different folder name")

// WithGateGIDRange sets the range gate gids are allocated from. The bounds are
// already validated; see runtime.ParseGIDRange.
func WithGateGIDRange(low, high int) Option {
	return func(db *DB) { db.gateLow, db.gateHigh = low, high }
}

func (db *DB) gateRange() (int, int) {
	if db.gateLow == 0 {
		return DefaultGateGIDLow, DefaultGateGIDHigh
	}
	return db.gateLow, db.gateHigh
}

// ensureGateTx gives a gate its number, inside the transaction that first needs
// it, and returns the gid.
//
// A gate that already exists by name keeps its number and is re-pointed at this
// resource only when it is this resource's own row or a retired gate of the
// same folder: a resource created onto a slug a deleted one held is created
// onto the same directory, whose files carry that number. A name held by any
// other row — another resource's live gate, a different folder whose 48-bit
// name collides — is refused with ErrGateNameTaken rather than handed over, or
// a chosen folder name could take another resource's gid. Otherwise the lowest
// free gid in the range is taken under a table lock, so two creates cannot both
// see the same number free.
func (db *DB) ensureGateTx(tx *Tx, groupName, resourceID, folder string) (int, error) {
	var gid int
	err := tx.QueryRow(`
		UPDATE storage_gates SET resource_id = ?, folder = ?, retired_at = NULL
		 WHERE group_name = ? AND folder = ? AND (retired_at IS NOT NULL OR resource_id = ?)
		 RETURNING gid`, resourceID, folder, groupName, folder, resourceID).Scan(&gid)
	if err == nil {
		return gid, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var taken int
	err = tx.QueryRow(`SELECT 1 FROM storage_gates WHERE group_name = ?`, groupName).Scan(&taken)
	if err == nil {
		return 0, ErrGateNameTaken
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	if _, err := tx.Exec(`LOCK TABLE storage_gates IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return 0, err
	}
	low, high := db.gateRange()
	err = tx.QueryRow(`
		SELECT n FROM generate_series(?::int, ?::int) AS n
		 WHERE NOT EXISTS (SELECT 1 FROM storage_gates g WHERE g.gid = n)
		 ORDER BY n LIMIT 1`, low, high).Scan(&gid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w (%d-%d)", ErrGateRangeExhausted, low, high)
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`
		INSERT INTO storage_gates (group_name, resource_id, folder, gid) VALUES (?, ?, ?, ?)`,
		groupName, resourceID, folder, gid); err != nil {
		return 0, err
	}
	return gid, nil
}

// retireGatesTx marks a deleted resource's gates as held by nobody. The rows
// stay, and with them the numbers: a host-root delete removes the directory
// now, but a number that is never reused needs no condition on whether files
// carrying it could still exist anywhere — on a cluster's retained volume, or
// on a backup of the root.
func retireGatesTx(tx *Tx, resourceID string) error {
	_, err := tx.Exec(`
		UPDATE storage_gates SET retired_at = now()
		 WHERE resource_id = ? AND retired_at IS NULL`, resourceID)
	return err
}

// resourceGateName is the whole resource's gate. One spelling, the runtime's,
// because the database records the name the host will be asked for.
func resourceGateName(slug string) string { return runtime.StorageGroupName(slug) }

// ErrNoGateForKey means a write on a host-root resource lands outside every
// gate — on a partitioned resource, beside its mapped folders rather than in
// one. Nothing there is any app's, so the host data plane refuses to put a
// file where no app could open it.
var ErrNoGateForKey = errors.New("this is outside every folder an app is mapped to")

// GateForKey is the gid a file or folder the host data plane writes takes: the
// resource's own gate on a whole resource, the gate of the mapped folder that
// holds it on a partitioned one. name is relative to the resource directory,
// and a trailing slash is a folder, which covers itself.
//
// The mapping selects the gate, not the gate row alone: a folder's gate
// outlives the mapping that numbered it (a re-split, a deploy that failed after
// EnsureFolderGate), and such a stale row still covers every key beneath its
// folder. Mapped folders of a partitioned resource never nest, so with only
// mapped folders' gates considered at most one row covers a key.
//
// Recorded rather than read off the directory, so a write is the gate's even
// before any app has started and gated the directory it lands in — and so it
// is the number the files must carry, whatever the host's /etc/group says.
func (db *DB) GateForKey(resourceID, name string) (int, error) {
	rows, err := db.conn.Query(`
		SELECT g.folder, g.gid FROM storage_gates g
		 WHERE g.resource_id = ? AND g.retired_at IS NULL
		   AND (g.folder = '' OR EXISTS (SELECT 1 FROM storage_mappings m
		         WHERE m.resource_id = g.resource_id AND m.folder = g.folder))`, resourceID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	folderPath := strings.HasSuffix(name, "/")
	name = strings.TrimSuffix(name, "/")
	for rows.Next() {
		var folder string
		var gid int
		if err := rows.Scan(&folder, &gid); err != nil {
			return 0, err
		}
		if folder == "" || coversKey(folder, name) || (folderPath && name == folder) {
			return gid, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return 0, ErrNoGateForKey
}

// MappedFolders is every folder of a resource some app is mapped to, sorted and
// without duplicates — the only places a partitioned resource takes a write
// from outside, on any backing. Read from the mappings rather than the gates:
// a gate outlives the mapping that made it, and a folder no app is mapped to
// any longer is no app's, which is the reason the write is refused.
func (db *DB) MappedFolders(resourceID string) ([]string, error) {
	rows, err := db.conn.Query(`
		SELECT DISTINCT folder FROM storage_mappings
		 WHERE resource_id = ? AND folder <> '' ORDER BY folder`, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var folder string
		if err := rows.Scan(&folder); err != nil {
			return nil, err
		}
		out = append(out, folder)
	}
	return out, rows.Err()
}

// InsideMappedFolder reports whether a name a write would create lies in one of
// folders: a key under it, or — with a trailing slash — the folder's own marker
// or one beneath it. Matched on a separator, like every folder here.
func InsideMappedFolder(folders []string, name string) bool {
	for _, f := range folders {
		if strings.HasPrefix(name, f+"/") {
			return true
		}
	}
	return false
}
