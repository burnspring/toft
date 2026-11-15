package api

import (
	"path"
	"strings"
	"testing"
	"unicode"

	"toft/internal/database"
	"toft/internal/kube"
	"toft/internal/runtime"
)

// The prefix is the containment, and on one of the two drivers it is one `..`
// away from not being.
//
// Mountpoint matches a prefix as a literal string, so `team/../` is a prefix no
// key has and the mount is simply empty. blobfuse2 composes every request as
// `filepath.Join(subdirectory, name)`, and Join *cleans* — so the same string
// mounts the container root. Against a real blobfuse2 that is exactly what it
// does: `--subdirectory=team/../` reads a blob beside `team/` rather than
// inside it.
//
// `kube.StorePrefix` cannot refuse it: it returns a string, and the refusal
// belongs where a resource is written down. So what has to hold is that the two
// halves agree about which strings are safe — everything the validator accepts
// comes out of StorePrefix unable to move above the resource. A validator that
// stopped refusing a traversal fails here rather than in a mount on a cluster.
func TestStorePrefixCannotEscapeWhatTheValidatorAccepts(t *testing.T) {
	cases := []struct{ prefix, folder string }{
		{"team", ""},
		{"team/reports", "q1"},
		{"team", "sub/deeper"},
		{"team/../", ""},
		{"..", ""},
		{"../..", ""},
		{"team", ".."},
		{"team", "../../etc"},
		{"team/./x", ""},
		{"", ""},
	}

	for _, c := range cases {
		mount := kube.StorePrefix(runtime.StorageResource{
			Slug: "reports", Kind: runtime.MountStore, Backing: runtime.BackingBlob,
			Bucket: "acme-data", Prefix: c.prefix, Folder: c.folder, Access: runtime.AccessRead,
		})
		// What blobfuse2 makes of it, and therefore where the mount is rooted.
		cleaned := path.Clean("/" + mount)
		escapes := cleaned == "/" || strings.HasPrefix(cleaned, "/..")
		refused := ValidateStorePath(c.prefix) != nil || ValidateStorePath(c.folder) != nil ||
			strings.Trim(c.prefix, "/") == ""

		if escapes && !refused {
			t.Errorf("prefix %q folder %q mounts %q, which cleans to %q — at or above the "+
				"container root, and nothing refused it", c.prefix, c.folder, mount, cleaned)
		}
	}
}

// The blob driver splits mount options on spaces into blobfuse2's argv, so
// whitespace in a blob prefix or folder is a second flag, not a path — a second
// `--subdirectory=` moves the mount out of the store's prefix. Everything the
// validators accept for a blob store renders one --subdirectory= element with
// no whitespace in it (security review 2026-11-14, K8-2).
func TestStorePrefixOnBlobCarriesNoWhitespace(t *testing.T) {
	cases := []struct{ prefix, folder string }{
		{"team", ""},
		{"team", "q1"},
		{"team", "x --subdirectory=other"},
		{"te am", ""},
		{"team", "a\tb"},
		{"team --subdirectory=/", "q1"},
	}
	for _, c := range cases {
		accepted := ValidateStorePath(c.prefix) == nil && !hasSpace(c.prefix) &&
			ValidateStorePath(c.folder) == nil && !hasSpace(c.folder)
		if !accepted {
			continue
		}
		opts := kube.StoreMountOptions(runtime.StorageResource{
			Slug: "reports", Kind: runtime.MountStore, Backing: runtime.BackingBlob,
			Bucket: "acme-data", Prefix: c.prefix, Folder: c.folder, Access: runtime.AccessRead,
		}, "")
		for _, o := range opts {
			if strings.HasPrefix(o, "--subdirectory=") && strings.ContainsFunc(o, unicode.IsSpace) {
				t.Errorf("prefix %q folder %q was accepted and renders %q, which blobfuse2 "+
					"receives as more than one argument", c.prefix, c.folder, o)
			}
		}
	}
	for _, refused := range []string{"x --subdirectory=other", "te am"} {
		if !hasSpace(refused) {
			t.Errorf("%q is not refused on a blob store", refused)
		}
	}
}

// Every store backing is addressed by a bucket, and the server has to keep it.
//
// This is the test the `blob` backing shipped without. The create form asked for
// a container and a prefix; the handler's condition named `mountpoint` and
// `s3files`, so a blob store was created with neither — no container to mount,
// and no prefix, which is the containment. `database.BackingNeedsBucket` is now
// the one list, and a store backing that is not in it is a store with no data
// plane: no Files page, no grants, nothing to mount.
func TestEveryStoreBackingNeedsItsBucket(t *testing.T) {
	for _, backing := range database.StorageBackings(database.StorageKindStore) {
		if !database.BackingNeedsBucket(backing) {
			t.Errorf("store backing %q needs no bucket — a store with no bucket has no data "+
				"plane and no prefix to be contained by", backing)
		}
	}
	if !database.BackingNeedsBucket(database.StorageBackingS3Files) {
		t.Error("an S3 Files volume is a bucket mounted as a filesystem and needs one named")
	}
	for _, backing := range []string{
		database.StorageBackingEFS, database.StorageBackingHost, database.StorageBackingAzureFiles,
	} {
		if database.BackingNeedsBucket(backing) {
			t.Errorf("backing %q names no bucket; asking for one invents a data plane it "+
				"does not have", backing)
		}
	}
}
