package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The CHECK, in both directions.
//
// A constraint is the one place a mistake in this work is not observable from
// the product at all: a widening that admitted a backing no code path
// provisions produces a row an administrator created and nothing can mount, and
// a narrowing that admitted less than the code offers produces a create form
// whose submit fails with a database error.
func TestAzureBackingsAreAcceptedAndOthersAreNot(t *testing.T) {
	db := dbtest.New(t)
	conn := db.Conn()

	insert := func(slug, kind, backing, bucket, prefix string) error {
		_, err := conn.Exec(
			`INSERT INTO storage_resources (id, slug, name, kind, backing, claim_name, bucket, prefix)
			 VALUES (gen_random_uuid()::text, $1, $1, $2, $3, $1, $4, $5)`,
			slug, kind, backing, bucket, prefix)
		return err
	}

	// Everything StorageBackings offers must be insertable, or the create form
	// offers something the database refuses.
	for _, backing := range database.StorageBackings(database.StorageKindVolume) {
		if backing == database.StorageBackingHost {
			// `host` carries a root, which the root check demands separately —
			// tested where that constraint lives.
			continue
		}
		if err := insert("vol-"+backing, database.StorageKindVolume, backing, "", ""); err != nil {
			t.Errorf("a volume backed by %q was refused by the CHECK, but the create form "+
				"offers it: %v", backing, err)
		}
	}
	for _, backing := range database.StorageBackings(database.StorageKindStore) {
		if err := insert("store-"+backing, database.StorageKindStore, backing,
			"acme-data", "reports"); err != nil {
			t.Errorf("a store backed by %q was refused by the CHECK, but the create form "+
				"offers it: %v", backing, err)
		}
	}

	// And the cross products that are not backings of that kind.
	for _, tc := range []struct{ kind, backing string }{
		{database.StorageKindVolume, database.StorageBackingBlob},
		{database.StorageKindVolume, database.StorageBackingMountpoint},
		{database.StorageKindStore, database.StorageBackingAzureFiles},
		{database.StorageKindStore, database.StorageBackingEFS},
		{database.StorageKindStore, "azureblob"},
	} {
		if err := insert("bad-"+tc.kind+"-"+tc.backing, tc.kind, tc.backing, "b", "p"); err == nil {
			t.Errorf("a %s backed by %q was accepted; the kind and the backing have to agree",
				tc.kind, tc.backing)
		}
	}
}
