package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeobj "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"toft/internal/runner"
	"toft/internal/runtime"
)

// Durable storage, at the tier that catches object-shaping mistakes: what the
// backend *sends* to the API. Whether the mount actually appears in a container
// is a cluster question and lives in kube_cluster_test.go.

// --- Retain, and the reason it is not negotiable ---

// A store's PersistentVolume must never carry a Delete reclaim policy.
//
// Under Delete, releasing the binding asks the driver to remove the objects
// behind it — data an administrator provisioned deliberately and Toft has
// no business destroying. This is the single assertion that stands between a
// resource delete and someone's dataset.
func TestStoreVolumeRetainsData(t *testing.T) {
	b, _ := testBackend(t)
	b.cfg.StoreRegion = "eu-west-1"

	res := runtime.StorageResource{
		Slug: "reports", Kind: runtime.MountStore,
		Bucket: "acme-data", Prefix: "sales/reports", Access: runtime.AccessRead,
	}
	if err := b.EnsureStorage(context.Background(), res); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	pv, err := b.client.CoreV1().PersistentVolumes().
		Get(context.Background(), "toft-apps-lpv-reports-read", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the volume: %v", err)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Fatalf("reclaim policy = %q, want Retain — a Delete policy destroys the data",
			pv.Spec.PersistentVolumeReclaimPolicy)
	}
}

// The prefix is the containment. Mountpoint matches it as a literal string, so
// "team" without a trailing slash also matches "team-archive" — a containment
// hole that reads as a typo.
func TestStorePrefixIsTerminated(t *testing.T) {
	opts := StoreMountOptions(runtime.StorageResource{
		Slug: "team", Kind: runtime.MountStore, Bucket: "b", Prefix: "team", Access: runtime.AccessRead,
	}, "")

	if !hasOption(opts, "prefix team/") {
		t.Errorf("mount options = %v, want a prefix ending in a slash", opts)
	}
}

// A store with no prefix would mount a bucket root, giving every app mapped to
// any store in that bucket access to all of it. Refused, not defaulted.
func TestStoreWithoutAPrefixIsRefused(t *testing.T) {
	b, _ := testBackend(t)

	err := b.EnsureStorage(context.Background(), runtime.StorageResource{
		Slug: "everything", Kind: runtime.MountStore, Bucket: "acme-data", Access: runtime.AccessRead,
	})
	if err == nil {
		t.Fatal("expected a refusal for a store with no prefix")
	}
	if !strings.Contains(err.Error(), "isolation boundary") {
		t.Errorf("the refusal should say why, got: %v", err)
	}
}

// The blob driver splits mount options on spaces into blobfuse2's argv, so a
// folder with a space in it is a second flag — a second --subdirectory= among
// them. Refused before any PV is written (security review 2026-11-14, K8-2).
func TestEnsureStorageRefusesWhitespaceInABlobSubdirectory(t *testing.T) {
	b, client := testBackend(t)

	err := b.EnsureStorage(context.Background(), runtime.StorageResource{
		Slug: "team", Kind: runtime.MountStore, Backing: runtime.BackingBlob,
		Bucket: "c", Prefix: "team", Folder: "a b", Access: runtime.AccessRead,
	})
	if err == nil {
		t.Fatal("a blob folder with a space in it was bound")
	}
	pvs, _ := client.CoreV1().PersistentVolumes().List(context.Background(), metav1.ListOptions{})
	if len(pvs.Items) != 0 {
		t.Errorf("a PV was written for the refused binding: %v", pvs.Items[0].Spec.MountOptions)
	}
}

// --- The access ladder ---

// Each level is the previous plus one flag, and nothing above the mount
// re-checks them — which is exactly what makes them worth having.
func TestAccessLadderIsCumulative(t *testing.T) {
	res := runtime.StorageResource{
		Slug: "data", Kind: runtime.MountStore, Bucket: "b", Prefix: "p",
	}

	cases := []struct {
		access             string
		overwrite, deletes bool
	}{
		{runtime.AccessRead, false, false},
		{runtime.AccessWrite, false, false},
		{runtime.AccessFull, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.access, func(t *testing.T) {
			res.Access = tc.access
			opts := StoreMountOptions(res, "")

			if got := hasOption(opts, "allow-overwrite"); got != tc.overwrite {
				t.Errorf("allow-overwrite = %v, want %v (%v)", got, tc.overwrite, opts)
			}
			if got := hasOption(opts, "allow-delete"); got != tc.deletes {
				t.Errorf("allow-delete = %v, want %v (%v)", got, tc.deletes, opts)
			}
			// The pod runs as runnerUID and cannot chown a mount, so without
			// these the files are unreadable by app code.
			if !hasOption(opts, "uid=10001") || !hasOption(opts, "gid=10001") {
				t.Errorf("mount must present files as the runner UID, got %v", opts)
			}
		})
	}
}

