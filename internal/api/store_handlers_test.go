package api

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/kube"
	"toft/internal/runtime"
)

// The ladder is expressed twice, and this is the test that keeps the two
// expressions from disagreeing.
//
// Once as mount options (kube.StoreMountOptions), once as the conditions on a
// presigned URL (presignFor). Neither is wrong on its own, which is exactly why
// drift between them would be invisible: a rung that gained `allow-overwrite`
// at the mount and not `If-None-Match` on the wire is a permission escalation
// that every existing test still passes through.
//
// So the oracle here is the *other implementation*, never a copied constant.
//
// **It is a subset relation and no longer an equality**, in one direction:
// every rung the mount grants must be a rung the URL grants. That was always
// the property protecting anything, and it stopped being expressible as an
// equality when a second store driver arrived — blobfuse2 has no
// `allow-overwrite` and no `allow-delete`, so a `blob` store mounts two of the
// three rungs while the URL still expresses all three. The direction that matters
// is unchanged: the mount may never be more generous than the URL. The other
// direction is checked per backing by TestBlobMountNeverExceedsItsRefusal.
func TestPresignLadderMatchesTheMount(t *testing.T) {
	for _, level := range []string{
		runtime.AccessRead, runtime.AccessWrite, runtime.AccessFull,
	} {
		t.Run(level, func(t *testing.T) {
			opts := kube.StoreMountOptions(runtime.StorageResource{
				Slug: "reports", Kind: runtime.MountStore, Backing: runtime.BackingMountpoint,
				Bucket: "acme-data", Prefix: "team/reports", Access: level,
			}, "")
			mountAllows := func(flag string) bool {
				for _, o := range opts {
					if o == flag {
						return true
					}
				}
				return false
			}

			// GET: every rung reads. The mount adds no flag for it — read is
			// what a store is mounted as before anything is added — so the
			// assertion is that the URL is minted at all.
			if _, ok := presignFor(level, verbGet); !ok {
				t.Errorf("%s cannot read, but every rung of the ladder can", level)
			}

			// PUT: the mount permits a write at every rung above read, and the
			// URL must be mintable at exactly the same rungs.
			cond, canPut := presignFor(level, verbPut)
			wantPut := level != runtime.AccessRead
			if canPut != wantPut {
				t.Errorf("presignFor(%s, put) = %v, but the mount %s writable",
					level, canPut, map[bool]string{true: "is", false: "is not"}[wantPut])
			}

			// The `write` rung is the one that has to be conditional, and the
			// oracle for "is this the write rung" is the mount's own flags:
			// writable, and no allow-overwrite.
			wantConditional := wantPut && !mountAllows("allow-overwrite")
			if cond.IfNoneMatch != wantConditional {
				t.Errorf("presignFor(%s, put).IfNoneMatch = %v, but the mount %s allow-overwrite —\n"+
					"the two expressions of this rung disagree about whether it may clobber",
					level, cond.IfNoneMatch, map[bool]string{true: "sets", false: "omits"}[mountAllows("allow-overwrite")])
			}

			// DELETE: `full` and only `full`, and again the mount says which.
			_, canDelete := presignFor(level, verbDelete)
			if canDelete != mountAllows("allow-delete") {
				t.Errorf("presignFor(%s, delete) = %v, but the mount %s allow-delete",
					level, canDelete, map[bool]string{true: "sets", false: "omits"}[mountAllows("allow-delete")])
			}
		})
	}
}

// A level nobody granted is not a level. An empty access — a resolution that
// found nothing — must not satisfy the bottom rung, or a caller with no grant
// would read.
func TestPresignRefusesALevelThatIsNotOnTheLadder(t *testing.T) {
	// `overwrite` was a rung until migration 00074 merged it into `full`; a URL
	// minted for it now would be a level the database can no longer hold.
	for _, notALevel := range []string{"", "root", "admin", "READ", "write ", "overwrite"} {
		for _, verb := range []presignVerb{verbGet, verbPut, verbDelete} {
			if _, ok := presignFor(notALevel, verb); ok {
				t.Errorf("presignFor(%q, %s) allowed it; only the three rungs are rungs", notALevel, verb)
			}
		}
	}
	// And an unknown verb refuses rather than falling through to permissive.
	if _, ok := presignFor(runtime.AccessFull, presignVerb("copy")); ok {
		t.Error("an unknown verb was allowed at the top rung")
	}
}

