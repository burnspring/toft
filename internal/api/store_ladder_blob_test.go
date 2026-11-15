package api

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/kube"
	"toft/internal/runtime"
)

// The subset rule, stated per backing: **every rung a mount grants must be a
// rung the URL grants.**
//
// This is what the equality test used to be, generalised the only way that
// keeps it meaningful once a second store driver exists. blobfuse2 mounts
// read-only or read-write and has nothing in between, so a `blob` store cannot
// express `write` as a mount — and the danger is not that it
// offers fewer rungs, it is that a rung it cannot express gets *rounded up*.
//
// So for every backing and every level: whatever the mount permits, the URL
// must permit at least as much, and a level the backing refuses to mount must
// never produce a writable mount if it somehow gets past the refusal.
func TestMountNeverExceedsTheURL(t *testing.T) {
	backings := []string{runtime.BackingMountpoint, runtime.BackingBlob}
	levels := []string{
		runtime.AccessRead, runtime.AccessWrite, runtime.AccessFull,
	}

	for _, backing := range backings {
		for _, level := range levels {
			t.Run(backing+"/"+level, func(t *testing.T) {
				opts := kube.StoreMountOptions(runtime.StorageResource{
					Slug: "reports", Kind: runtime.MountStore, Backing: backing,
					Bucket: "acme-data", Prefix: "team/reports", Access: level,
				}, "")
				has := func(flag string) bool {
					for _, o := range opts {
						if o == flag {
							return true
						}
					}
					return false
				}

				// What the mount permits, in the ladder's own vocabulary.
				mountWrites := backing == runtime.BackingBlob && !has("-o ro")
				mountOverwrites := has("allow-overwrite") || mountWrites
				mountDeletes := has("allow-delete") || mountWrites

				// What the URL permits at the same level.
				_, urlWrites := presignFor(level, verbPut)
				urlCond, _ := presignFor(level, verbPut)
				urlOverwrites := urlWrites && !urlCond.IfNoneMatch
				_, urlDeletes := presignFor(level, verbDelete)

				if mountOverwrites && !urlOverwrites {
					t.Errorf("a %s mount at %s may overwrite, and a signed URL at the same level "+
						"may not — the mount is more generous than the URL, which is the one "+
						"direction this rule forbids.\nmount options: %v", backing, level, opts)
				}
				if mountDeletes && !urlDeletes {
					t.Errorf("a %s mount at %s may delete, and a signed URL at the same level "+
						"may not.\nmount options: %v", backing, level, opts)
				}
			})
		}
	}
}

// A level the backing cannot mount must never land on a writable mount.
//
// The refusal in validateAttach is what stops `write` reaching a
// `blob` binding at all, and `overwrite` no longer exists (migration 00074) —
// but a row the migration missed is exactly the case this guards. This is the second line: if one ever did —
// a new caller, a migration, a row written before the refusal existed —
// blobMountOptions must round it *down* to read-only, not up to read-write.
// Rounding up is a silent grant of `full` on somebody's dataset.
func TestBlobMountNeverExceedsItsRefusal(t *testing.T) {
	for _, level := range []string{runtime.AccessWrite, "overwrite"} {
		if database.ValidStorageMappingAccess(
			database.StorageKindStore, database.StorageBackingBlob, level) {
			t.Fatalf("%s is offered as a mountable level on a blob store; blobfuse2 cannot "+
				"express it, so offering it is a promise the mount cannot keep", level)
		}
		opts := kube.StoreMountOptions(runtime.StorageResource{
			Slug: "reports", Kind: runtime.MountStore, Backing: runtime.BackingBlob,
			Bucket: "acme-data", Prefix: "team/reports", Access: level,
		}, "")
		if !containsOption(opts, "-o ro") {
			t.Errorf("a blob mount at %s is read-write. A level the driver cannot express must "+
				"round DOWN — rounding up is a silent grant of full.\noptions: %v", level, opts)
		}
	}

	// And the two it *can* express are still what they say.
	read := kube.StoreMountOptions(runtime.StorageResource{
		Slug: "reports", Kind: runtime.MountStore, Backing: runtime.BackingBlob,
		Bucket: "acme-data", Prefix: "team/reports", Access: runtime.AccessRead,
	}, "")
	if !containsOption(read, "-o ro") {
		t.Errorf("a blob mount at read is not read-only: %v", read)
	}
	full := kube.StoreMountOptions(runtime.StorageResource{
		Slug: "reports", Kind: runtime.MountStore, Backing: runtime.BackingBlob,
		Bucket: "acme-data", Prefix: "team/reports", Access: runtime.AccessFull,
	}, "")
	if containsOption(full, "-o ro") {
		t.Errorf("a blob mount at full is read-only: %v", full)
	}
}

// The containment is the subdirectory, and it is the same string Mountpoint
// gets as its prefix. One definition of what a binding can reach, on either
// driver — a folder-scoped mapping that narrowed on one and not the other would
// be a folder grant reaching a sibling.
func TestBlobMountCarriesTheSamePrefixAsMountpoint(t *testing.T) {
	res := runtime.StorageResource{
		Slug: "reports", Kind: runtime.MountStore,
		Bucket: "acme-data", Prefix: "team/reports", Folder: "q1", Access: runtime.AccessRead,
	}
	want := kube.StorePrefix(res)
	if want == "" {
		t.Fatal("StorePrefix returned nothing; the containment boundary is empty")
	}

	res.Backing = runtime.BackingBlob
	var got string
	for _, o := range kube.StoreMountOptions(res, "") {
		if strings.HasPrefix(o, "--subdirectory=") {
			got = strings.TrimPrefix(o, "--subdirectory=")
		}
	}
	if got != want {
		t.Errorf("blobfuse2 subdirectory = %q, Mountpoint prefix = %q — the two drivers "+
			"disagree about what this binding can reach", got, want)
	}
}

// The three rungs are unchanged on the URL side. presignFor is the ladder and
// the backing does not narrow it: a `blob` store still signs create-only and
// delete URLs, which is what makes the attach-time refusal a real answer rather
// than a consolation.
func TestPresignKeepsAllThreeRungsOnEveryBacking(t *testing.T) {
	if _, ok := presignFor(runtime.AccessWrite, verbPut); !ok {
		t.Error("the write rung stopped signing a PUT")
	}
	if cond, _ := presignFor(runtime.AccessWrite, verbPut); !cond.IfNoneMatch {
		t.Error("the write rung stopped being create-only")
	}
	if cond, ok := presignFor(runtime.AccessFull, verbPut); !ok || cond.IfNoneMatch {
		t.Error("the full rung stopped signing an unconditional PUT")
	}
	if _, ok := presignFor(runtime.AccessFull, verbDelete); !ok {
		t.Error("the full rung stopped signing a DELETE")
	}
}

func containsOption(opts []string, want string) bool {
	for _, o := range opts {
		if o == want {
			return true
		}
	}
	return false
}