// A store's flags live on the volume, not on the pod, so two apps at different
// levels cannot share one binding. One binding per level in use is the
// consequence, and the names have to reflect it or the second app would
// silently get the first's permissions.
func TestStoreBindingsAreNamedPerAccessLevel(t *testing.T) {
	read := runtime.BindingName("data", runtime.MountStore, runtime.AccessRead, "")
	full := runtime.BindingName("data", runtime.MountStore, runtime.AccessFull, "")
	if read == full {
		t.Fatalf("two access levels share the binding %q — one app's flags would apply to another's", read)
	}

	// A volume needs none of this: read-only is expressed on the pod's own
	// mount, so one binding serves every level.
	if runtime.BindingName("cache", runtime.MountVolume, runtime.AccessRead, "") !=
		runtime.BindingName("cache", runtime.MountVolume, runtime.AccessFull, "") {
		t.Error("a volume should have one binding for every level")
	}

	// And the prefix keeps claims clear of workload names, which share the
	// namespace with them.
	if !strings.HasPrefix(read, "lpv-") {
		t.Errorf("binding %q should be prefixed to avoid colliding with a workload name", read)
	}
}

// --- Volumes ---

// A volume is provisioned dynamically from the operator's class, and must be
// ReadWriteMany: an app's service pods and its job pods mount the same claim,
// which is what makes sharing between them need no additional concept.
func TestVolumeClaimIsShareable(t *testing.T) {
	b, _ := testBackend(t)
	b.cfg.StorageClass = "efs-shared"

	res := runtime.StorageResource{Slug: "cache", Kind: runtime.MountVolume}
	if err := b.EnsureStorage(context.Background(), res); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	pvc, err := b.client.CoreV1().PersistentVolumeClaims("toft-apps").
		Get(context.Background(), "lpv-cache", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the claim: %v", err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "efs-shared" {
		t.Errorf("storage class = %v, want the configured one", pvc.Spec.StorageClassName)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("access modes = %v, want ReadWriteMany", pvc.Spec.AccessModes)
	}
}

// A claim name still held by one being deleted is not a provisioned claim.
//
// Creating over it answers AlreadyExists, which was swallowed as success — so
// the platform believed a resource was backed, the old claim finalized and took
// the name with it, and nothing was left anywhere. Nothing asks again once the
// answer was success, so no later deploy repaired it. The window is an hour on
// this install: a finished job pod holds its claim through pvc-protection for
// as long as the pod is kept, which a second run of the drills lands inside.
func TestAClaimStillBeingDeletedIsNotProvisioned(t *testing.T) {
	b, _ := testBackend(t)
	b.cfg.StorageClass = "efs-shared"
	res := runtime.StorageResource{Slug: "cache", Kind: runtime.MountVolume}
	ctx := context.Background()

	if err := b.EnsureStorage(ctx, res); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	// What a delete leaves behind while a pod still references it.
	pvc, err := b.client.CoreV1().PersistentVolumeClaims("toft-apps").
		Get(ctx, "lpv-cache", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the claim: %v", err)
	}
	now := metav1.Now()
	pvc.DeletionTimestamp = &now
	if _, err := b.client.CoreV1().PersistentVolumeClaims("toft-apps").
		Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("marking the claim terminating: %v", err)
	}

	err = b.EnsureStorage(ctx, res)
	if err == nil {
		t.Fatal("provisioning over a terminating claim reported success, so the resource " +
			"would be recorded as backed with no volume behind it")
	}
	if !strings.Contains(err.Error(), "being deleted") {
		t.Errorf("the refusal does not say what is in the way: %v", err)
	}
}

// A binary newer than the RBAC it runs under keeps working.
//
// The guard above was shipped reading the object back while both Role manifests
// granted `create` and `delete` and no `get`, so every re-provision failed with
// a forbidden — including the ordinary live-claim one — and the resource row
// survived with no binding: the same unbacked state by a different road. A chart
// is upgraded on an operator's schedule, so a binary that requires a permission
// it did not use yesterday breaks on upgrade. An unanswerable question goes back
// to the old behaviour rather than taking the install with it.
func TestAForbiddenReadBackSkipsTheCheckRatherThanFailing(t *testing.T) {
	b, client := testBackend(t)
	b.cfg.StorageClass = "efs-shared"
	res := runtime.StorageResource{Slug: "cache", Kind: runtime.MountVolume}
	ctx := context.Background()

	if err := b.EnsureStorage(ctx, res); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	// What an install whose Role has not been upgraded answers.
	client.PrependReactor("get", "persistentvolumeclaims",
		func(k8stesting.Action) (bool, runtimeobj.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Resource: "persistentvolumeclaims"}, "lpv-cache",
				errors.New(`User "system:serviceaccount:toft:toft" cannot get resource`))
		})

	if err := b.EnsureStorage(ctx, res); err != nil {
		t.Errorf("a re-provision failed because the platform could not read the claim back: %v", err)
	}
}

