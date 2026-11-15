package kube

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeobj "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"toft/internal/runtime"
)

// Destroying a volume resource (ADR §12a): the class keeps Retain, and the
// resource delete flips its own volumes — and only its own — to Delete.

func efsClass(name string, removes bool) *storagev1.StorageClass {
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: "efs.csi.aws.com"}
	if removes {
		sc.Annotations = map[string]string{DeleteRemovesDataAnnotation: "true"}
	}
	return sc
}

// boundClaim is a claim in ns bound to pv, and pv bound back to it.
func boundClaim(t *testing.T, client *fake.Clientset, ns, claim, pv string) {
	t.Helper()
	ctx := context.Background()
	if _, err := client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: ns},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pv},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().PersistentVolumes().Create(ctx, &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pv},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef:                      &corev1.ObjectReference{Namespace: ns, Name: claim},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func reclaimOf(t *testing.T, client *fake.Clientset, pv string) corev1.PersistentVolumeReclaimPolicy {
	t.Helper()
	got, err := client.CoreV1().PersistentVolumes().Get(context.Background(), pv, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading %s: %v", pv, err)
	}
	return got.Spec.PersistentVolumeReclaimPolicy
}

// The silent failure this guards: a destroy that patches a volume it did not
// reach through one of the resource's own recorded claims deletes another
// resource's data, and nothing on any page would say so.
func TestDestroyFlipsOnlyTheVolumesOfItsOwnClaims(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	if _, err := client.StorageV1().StorageClasses().Create(ctx, efsClass("efs-toft", true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	boundClaim(t, client, "toft-apps", "lpv-share", "pv-share")
	boundClaim(t, client, "toft-apps", "lpv-share-0a1b2c3d", "pv-share-teams")
	boundClaim(t, client, "toft-apps", "lpv-other", "pv-other")        // another resource
	boundClaim(t, client, "someone-else", "lpv-share", "pv-elsewhere") // another namespace

	res := runtime.StorageResource{Slug: "share", Kind: runtime.MountVolume, Backing: runtime.BackingEFS, StorageClass: "efs-toft"}
	report, err := b.DestroyStorage(ctx, res, []string{"lpv-share", "lpv-share-0a1b2c3d", "lpv-share-gone"})
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}

	for _, pv := range []string{"pv-share", "pv-share-teams"} {
		if got := reclaimOf(t, client, pv); got != corev1.PersistentVolumeReclaimDelete {
			t.Errorf("%s = %s, want Delete", pv, got)
		}
	}
	for _, pv := range []string{"pv-other", "pv-elsewhere"} {
		if got := reclaimOf(t, client, pv); got != corev1.PersistentVolumeReclaimRetain {
			t.Errorf("%s = %s — the destroy reached a volume that is not its own", pv, got)
		}
	}
	if strings.Join(report.Pending, ",") != "pv-share,pv-share-teams" || !report.RemovesData {
		t.Errorf("report = %+v, want both volumes pending and the data going", report)
	}
	if len(report.LeftBehind) != 1 || report.LeftBehind[0].What != "lpv-share-gone" {
		t.Errorf("left behind = %+v, want the claim already released named", report.LeftBehind)
	}
	for _, claim := range []string{"lpv-share", "lpv-share-0a1b2c3d"} {
		if _, err := client.CoreV1().PersistentVolumeClaims("toft-apps").Get(ctx, claim, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("claim %s survived the destroy: %v", claim, err)
		}
	}

	if gone, _ := b.VolumesGone(ctx, report.Pending); gone {
		t.Error("VolumesGone said yes while the volumes are still there")
	}
	for _, pv := range report.Pending {
		_ = client.CoreV1().PersistentVolumes().Delete(ctx, pv, metav1.DeleteOptions{})
	}
	if gone, err := b.VolumesGone(ctx, report.Pending); !gone || err != nil {
		t.Errorf("VolumesGone = %v, %v once deleted; want true", gone, err)
	}
}

// A claim whose volume says it belongs to someone else is never flipped, even
// though the claim names it.
func TestDestroyRefusesAVolumeBoundElsewhere(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	boundClaim(t, client, "toft-apps", "lpv-decoy", "pv-precious")
	if _, err := client.CoreV1().PersistentVolumeClaims("toft-apps").Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "lpv-share", Namespace: "toft-apps"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-precious"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	res := runtime.StorageResource{Slug: "share", Kind: runtime.MountVolume, Backing: runtime.BackingEFS}
	if _, err := b.DestroyStorage(ctx, res, []string{"lpv-share"}); err == nil {
		t.Error("destroy succeeded through a claim whose volume is bound to another claim")
	}
	if got := reclaimOf(t, client, "pv-precious"); got != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("pv-precious = %s, want untouched", got)
	}
}

// A cluster that will not let the platform patch a volume gets today's release
// and a report that says the data stayed — never a failed delete.
func TestDestroyWithoutPatchReleasesAndSaysSo(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	boundClaim(t, client, "toft-apps", "lpv-share", "pv-share")
	client.PrependReactor("patch", "persistentvolumes", func(k8stesting.Action) (bool, runtimeobj.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "pv-share", nil)
	})

	res := runtime.StorageResource{Slug: "share", Kind: runtime.MountVolume, Backing: runtime.BackingEFS}
	report, err := b.DestroyStorage(ctx, res, []string{"lpv-share"})
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if report.RemovesData || len(report.Pending) != 0 || len(report.LeftBehind) != 1 ||
		!strings.Contains(report.LeftBehind[0].Reason, "patch") {
		t.Errorf("report = %+v, want nothing pending and the kept data explained", report)
	}
	if got := reclaimOf(t, client, "pv-share"); got != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("pv-share = %s, want Retain", got)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims("toft-apps").Get(ctx, "lpv-share", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the claim was not released: %v", err)
	}
}

