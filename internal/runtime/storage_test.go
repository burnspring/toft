package runtime

import "testing"

// A binding with no folder is named exactly as it was before folders existed.
//
// **This is the migration's safety, expressed as a test.** Every binding object
// in every install today was created under these names, and a name that changed
// would not fail anywhere visible: the platform would create a second access
// point beside a full one, or bind a store under a name nothing is claiming,
// and the app would come up with an empty directory. Hence a digest that is
// *absent* rather than the digest of the empty string.
func TestBindingNameIsUnchangedWithoutAFolder(t *testing.T) {
	for _, tc := range []struct{ slug, kind, access, want string }{
		{"reports", MountStore, AccessRead, "lpv-reports-read"},
		{"reports", MountStore, AccessFull, "lpv-reports-full"},
		{"reports", MountStore, "", "lpv-reports-read"},
		{"cache", MountVolume, AccessFull, "lpv-cache"},
	} {
		if got := BindingName(tc.slug, tc.kind, tc.access, ""); got != tc.want {
			t.Errorf("BindingName(%q, %q, %q, \"\") = %q, want %q",
				tc.slug, tc.kind, tc.access, got, tc.want)
		}
	}
}

// A folder-scoped binding is a different object, and two folders are two.
func TestBindingNameSeparatesFolders(t *testing.T) {
	root := BindingName("reports", MountStore, AccessFull, "")
	a := BindingName("reports", MountStore, AccessFull, "analytics")
	b := BindingName("reports", MountStore, AccessFull, "finance")

	if a == root || b == root || a == b {
		t.Errorf("names collide: root=%q a=%q b=%q", root, a, b)
	}
	if got := BindingName("reports", MountStore, AccessFull, "analytics"); got != a {
		t.Errorf("the same folder must produce the same name: %q then %q", a, got)
	}
	// A DNS label, still: 63 characters, lower-case alphanumerics and dashes.
	for _, name := range []string{a, b} {
		if len(name) > 63 {
			t.Errorf("%q is %d characters, and a binding name is a DNS label", name, len(name))
		}
		for _, r := range name {
			if !(r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
				t.Errorf("%q contains %q, which a DNS label cannot carry", name, r)
			}
		}
	}
}

// Bindings fans over the folders it is given, and always includes the
// resource's own — which is the one a delete has to release.
func TestBindingsCoverTheResourceAndItsFolders(t *testing.T) {
	res := StorageResource{Slug: "reports", Kind: MountStore, Bucket: "b", Prefix: "team/reports"}

	root := Bindings(res)
	if len(root) != 4 {
		t.Fatalf("a store's own bindings = %d, want 4 (one per rung, and the retired overwrite)", len(root))
	}
	for _, b := range root {
		if b.Folder != "" {
			t.Errorf("Bindings(res) produced folder %q, want the resource itself", b.Folder)
		}
	}

	withFolders := Bindings(res, "analytics", "analytics", "finance")
	if len(withFolders) != 12 {
		t.Errorf("bindings for two distinct folders plus the root = %d, want 12 — "+
			"a folder named twice is one folder", len(withFolders))
	}

	// One folder-scoped binding, which is what a sweep releases.
	one := Bindings(StorageResource{Slug: "reports", Kind: MountStore, Folder: "analytics"})
	if len(one) != 4 {
		t.Fatalf("releasing one folder = %d bindings, want 4", len(one))
	}
	for _, b := range one {
		if b.Folder != "analytics" {
			t.Errorf("releasing one folder touched %q", b.Folder)
		}
	}
}

func TestParseStorageClasses(t *testing.T) {
	got, err := ParseStorageClasses(" oms-share , ord-share=s3files,legacy=azurefiles ")
	if err != nil {
		t.Fatal(err)
	}
	want := []DeclaredClass{{Name: "oms-share"}, {Name: "ord-share", Backing: BackingS3Files}, {Name: "legacy", Backing: BackingAzureFiles}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"=efs", "a=gp3", "a,a"} {
		if _, err := ParseStorageClasses(bad); err == nil {
			t.Errorf("%q parsed, want an error", bad)
		}
	}
}