// A live claim of the same name is still success — provisioning twice must not
// fail, which is what the AlreadyExists swallow was written for.
func TestProvisioningTwiceIsStillIdempotent(t *testing.T) {
	b, _ := testBackend(t)
	b.cfg.StorageClass = "efs-shared"
	res := runtime.StorageResource{Slug: "cache", Kind: runtime.MountVolume}

	if err := b.EnsureStorage(context.Background(), res); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := b.EnsureStorage(context.Background(), res); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
}

// Without a class there is nothing to provision from, and the error must name
// the setting rather than surfacing as a Pending claim nobody can explain.
func TestVolumeWithoutAStorageClassNamesTheSetting(t *testing.T) {
	b, _ := testBackend(t)

	err := b.EnsureStorage(context.Background(), runtime.StorageResource{
		Slug: "cache", Kind: runtime.MountVolume,
	})
	if err == nil {
		t.Fatal("expected a refusal with no storage class configured")
	}
	if !strings.Contains(err.Error(), "KUBE_STORAGE_CLASS") {
		t.Errorf("the error should name the setting, got: %v", err)
	}
}

// StorageKinds is what the admin surface offers. A form that offered volumes on
// an install with no shared filesystem would fail on submit for a reason the
// admin cannot see from the page.
func TestOnlyProvisionableKindsAreOffered(t *testing.T) {
	b, _ := testBackend(t)

	kinds := b.StorageKinds()
	if contains(kinds, runtime.MountVolume) {
		t.Error("volumes were offered with no storage class configured")
	}
	// The store driver is defaulted, so stores are always on offer.
	if !contains(kinds, runtime.MountStore) {
		t.Errorf("stores should be offered by default, got %v", kinds)
	}

	b.cfg.StorageClass = "efs-shared"
	if !contains(b.StorageKinds(), runtime.MountVolume) {
		t.Error("volumes should be offered once a class is configured")
	}
}

// A backing an operator configured no class for is absent, not broken. The
// create form asks per kind, so this answers per kind.
func TestOnlyProvisionableBackingsAreOffered(t *testing.T) {
	b, _ := testBackend(t)

	if got := b.StorageBackings(runtime.MountVolume); len(got) != 0 {
		t.Errorf("backings = %v with no class configured, want none", got)
	}
	// The store driver is defaulted, so Mountpoint is always on offer.
	if got := b.StorageBackings(runtime.MountStore); !contains(got, runtime.BackingMountpoint) {
		t.Errorf("store backings = %v, want mountpoint", got)
	}

	b.cfg.StorageClass = "efs-shared"
	got := b.StorageBackings(runtime.MountVolume)
	if !contains(got, runtime.BackingEFS) {
		t.Errorf("backings = %v, want efs once its class is configured", got)
	}
	if contains(got, runtime.BackingS3Files) {
		t.Errorf("backings = %v, want no s3files until its own class is configured", got)
	}

	b.cfg.S3FilesStorageClass = "s3files-shared"
	if got := b.StorageBackings(runtime.MountVolume); !contains(got, runtime.BackingS3Files) {
		t.Errorf("backings = %v, want s3files once its class is configured", got)
	}
}