// The client supplies a name and never a key. This is the one place in the
// feature where a bug is a containment breach rather than an error message: a
// name that escapes the prefix would let a caller granted one store address
// another one, with a URL the platform signed itself.
func TestStoreObjectNamesCannotEscapeThePrefix(t *testing.T) {
	refused := []struct{ name, why string }{
		{"", "an empty name"},
		{"/absolute.parquet", "a leading slash"},
		{"../escape.parquet", "a parent segment"},
		{"reports/../../escape.parquet", "a parent segment in the middle"},
		{"reports/./x.parquet", "a current-directory segment"},
		{"reports//x.parquet", "an empty segment"},
		{"reports/", "a trailing slash"},
		{"reports\\x.parquet", "a backslash separator"},
		{"reports/\x00x.parquet", "a NUL"},
		{"reports/\nx.parquet", "a newline"},
		{"..", "the parent itself"},
		{strings.Repeat("a", maxStoreObjectName+1), "a name past the length cap"},
	}
	for _, c := range refused {
		if err := ValidateStoreObjectName(c.name); err == nil {
			t.Errorf("%s (%q) was accepted", c.why, c.name)
		}
	}

	accepted := []string{
		"nightly.parquet",
		"2026/08/17.parquet",
		"reports/q3 summary.xlsx",
		"a.b.c/..hidden-but-not-a-segment/x",
		strings.Repeat("a", maxStoreObjectName),
	}
	for _, name := range accepted {
		if err := ValidateStoreObjectName(name); err != nil {
			t.Errorf("%q should be a fine name: %v", name, err)
		}
	}
}

// Every accepted name lands inside the store's prefix. The property, rather
// than the list above: whatever the validator lets through, the key it produces
// is under the prefix and the prefix ends where the mount says it ends.
func TestEveryAcceptedNameLandsUnderThePrefix(t *testing.T) {
	res := database.StorageResource{Slug: "reports", Kind: database.StorageKindStore,
		Bucket: "acme-data", Prefix: "team/reports"}

	for _, name := range []string{
		"nightly.parquet", "2026/08/17.parquet", "deep/deeper/deepest.csv", "..hidden",
	} {
		if err := ValidateStoreObjectName(name); err != nil {
			t.Fatalf("fixture %q is not accepted: %v", name, err)
		}
		key := storeKeyFor(res, name)
		if !strings.HasPrefix(key, "team/reports/") {
			t.Errorf("%q produced key %q, which is outside the store", name, key)
		}
	}

	// The trailing slash is forced, for the same reason the mount forces it:
	// "team/reports" without it also matches "team/reports-archive", and the
	// two expressions of where a store ends have to agree.
	if got := storeKeyFor(res, "x"); got != "team/reports/x" {
		t.Errorf("storeKeyFor = %q, want the prefix closed with a slash", got)
	}
	withSlash := res
	withSlash.Prefix = "team/reports/"
	if got := storeKeyFor(withSlash, "x"); got != "team/reports/x" {
		t.Errorf("a prefix that already ends in a slash produced %q", got)
	}
	leading := res
	leading.Prefix = "/team/reports"
	if got := storeKeyFor(leading, "x"); got != "team/reports/x" {
		t.Errorf("a prefix with a leading slash produced %q; an S3 key has no leading slash", got)
	}
}

// A path is a name with one relaxation, and the relaxation is the risk.
//
// `path` is the only input in this feature a caller supplies that is *not* an
// object name, and the difference between them is a single trailing slash. So
// everything the name validator refuses this must refuse too — and this test is
// written against the same list, deliberately, because a second implementation
// of it is the way a traversal gets permitted in one place and not the other.
func TestStorePathsCannotEscapeThePrefix(t *testing.T) {
	refused := []struct{ path, why string }{
		{"/absolute/", "a leading slash"},
		{"../", "a parent segment"},
		{"..", "the parent itself, unterminated"},
		{"reports/../../escape/", "a parent segment in the middle"},
		{"reports/./", "a current-directory segment"},
		{"reports//nested/", "an empty segment"},
		{"reports\\nested/", "a backslash separator"},
		{"reports/\x00nested/", "a NUL"},
		{"reports/\nnested/", "a newline"},
		{"/", "the root spelled as a slash"},
		{"//", "two slashes"},
		{strings.Repeat("a", maxStoreObjectName+1) + "/", "a path past the length cap"},
	}
	for _, c := range refused {
		if err := ValidateStorePath(c.path); err == nil {
			t.Errorf("%s (%q) was accepted as a path", c.why, c.path)
		}
	}

	accepted := []string{
		"",          // the store root
		"reports",   // a folder named without its slash
		"reports/",  // and with it
		"2026/08/",  // nested
		"a b/c.d/",  // spaces and dots are ordinary characters in a key
		"..hidden/", // leading dots that are not a parent segment
	}
	for _, path := range accepted {
		if err := ValidateStorePath(path); err != nil {
			t.Errorf("%q should be a fine path: %v", path, err)
		}
	}
}