// A class that does not promise the directory goes still has its volumes
// flipped — that frees the access point — and the report says the files stay.
func TestDestroyOnAnUnannotatedClassSaysTheFilesStay(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	if _, err := client.StorageV1().StorageClasses().Create(ctx, efsClass("efs-toft", false), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	boundClaim(t, client, "toft-apps", "lpv-share", "pv-share")

	res := runtime.StorageResource{Slug: "share", Kind: runtime.MountVolume, Backing: runtime.BackingEFS, StorageClass: "efs-toft"}
	report, err := b.DestroyStorage(ctx, res, []string{"lpv-share"})
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got := reclaimOf(t, client, "pv-share"); got != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("pv-share = %s, want Delete — the access point should still go", got)
	}
	if report.RemovesData || len(report.LeftBehind) != 1 || !strings.Contains(report.LeftBehind[0].Reason, "deleteAccessPointRootDir") {
		t.Errorf("report = %+v, want the directory left behind with the driver setting named", report)
	}
}

// A store is a prefix in a bucket the operator lent: the destroy releases it.
func TestDestroyingAStoreOnlyReleasesIt(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	res := runtime.StorageResource{Slug: "drop", Kind: runtime.MountStore, Bucket: "b", Prefix: "p", Access: runtime.AccessRead}
	if err := b.EnsureStorage(ctx, res); err != nil {
		t.Fatal(err)
	}
	report, err := b.DestroyStorage(ctx, res, nil)
	if err != nil || report.RemovesData || len(report.Pending) != 0 {
		t.Fatalf("destroy = %+v, %v; want a plain release", report, err)
	}
	pvs, _ := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	for _, pv := range pvs.Items {
		t.Errorf("volume %s survived the release", pv.Name)
	}
}

// What each class says a delete removes.
func TestDeleteVerdict(t *testing.T) {
	cases := []struct {
		name  string
		class *storagev1.StorageClass
		want  bool
	}{
		{"efs annotated", efsClass("c", true), true},
		{"efs plain", efsClass("c", false), false},
		{"azure share per claim", &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Provisioner: "file.csi.azure.com"}, true},
		{"azure fixed share", &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Provisioner: "file.csi.azure.com",
			Parameters: map[string]string{"shareName": "toft"}}, false},
		{"in-tree azure", &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Provisioner: "kubernetes.io/azure-file"}, true},
		{"unknown driver, annotated anyway", &storagev1.StorageClass{Provisioner: "nfs.csi.k8s.io",
			ObjectMeta: metav1.ObjectMeta{Name: "c", Annotations: map[string]string{DeleteRemovesDataAnnotation: "true"}}}, false},
		{"missing class", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, client := testBackend(t)
			if tc.class != nil {
				if _, err := client.StorageV1().StorageClasses().Create(context.Background(), tc.class, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			got, why := b.deleteVerdict(context.Background(), "c")
			if got != tc.want {
				t.Errorf("verdict = %v (%s), want %v", got, why, tc.want)
			}
			if !got && why == "" {
				t.Error("a verdict of no came with no reason")
			}
		})
	}
}

// AP-1: a store's folder bindings are in its recorded claims, and the destroy
// releases them as well as the root ones.
func TestDestroyingAStoreReleasesItsFolderBindings(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	root := runtime.StorageResource{Slug: "drop", Kind: runtime.MountStore, Bucket: "b", Prefix: "p", Access: runtime.AccessRead}
	folder := root
	folder.Folder = "raw"
	for _, r := range []runtime.StorageResource{root, folder} {
		if err := b.EnsureStorage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	claim := runtime.BindingName(folder.Slug, folder.Kind, folder.Access, folder.Folder)
	if _, err := b.DestroyStorage(ctx, runtime.StorageResource{Slug: "drop", Kind: runtime.MountStore}, []string{claim}); err != nil {
		t.Fatal(err)
	}
	pvs, _ := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	for _, pv := range pvs.Items {
		t.Errorf("volume %s survived the destroy", pv.Name)
	}
	pvcs, _ := client.CoreV1().PersistentVolumeClaims(b.cfg.Namespace).List(ctx, metav1.ListOptions{})
	for _, pvc := range pvcs.Items {
		t.Errorf("claim %s survived the destroy", pvc.Name)
	}
}

// LSB-1: a volume whose recorded claim turned out to be a store's binding —
// volume reports-read and store reports made one claim on an install from
// before the create refused the pair — neither flips the store's volume to
// Delete nor releases its claim, and says so.
func TestDestroyLeavesAStoreBindingRecordedAsAVolumesClaim(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	if _, err := client.StorageV1().StorageClasses().Create(ctx, efsClass("efs-toft", true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	store := runtime.StorageResource{Slug: "reports", Kind: runtime.MountStore, Bucket: "b", Prefix: "p", Access: runtime.AccessRead}
	if err := b.EnsureStorage(ctx, store); err != nil {
		t.Fatal(err)
	}
	vol := runtime.StorageResource{Slug: "reports-read", Kind: runtime.MountVolume, Backing: runtime.BackingEFS, StorageClass: "efs-toft"}
	report, err := b.DestroyStorage(ctx, vol, []string{"lpv-reports-read"})
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got := reclaimOf(t, client, "toft-apps-lpv-reports-read"); got != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("the store's volume was flipped to %s", got)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims("toft-apps").Get(ctx, "lpv-reports-read", metav1.GetOptions{}); err != nil {
		t.Errorf("the store's claim was released: %v", err)
	}
	if len(report.LeftBehind) != 1 || !strings.Contains(report.LeftBehind[0].Reason, "store's binding") || len(report.Pending) != 0 {
		t.Errorf("report = %+v, want the claim left behind as a store's", report)
	}
}