// EFS and Azure Files share KUBE_STORAGE_CLASS, and no cluster has both, so the
// class's provisioner decides which one the form offers. A class that is
// missing or unknown offers both: hiding the backing that works is worse than
// showing one too many.
func TestTheClassProvisionerPicksEFSOrAzureFiles(t *testing.T) {
	for _, tc := range []struct {
		name, provisioner string
		want              []string
	}{
		{"efs", "efs.csi.aws.com", []string{runtime.BackingEFS}},
		{"azure csi", "file.csi.azure.com", []string{runtime.BackingAzureFiles}},
		{"azure in-tree", "kubernetes.io/azure-file", []string{runtime.BackingAzureFiles}},
		{"unknown", "rancher.io/local-path", []string{runtime.BackingEFS, runtime.BackingAzureFiles}},
		{"missing", "", []string{runtime.BackingEFS, runtime.BackingAzureFiles}},
	} {
		b, client := testBackend(t)
		b.cfg.StorageClass = "apps"
		if tc.provisioner != "" {
			if _, err := client.StorageV1().StorageClasses().Create(context.Background(), &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "apps"}, Provisioner: tc.provisioner,
			}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		if got := b.StorageBackings(runtime.MountVolume); strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: backings = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// One install, two filesystems. The class a resource is bound from is the
// backing's, and a resource that names its own class wins over both — which is
// the whole of what putting the class on the resource bought.
func TestTheBackingChoosesTheClassAndTheResourceOverridesIt(t *testing.T) {
	b, _ := testBackend(t)
	b.cfg.StorageClass = "efs-shared"
	b.cfg.S3FilesStorageClass = "s3files-shared"

	for _, c := range []struct {
		name     string
		res      runtime.StorageResource
		wantName string
		want     string
	}{
		{"efs", runtime.StorageResource{Slug: "cache", Kind: runtime.MountVolume,
			Backing: runtime.BackingEFS}, "lpv-cache", "efs-shared"},
		{"s3files", runtime.StorageResource{Slug: "reports", Kind: runtime.MountVolume,
			Backing: runtime.BackingS3Files}, "lpv-reports", "s3files-shared"},
		{"named", runtime.StorageResource{Slug: "archive", Kind: runtime.MountVolume,
			Backing: runtime.BackingS3Files, StorageClass: "s3files-archive"},
			"lpv-archive", "s3files-archive"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := b.EnsureStorage(context.Background(), c.res); err != nil {
				t.Fatalf("ensure: %v", err)
			}
			pvc, err := b.client.CoreV1().PersistentVolumeClaims("toft-apps").
				Get(context.Background(), c.wantName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("reading the claim: %v", err)
			}
			if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != c.want {
				t.Errorf("storage class = %v, want %q", pvc.Spec.StorageClassName, c.want)
			}
		})
	}
}

// The prerequisites differ per backing, so the refusal has to as well: an
// administrator told to set KUBE_STORAGE_CLASS when what is missing is the S3
// Files class has been sent to the wrong setting.
func TestTheRefusalNamesTheBackingsOwnSetting(t *testing.T) {
	b, _ := testBackend(t)
	b.cfg.StorageClass = "efs-shared"

	err := b.EnsureStorage(context.Background(), runtime.StorageResource{
		Slug: "reports", Kind: runtime.MountVolume, Backing: runtime.BackingS3Files,
	})
	if err == nil {
		t.Fatal("expected a refusal with no S3 Files class configured")
	}
	if !strings.Contains(err.Error(), "KUBE_S3FILES_STORAGE_CLASS") {
		t.Errorf("the error should name the S3 Files setting, got: %v", err)
	}
}

// --- Attaching to a pod ---

// storagePairs sets aside the always-present scratch emptyDir (which is not
// part of the storage machinery this file is about) and returns the storage
// volumes and their mount points.
// storagePairs is the opt-in storage only. Every workload also carries the
// scratch emptyDir and its projected identity, neither of which is storage.
func storagePairs(spec *corev1.PodSpec) ([]corev1.Volume, []corev1.VolumeMount) {
	always := map[string]bool{"scratch": true, "identity": true}
	var vols []corev1.Volume
	for _, v := range spec.Volumes {
		if !always[v.Name] {
			vols = append(vols, v)
		}
	}
	var mounts []corev1.VolumeMount
	for _, m := range spec.Containers[0].VolumeMounts {
		if !always[m.Name] {
			mounts = append(mounts, m)
		}
	}
	return vols, mounts
}

// The volumes and the mount points come from one slice, so a pod can never
// carry a mount point with no volume behind it — which the kubelet reports as a
// CreateContainerConfigError naming neither the app nor the resource.
func TestPodCarriesItsMountsAndTheirDescription(t *testing.T) {
	b, _ := testBackend(t)

	spec := testSpec()
	spec.Mounts = []runtime.Mount{
		{Name: "cache", Claim: "lpv-cache", Path: "/data/cache", Kind: runtime.MountVolume, Access: runtime.AccessFull},
		{Name: "models", Claim: "lpv-models-read", Path: "/data/models", Kind: runtime.MountStore, Access: runtime.AccessRead},
	}

	podSpec, err := b.podSpec(context.Background(), spec, "serve", "")
	if err != nil {
		t.Fatalf("podSpec: %v", err)
	}

	volumes, mounts := storagePairs(podSpec)
	if len(volumes) != 2 {
		t.Fatalf("got %d storage volumes, want 2", len(volumes))
	}
	if len(mounts) != 2 {
		t.Fatalf("got %d storage mount points, want 2", len(mounts))
	}
	for i, v := range volumes {
		if v.Name != mounts[i].Name {
			t.Errorf("volume %q has no matching mount point (%q)", v.Name, mounts[i].Name)
		}
		if v.PersistentVolumeClaim == nil {
			t.Fatalf("volume %q is not a claim", v.Name)
		}
		if v.PersistentVolumeClaim.ClaimName != spec.Mounts[i].Claim {
			t.Errorf("volume %q binds %q, want %q", v.Name,
				v.PersistentVolumeClaim.ClaimName, spec.Mounts[i].Claim)
		}
	}

	// A read-only mapping is read-only at the pod as well. For a volume this is
	// the *only* thing enforcing the level, since one claim serves every level.
	if mounts[0].ReadOnly {
		t.Error("a write mapping was mounted read-only")
	}
	if !mounts[1].ReadOnly {
		t.Error("a read mapping must be mounted read-only")
	}

	// And the app is told what it has, from the same slice.
	var descs []runtime.MountDescriptor
	for _, env := range podSpec.Containers[0].Env {
		if env.Name != runner.EnvStorage {
			continue
		}
		if err := json.Unmarshal([]byte(env.Value), &descs); err != nil {
			t.Fatalf("%s is not valid JSON: %v", runner.EnvStorage, err)
		}
	}
	if len(descs) != 2 {
		t.Fatalf("%s described %d mounts, want 2", runner.EnvStorage, len(descs))
	}
	if descs[0].Name != "cache" || descs[0].Path != "/data/cache" {
		t.Errorf("first descriptor = %+v", descs[0])
	}
}

// An app with no storage carries no storage volumes and no description — the
// variable is absent rather than empty, so code checking for it gets a clean
// answer. (The scratch emptyDir is always there; it is not storage.)
func TestAppsWithoutStorageAreUnchanged(t *testing.T) {
	b, _ := testBackend(t)

	podSpec, err := b.podSpec(context.Background(), testSpec(), "serve", "")
	if err != nil {
		t.Fatalf("podSpec: %v", err)
	}
	if volumes, mounts := storagePairs(podSpec); len(volumes) != 0 || len(mounts) != 0 {
		t.Error("an app with no mappings should carry no storage volumes")
	}
	for _, env := range podSpec.Containers[0].Env {
		if env.Name == runner.EnvStorage {
			t.Errorf("%s should be absent, got %q", runner.EnvStorage, env.Value)
		}
	}
	// And the invariant that survives all of this: nothing mounts the service
	// account token, whatever else the pod carries.
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		t.Error("AutomountServiceAccountToken must stay false")
	}
}