// The property behind the list: whatever the path validator lets through, the
// prefix the handler lists with is inside the store — never the bucket root,
// never a sibling store, never the parent of either.
//
// This is the assertion that `ListObjectsV2` cannot be reached with a key
// outside the store, stated over the two functions that build the one it gets.
func TestEveryAcceptedPathListsInsideTheStore(t *testing.T) {
	res := database.StorageResource{Slug: "reports", Kind: database.StorageKindStore,
		Bucket: "acme-data", Prefix: "team/reports"}

	for _, path := range []string{"", "reports", "reports/", "2026/08/", "..hidden/"} {
		if err := ValidateStorePath(path); err != nil {
			t.Fatalf("fixture %q is not accepted: %v", path, err)
		}
		key := storeKeyFor(res, storePathPrefix(path))
		if !strings.HasPrefix(key, "team/reports/") {
			t.Errorf("path %q produced listing prefix %q, which is outside the store", path, key)
		}
	}

	// The store root lists the store and nothing beside it. "team/reports"
	// without the slash would also match "team/reports-archive", which is a
	// different store on the same bucket.
	if got := storeKeyFor(res, storePathPrefix("")); got != "team/reports/" {
		t.Errorf("the root listed with prefix %q, want it closed with a slash", got)
	}

	// A folder named without its slash is listed with one, so "team/reports/q3"
	// cannot pick up "team/reports/q3-draft.csv" beside it.
	if got := storeKeyFor(res, storePathPrefix("q3")); got != "team/reports/q3/" {
		t.Errorf("folder q3 listed with prefix %q", got)
	}
}

// A digest arrives as hex or as base64, because "sha256" means hex to
// sha256sum and base64 to S3. Anything that is neither, or is the right
// encoding of the wrong number of bytes, is a 400 before anything is signed.
func TestDigestsAreAcceptedInEitherEncoding(t *testing.T) {
	// 32 bytes of 0xAB, as hex and as base64.
	hexSHA := strings.Repeat("ab", 32)
	b64SHA := "q6urq6urq6urq6urq6urq6urq6urq6urq6urq6urq6s="

	fromHex, err := normalizeDigest(hexSHA, 32)
	if err != nil {
		t.Fatalf("hex sha256: %v", err)
	}
	fromB64, err := normalizeDigest(b64SHA, 32)
	if err != nil {
		t.Fatalf("base64 sha256: %v", err)
	}
	if fromHex != fromB64 {
		t.Errorf("the same digest in two encodings produced %q and %q", fromHex, fromB64)
	}
	if fromHex != b64SHA {
		t.Errorf("normalized to %q, want the base64 form S3 wants (%q)", fromHex, b64SHA)
	}

	for _, bad := range []string{"", "   ", "not-a-digest", strings.Repeat("ab", 16), "q6urq6s="} {
		if _, err := normalizeDigest(bad, 32); err == nil {
			t.Errorf("%q was accepted as a 32-byte digest", bad)
		}
	}
}

// Absent, not dormant. An install with stores and no signer offers no upload
// routes at all — the operator has not granted the platform any permission on
// their buckets, and a route that existed and failed would say the opposite.
func TestStoreUploadsAreAbsentWithoutASigner(t *testing.T) {
	s := &Server{}
	if s.storeUploadsOffered() {
		t.Error("a server with neither storage nor a signer offers uploads")
	}
	s.storage = stubProvisioner{}
	if s.storeUploadsOffered() {
		t.Error("storage without a signer offers uploads; the operator granted no bucket permission")
	}
}

type stubProvisioner struct{ runtime.StorageProvisioner }

