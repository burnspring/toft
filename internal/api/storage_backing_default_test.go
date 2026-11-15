package api

import (
	"testing"

	"toft/internal/database"
)

// A volume created without a backing on a shared install, which offers only
// host, is a host volume — not a refusal about EFS (porting feedback 2026-11-03).
func TestABackingNamedByNobodyIsTheOneOnOffer(t *testing.T) {
	cases := []struct {
		kind    string
		offered []string
		want    string
	}{
		{database.StorageKindVolume, []string{database.StorageBackingHost}, database.StorageBackingHost},
		{database.StorageKindVolume, []string{database.StorageBackingEFS, database.StorageBackingS3Files}, database.StorageBackingEFS},
		// BB-051, fifth 2026-11-03 black-box list: host and S3 Files on offer is
		// host, not efs; two neither of which is the default is no default.
		{database.StorageKindVolume, []string{database.StorageBackingHost, database.StorageBackingS3Files}, database.StorageBackingHost},
		{database.StorageKindVolume, []string{database.StorageBackingS3Files, "other"}, ""},
		{database.StorageKindVolume, nil, database.StorageBackingEFS},
		{database.StorageKindStore, []string{database.StorageBackingMountpoint}, database.StorageBackingMountpoint},
	}
	for _, c := range cases {
		if got := defaultBackingOffered(c.kind, c.offered); got != c.want {
			t.Errorf("%s offering %v = %q, want %q", c.kind, c.offered, got, c.want)
		}
	}
}