// A job pod gets the same storage its app's service pods get — same spec,
// same rendering — which is what makes sharing between an app and its own
// background work need no additional concept.
func TestJobPodsGetTheSameMounts(t *testing.T) {
	b, _ := testBackend(t)

	spec := testSpec()
	spec.Mounts = []runtime.Mount{
		{Name: "cache", Claim: "lpv-cache", Path: "/data/cache", Kind: runtime.MountVolume, Access: runtime.AccessFull},
	}

	service, err := b.podSpec(context.Background(), spec, "serve", "")
	if err != nil {
		t.Fatalf("service podSpec: %v", err)
	}
	job, err := b.podSpec(context.Background(), spec, "job", "run-1")
	if err != nil {
		t.Fatalf("job podSpec: %v", err)
	}

	if len(job.Volumes) != len(service.Volumes) {
		t.Fatalf("job carries %d volumes, service carries %d", len(job.Volumes), len(service.Volumes))
	}
	if job.Containers[0].VolumeMounts[0].MountPath != service.Containers[0].VolumeMounts[0].MountPath {
		t.Error("a job's mount path differs from its app's")
	}
}

// --- Removal ---

// Removal sweeps every level rather than only those in use: by the time a
// resource can be deleted its mappings are gone, and there is nothing left to
// ask which levels existed.
func TestRemoveSweepsEveryAccessLevel(t *testing.T) {
	b, _ := testBackend(t)
	ctx := context.Background()

	res := runtime.StorageResource{
		Slug: "data", Kind: runtime.MountStore, Bucket: "b", Prefix: "p",
	}
	for _, access := range []string{runtime.AccessRead, runtime.AccessFull} {
		r := res
		r.Access = access
		if err := b.EnsureStorage(ctx, r); err != nil {
			t.Fatalf("ensure %s: %v", access, err)
		}
	}

	if err := b.RemoveStorage(ctx, res); err != nil {
		t.Fatalf("remove: %v", err)
	}

	claims, err := b.client.CoreV1().PersistentVolumeClaims("toft-apps").
		List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claims.Items {
		if strings.HasPrefix(c.Name, "lpv-data") {
			t.Errorf("claim %q survived the removal", c.Name)
		}
	}
	volumes, err := b.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range volumes.Items {
		if strings.Contains(v.Name, "lpv-data") {
			t.Errorf("volume %q survived the removal", v.Name)
		}
	}
}