// The same four fixtures as `web/src/lib/store-hash.test.ts`, hashed by Go.
//
// The browser computes both digests and the platform signs them into a URL it
// never sends bytes through, so nothing downstream can catch a disagreement:
// the first sign of one is an upload the object store refuses as BadDigest,
// after the URL is minted and the body is on the wire. Two implementations of
// one number, tied here by a shared table.
func TestBrowserDigestVectorsAreTheOnesGoComputes(t *testing.T) {
	pattern := func(size int) []byte {
		out := make([]byte, size)
		for i := range out {
			out[i] = byte((i*7 + 3) % 251)
		}
		return out
	}

	for _, c := range []struct {
		name, sha256Hex, md5Hex string
		bytes                   []byte
	}{
		{"the empty file", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"d41d8cd98f00b204e9800998ecf8427e", nil},
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			"900150983cd24fb0d6963f7d28e17f72", []byte("abc")},
		{"toft", "6a6f9028c099184750c56aa6f1655f97aeae171bd6a7d384b56b95c28b5a6179",
			"b4f445a2e2c16f4b8ab6496bb54ef265", []byte("toft")},
		{"20 MiB of pattern", "fc8299d1434a2b792cd13a13d546a73d9351033fe7bcff5a6f342ff865cbabc9",
			"eb9c339d686a68906f8496c456e45169", pattern(20 * 1024 * 1024)},
	} {
		sha := sha256.Sum256(c.bytes)
		if got := hex.EncodeToString(sha[:]); got != c.sha256Hex {
			t.Errorf("%s: sha256 = %s, and the browser sends %s", c.name, got, c.sha256Hex)
		}
		sum := md5.Sum(c.bytes)
		if got := hex.EncodeToString(sum[:]); got != c.md5Hex {
			t.Errorf("%s: md5 = %s, and the browser sends %s", c.name, got, c.md5Hex)
		}

		// And the hex the browser sends is a form the upload route accepts —
		// the digests are useless if they arrive in an encoding it refuses.
		if _, err := normalizeDigest(c.sha256Hex, 32); err != nil {
			t.Errorf("%s: the route refuses its own sha256 vector: %v", c.name, err)
		}
		if _, err := normalizeDigest(c.md5Hex, 16); err != nil {
			t.Errorf("%s: the route refuses its own md5 vector: %v", c.name, err)
		}
	}
}

// The audit row names the folder whose grant authorized the mint. The key it
// is asked about carries the store prefix the grant folders never have, and
// the match is on a separator, the same match LevelForKey used — a bare string
// prefix on the prefixed key recorded "" for a delete a folder grant allowed,
// which told the auditor the caller held the whole store.
func TestGrantingFolderIsTheOneThatAuthorized(t *testing.T) {
	scopes := &database.StoreScopes{
		Resource: database.StorageResource{Slug: "analytics", Prefix: "team-a"},
		Scopes: []database.StoreScope{
			{Folder: "reports", Access: runtime.AccessFull},
			{Folder: "reports-archive", Access: runtime.AccessFull},
			{Folder: "", Access: runtime.AccessRead},
		},
	}
	// The name is the caller's path under the store — what the handler
	// resolves the level with — not the bucket key.
	if got := grantingFolder(scopes, "reports/q1.csv", runtime.AccessFull); got != "reports" {
		t.Errorf("folder for reports/q1.csv at full = %q, want reports", got)
	}
	if got := grantingFolder(scopes, "reports-archive/q1.csv", runtime.AccessFull); got != "reports-archive" {
		t.Errorf("folder for reports-archive/q1.csv at full = %q, want reports-archive — a separator match, not a string prefix", got)
	}
	if got := grantingFolder(scopes, "reportsx/q1.csv", runtime.AccessFull); got != "" {
		t.Errorf("reportsx/ is not under reports/, got folder %q", got)
	}
	if got := grantingFolder(scopes, "reports/q1.csv", runtime.AccessRead); got != "" {
		t.Errorf("a read under the root grant = %q, want the root, spelled as the empty folder", got)
	}
}

// A person's store refusal carries insufficient_access and a hint, as the app
// surface's does — outside the grant, or below the level (BB-015).
func TestAStoreRefusalIsInsufficientAccessWithAHint(t *testing.T) {
	scopes := &database.StoreScopes{}
	for name, body := range map[string]map[string]any{
		"outside": refusalFor(scopes, "", runtime.AccessRead),
		"below":   refusalFor(scopes, runtime.AccessRead, runtime.AccessWrite),
	} {
		hint, _ := body["hint"].(string)
		if body["code"] != "insufficient_access" || hint == "" {
			t.Errorf("%s: %v", name, body)
		}
	}
	if hint, _ := refusalFor(scopes, runtime.AccessRead, runtime.AccessWrite)["hint"].(string); !strings.Contains(hint, `"write"`) {
		t.Errorf("below hint %q does not name the level needed", hint)
	}
}