// A volume naming no class is created on its backing's default, or on the one
// class serving the backing; several with no default is a choice the create
// asks for rather than guesses.
func TestResolveDefaultClass(t *testing.T) {
	classes := []DeclaredClass{
		{Name: "efs-toft", Backing: BackingEFS, Default: true},
		{Name: "ord-share", Backing: BackingS3Files},
		{Name: "a", Backing: BackingAzureFiles},
		{Name: "b", Backing: BackingAzureFiles},
	}
	for _, tc := range []struct {
		backing, want string
		ok            bool
	}{
		{BackingEFS, "efs-toft", true},
		{BackingS3Files, "ord-share", true},
		{BackingAzureFiles, "", false},
		{BackingHost, "", true},
	} {
		got, ok := ResolveDefaultClass(classes, tc.backing)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s = %q %v, want %q %v", tc.backing, got, ok, tc.want, tc.ok)
		}
	}
}

// A store binding's name carries its level, so a mapping at `overwrite` before
// migration 00074 left a `-overwrite` binding that nothing claims once the
// mapping is `full`. A resource's delete is what releases it, so Bindings must
// keep naming it for a store — and never for a volume, whose binding name has
// never carried a level.
func TestBindingsStillReleaseTheRetiredOverwriteLevel(t *testing.T) {
	store := Bindings(StorageResource{Slug: "reports", Kind: MountStore, Bucket: "b", Prefix: "p"})
	found := false
	for _, b := range store {
		found = found || b.Access == legacyAccessOverwrite
	}
	if !found {
		t.Errorf("a store's bindings %+v leave out %q; a promoted mapping's old binding would never be released",
			store, legacyAccessOverwrite)
	}
	for _, b := range Bindings(StorageResource{Slug: "share", Kind: MountVolume}) {
		if b.Access == legacyAccessOverwrite {
			t.Errorf("a volume binding at %q: a volume's binding name carries no level", b.Access)
		}
	}
}

// K8-D1: a volume's folder is a directory of its one claim, so the folder
// names no binding of its own and a volume has one binding whatever folders
// are asked about.
func TestAVolumeFolderBindsTheVolumesOneClaim(t *testing.T) {
	if got := BindingName("shared", MountVolume, AccessRead, "inbox"); got != "lpv-shared" {
		t.Errorf("BindingName for a volume folder = %q, want lpv-shared", got)
	}
	got := Bindings(StorageResource{Slug: "shared", Kind: MountVolume, Folder: "inbox"}, "a", "b")
	if len(got) != 1 || got[0].Folder != "" ||
		BindingName(got[0].Slug, got[0].Kind, got[0].Access, got[0].Folder) != "lpv-shared" {
		t.Errorf("Bindings of a volume = %+v, want only its root claim", got)
	}
}

// A volume's claim is lpv-<slug> and a store's lpv-<slug>-<level>[-<8 hex>],
// in one namespace, so a volume named for a store's level is that store's
// binding. The pair is what clashes; a level-suffixed name alone does not.
func TestBindingNamesClash(t *testing.T) {
	for _, tc := range []struct {
		volume, store string
		want          bool
	}{
		{"reports-full", "reports", true},
		{"reports-read", "reports", true},
		{"reports-write", "reports", true},
		{"reports-overwrite", "reports", true},
		{"reports-full-0a1b2c3d", "reports", true},
		{"reports-fuller", "reports", false},
		{"reports-full-0A1B2C3D", "reports", false},
		{"reports-full-0a1b2c3", "reports", false},
		{"reports", "reports", false},
		{"reports-full", "report", false},
		{"uploads-full", "reports", false},
	} {
		if got := BindingNamesClash(tc.volume, tc.store); got != tc.want {
			t.Errorf("BindingNamesClash(%q, %q) = %v, want %v", tc.volume, tc.store, got, tc.want)
		}
		if tc.want && BindingName(tc.volume, MountVolume, "", "") != "lpv-"+tc.volume {
			t.Errorf("volume %q's claim is not lpv-%s", tc.volume, tc.volume)
		}
	}
	if BindingName("reports-full", MountVolume, "", "") != BindingName("reports", MountStore, AccessFull, "") {
		t.Error("the clash this guards is no longer one name")
	}
}