// Ensuring twice is what an administrator retrying a half-failed create does,
// and it must not leave a second access point behind — on a shared filesystem
// that is a second empty directory that looks exactly like the first.
func TestEnsureIsIdempotent(t *testing.T) {
	b, _ := testBackend(t)
	b.cfg.StorageClass = "efs-shared"
	ctx := context.Background()

	res := runtime.StorageResource{Slug: "cache", Kind: runtime.MountVolume}
	for i := 0; i < 3; i++ {
		if err := b.EnsureStorage(ctx, res); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}

	claims, err := b.client.CoreV1().PersistentVolumeClaims("toft-apps").
		List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 1 {
		t.Fatalf("got %d claims after three ensures, want 1", len(claims.Items))
	}
}

// --- Keeping an app away from the node's identity ---

// On a self-managed cluster the CSI drivers authenticate as the *node*. An app
// pod that could reach the metadata endpoint could take that identity and read
// the buckets and filesystems its mounts were supposed to mediate — going
// straight past the containment the whole storage design rests on.
//
// This is the isolated half of BLOCK_AWS_METADATA; the shared half is an
// iptables rule keyed on the app accounts' group.
func TestBlockedEgressKeepsAppsOffTheMetadataEndpoint(t *testing.T) {
	b, _ := testBackend(t)
	b.cfg.BlockedEgressCIDRs = MetadataCIDRs
	ctx := context.Background()

	labels := b.labelsFor(testSpec(), "")
	if err := b.ensureNetworkPolicy(ctx, "demo-abc123", labels); err != nil {
		t.Fatalf("ensureNetworkPolicy: %v", err)
	}

	policy, err := b.client.NetworkingV1().NetworkPolicies("toft-apps").
		Get(ctx, "demo-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the policy: %v", err)
	}

	// Naming an egress rule at all is what makes egress default-deny, so the
	// policy type has to be declared or the rule below is inert.
	var hasEgressType bool
	for _, pt := range policy.Spec.PolicyTypes {
		if pt == networkingv1.PolicyTypeEgress {
			hasEgressType = true
		}
	}
	if !hasEgressType {
		t.Fatal("Egress is not in PolicyTypes, so the egress rule does nothing")
	}
	if len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].To) != 2 {
		t.Fatalf("egress = %+v", policy.Spec.Egress)
	}

	block := policy.Spec.Egress[0].To[0].IPBlock
	if block == nil || block.CIDR != "0.0.0.0/0" {
		t.Fatalf("egress must re-allow the world and carve out the exceptions, got %+v", block)
	}
	if !contains(block.Except, "169.254.169.254/32") {
		t.Errorf("the EC2 metadata endpoint is not blocked: %v", block.Except)
	}
	if !contains(block.Except, "169.254.170.2/32") {
		t.Errorf("the ECS task metadata endpoint is not blocked: %v", block.Except)
	}
	if v6 := policy.Spec.Egress[0].To[1].IPBlock; v6 == nil || v6.CIDR != "::/0" || !contains(v6.Except, "fd00:ec2::254/128") {
		t.Errorf("egress needs an IPv6 peer, or every IPv6 destination is denied: %+v", v6)
	}

	// The ingress half is untouched by any of this — it is what makes app↔app
	// separation real, and it does not become optional because egress arrived.
	if len(policy.Spec.Ingress) != 1 {
		t.Errorf("the ingress rule was lost: %+v", policy.Spec.Ingress)
	}
}

// Unset, egress stays open. Apps legitimately reach internal databases and
// third-party APIs, and a default-deny egress policy would break more than it
// protected — so the switch has to be the operator's.
func TestEgressIsUnrestrictedByDefault(t *testing.T) {
	b, _ := testBackend(t)
	ctx := context.Background()

	labels := b.labelsFor(testSpec(), "")
	if err := b.ensureNetworkPolicy(ctx, "demo-abc123", labels); err != nil {
		t.Fatalf("ensureNetworkPolicy: %v", err)
	}
	policy, err := b.client.NetworkingV1().NetworkPolicies("toft-apps").
		Get(ctx, "demo-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, pt := range policy.Spec.PolicyTypes {
		if pt == networkingv1.PolicyTypeEgress {
			t.Error("egress must not be restricted unless an operator asked for it")
		}
	}
	if len(policy.Spec.Egress) != 0 {
		t.Errorf("egress = %+v, want none", policy.Spec.Egress)
	}
}

func hasOption(opts []string, want string) bool { return contains(opts, want) }

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// The two defaults are the declared classes until KUBE_STORAGE_CLASSES adds
// more, each with the backing it serves.
func TestTheDefaultClassesAreDeclared(t *testing.T) {
	b, client := testBackend(t)
	b.cfg.StorageClass = "efs-toft"
	b.cfg.S3FilesStorageClass = "s3files-toft"
	if _, err := client.StorageV1().StorageClasses().Create(context.Background(), &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "efs-toft"}, Provisioner: "efs.csi.aws.com",
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	got := b.DeclaredClasses()
	want := []runtime.DeclaredClass{
		{Name: "efs-toft", Backing: runtime.BackingEFS, Default: true},
		{Name: "s3files-toft", Backing: runtime.BackingS3Files, Default: true},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("declared = %+v, want %+v", got, want)
	}
}

