package database

import (
	"testing"

	"toft/internal/runtime"
)

// K8-D1: a volume's folder mapping names the volume's one claim; a store's
// folder mapping still names its own folder-scoped binding.
func TestMountOfAVolumeFolderNamesTheVolumesClaim(t *testing.T) {
	vol := mountOf(StorageMapping{ResourceSlug: "shared", ResourceKind: runtime.MountVolume,
		Access: runtime.AccessRead, Folder: "inbox", Path: "/data/inbox"}, nil)
	if vol.Claim != "lpv-shared" || vol.Folder != "inbox" {
		t.Errorf("volume folder mount = claim %q folder %q, want lpv-shared and inbox", vol.Claim, vol.Folder)
	}
	store := mountOf(StorageMapping{ResourceSlug: "reports", ResourceKind: runtime.MountStore,
		Access: runtime.AccessRead, Folder: "inbox", Path: "/data/inbox"}, nil)
	if store.Claim == "lpv-reports-read" {
		t.Errorf("store folder mount = %q, want a folder-scoped binding", store.Claim)
	}
}