// KUBE_STORAGE_CLASSES adds drives after the defaults; a class whose backing is
// not stated takes the provisioner's reading, and the backings offered are the
// union of what the declared classes serve.
func TestExtraClassesAreDeclaredAndOffered(t *testing.T) {
	b, client := testBackend(t)
	b.cfg.StorageClass = "efs-toft"
	b.cfg.ExtraStorageClasses = []runtime.DeclaredClass{{Name: "ord-efs"}, {Name: "ord-s3", Backing: runtime.BackingS3Files}}
	for _, name := range []string{"efs-toft", "ord-efs"} {
		if _, err := client.StorageV1().StorageClasses().Create(context.Background(), &storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: "efs.csi.aws.com",
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	got := b.DeclaredClasses()
	if len(got) != 3 || got[1] != (runtime.DeclaredClass{Name: "ord-efs", Backing: runtime.BackingEFS}) ||
		got[2] != (runtime.DeclaredClass{Name: "ord-s3", Backing: runtime.BackingS3Files}) {
		t.Errorf("declared = %+v", got)
	}
	if backings := strings.Join(b.StorageBackings(runtime.MountVolume), ","); backings != "efs,s3files" {
		t.Errorf("backings = %s, want efs,s3files — S3 Files offered by the extra class alone", backings)
	}
}

// The evidence a cluster re-point rests on: the file system a volume's claims
// are bound to, read from the PV's volumeHandle, and the one a class names.
func TestBoundFileSystemsReadsTheClaimsVolumes(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	res := runtime.StorageResource{Slug: "sales-data", Kind: runtime.MountVolume, Backing: runtime.BackingEFS}
	claim := runtime.BindingName(res.Slug, res.Kind, runtime.AccessRead, "")
	if _, err := client.CoreV1().PersistentVolumes().Create(ctx, &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "efs.csi.aws.com", VolumeHandle: "fs-aaa::fsap-1"},
		}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims("toft-apps").Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claim},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-1"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := b.BoundFileSystems(ctx, res, []string{"never-bound"})
	if err != nil || len(got) != 1 || got[0] != "fs-aaa" {
		t.Errorf("bound = %v (%v), want [fs-aaa]", got, err)
	}

	if _, err := client.StorageV1().StorageClasses().Create(ctx, &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ord-efs"}, Provisioner: "efs.csi.aws.com",
		Parameters: map[string]string{"fileSystemId": "fs-aaa"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if p, fs, err := b.ClassFileSystem(ctx, "ord-efs"); err != nil || p != "efs.csi.aws.com" || fs != "fs-aaa" {
		t.Errorf("class = %q %q (%v)", p, fs, err)
	}
}

// AP-1: a binding's name carries no bucket and no prefix, so a store deleted
// and created again under the same name must not inherit a folder binding the
// old one left behind. The stale volume is released and the call refused; the
// retry binds the new bucket.
func TestAStoreBindingLeftByAnEarlierResourceIsReleasedNotInherited(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	old := runtime.StorageResource{Slug: "reports", Kind: runtime.MountStore,
		Bucket: "b1", Prefix: "team-a", Folder: "raw", Access: runtime.AccessRead}
	if err := b.EnsureStorage(ctx, old); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	pvName := b.storePVName(old)

	again := old
	if err := b.EnsureStorage(ctx, again); err != nil {
		t.Fatalf("the same binding twice must stay idempotent: %v", err)
	}

	fresh := old
	fresh.Bucket = "b2"
	err := b.EnsureStorage(ctx, fresh)
	if err == nil || !strings.Contains(err.Error(), "earlier storage resource") {
		t.Fatalf("ensure over a stale binding = %v, want a refusal naming the earlier resource", err)
	}
	if _, err := client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the stale volume should be released, got %v", err)
	}
	if err := b.EnsureStorage(ctx, fresh); err != nil {
		t.Fatalf("retry: %v", err)
	}
	pv, err := client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := pv.Spec.CSI.VolumeAttributes["bucketName"]; got != "b2" {
		t.Errorf("bucketName = %q, want b2", got)
	}

	// A prefix that moved is the same mismatch.
	moved := fresh
	moved.Prefix = "team-b"
	if err := b.EnsureStorage(ctx, moved); err == nil {
		t.Error("a stale binding with another prefix was accepted")
	}
}

// K8-D1: a volume's folder mapping mounts the volume's one claim with the
// folder as a subPath, so it is the same tree a whole-volume mapping sees. At
// read the claim source stays read-write — the kubelet creates a missing
// subPath through it — and the container's mount carries the level.
func TestStorageVolumesMountAVolumeFolderAsASubPathOfItsClaim(t *testing.T) {
	m := runtime.Mount{Name: "shared", Kind: runtime.MountVolume, Access: runtime.AccessRead,
		Folder: "inbox", Path: "/data/inbox",
		Claim: runtime.BindingName("shared", runtime.MountVolume, runtime.AccessRead, "inbox")}
	volumes, mounts := storageVolumes([]runtime.Mount{m})
	if len(volumes) != 1 || len(mounts) != 1 {
		t.Fatalf("got %d volumes and %d mounts", len(volumes), len(mounts))
	}
	if got := volumes[0].PersistentVolumeClaim.ClaimName; got != "lpv-shared" {
		t.Errorf("claim = %q, want the volume's one claim lpv-shared", got)
	}
	if mounts[0].SubPath != "inbox" {
		t.Errorf("subPath = %q, want inbox", mounts[0].SubPath)
	}
	if volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Error("a read folder's claim source is read-only, so the kubelet cannot create the subPath")
	}
	if !mounts[0].ReadOnly {
		t.Error("a read folder must be mounted read-only in the container")
	}

	// A whole-volume read mapping keeps the read-only source.
	whole := m
	whole.Folder = ""
	volumes, _ = storageVolumes([]runtime.Mount{whole})
	if !volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Error("a whole-volume read mapping lost its read-only source")
	}
}

// LSB-1: a volume's claim is lpv-<slug> and a store's lpv-<slug>-<level>, in one
// namespace, so volume reports-read and store reports name one claim. Neither
// ensure adopts the other's claim, in either order, and the store leaves no
// volume behind it; a store's release never takes the volume's claim.
func TestAStoreBindingClashWithAVolumeIsRefusedNotAdopted(t *testing.T) {
	ctx := context.Background()
	volume := runtime.StorageResource{Slug: "reports-read", Kind: runtime.MountVolume}
	store := runtime.StorageResource{Slug: "reports", Kind: runtime.MountStore,
		Bucket: "acme-data", Prefix: "team/reports", Access: runtime.AccessRead}

	// Volume first, then the store.
	b, client := testBackend(t)
	b.cfg.StorageClass = "efs-shared"
	if err := b.EnsureStorage(ctx, volume); err != nil {
		t.Fatalf("volume ensure: %v", err)
	}
	err := b.EnsureStorage(ctx, store)
	if err == nil || !strings.Contains(err.Error(), "another storage resource") {
		t.Fatalf("store ensure over the volume's claim = %v, want a refusal", err)
	}
	if _, err := client.CoreV1().PersistentVolumes().Get(ctx, "toft-apps-lpv-reports-read", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the store's volume was left behind: %v", err)
	}
	if err := b.RemoveStorage(ctx, store); err != nil {
		t.Fatalf("RemoveStorage(store): %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims("toft-apps").Get(ctx, "lpv-reports-read", metav1.GetOptions{}); err != nil {
		t.Errorf("releasing the store took the volume's claim: %v", err)
	}
	if err := b.releaseStoreBinding(ctx, "lpv-reports-read", "toft-apps-lpv-reports-read"); err != nil {
		t.Fatalf("releaseStoreBinding: %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims("toft-apps").Get(ctx, "lpv-reports-read", metav1.GetOptions{}); err != nil {
		t.Errorf("releasing a store folder binding of that name took the volume's claim: %v", err)
	}

	// Store first, then the volume.
	b, client = testBackend(t)
	b.cfg.StorageClass = "efs-shared"
	if err := b.EnsureStorage(ctx, store); err != nil {
		t.Fatalf("store ensure: %v", err)
	}
	err = b.EnsureStorage(ctx, volume)
	if err == nil || !strings.Contains(err.Error(), "store's binding") {
		t.Fatalf("volume ensure over the store's claim = %v, want a refusal", err)
	}
	if err := b.RemoveStorage(ctx, volume); err != nil {
		t.Fatalf("RemoveStorage(volume): %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims("toft-apps").Get(ctx, "lpv-reports-read", metav1.GetOptions{}); err != nil {
		t.Errorf("releasing the volume took the store's claim: %v", err)
	}
	// Each still ensures and releases its own.
	if err := b.EnsureStorage(ctx, store); err != nil {
		t.Errorf("the store's own re-ensure: %v", err)
	}
	if err := b.RemoveStorage(ctx, store); err != nil {
		t.Fatalf("RemoveStorage(store): %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims("toft-apps").Get(ctx, "lpv-reports-read", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the store's own claim was not released: %v", err)
	}
}
