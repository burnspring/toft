package kube

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"toft/internal/runtime"
)

// Durable storage, as Kubernetes objects.
//
// Toft creates *binding* objects only — a PersistentVolumeClaim, and for
// an object store the static PersistentVolume behind it. The filesystem and the
// bucket already exist; the infrastructure module made them, and the platform's
// credentials should not be able to. This is the same rule as "Toft never
// creates buckets", and it is why nothing in this file needs an AWS permission
// of any kind.
//
// Two properties are load-bearing and neither is negotiable:
//
//   - **Retain on every class, Delete on one volume at the moment of its
//     resource's delete.** Under a Delete class, removing a claim destroys the
//     access point and everything in it, so a namespace deleted, a Helm
//     uninstall or a stray `kubectl delete pvc` would be data loss. The class
//     keeps Retain for every one of those; DestroyStorage — the resource
//     delete's, and only its — flips the resource's own volumes to Delete
//     before releasing their claims, and the driver removes what it made.
//   - **The mount is the boundary.** Every pod runs as runnerUID, so filesystem
//     modes cannot distinguish one app from another. Isolation comes from the
//     access point's root-directory jail and the object mount's prefix, both
//     enforced below the pod. Never mount a parent and subdivide it.

// storageSize is the capacity field Kubernetes requires and neither backing
// honours: EFS ignores a claim's request outright, and Mountpoint has no
// capacity concept at all. It is written large enough that nothing schedules
// against it as if it were a real limit. Per-volume quotas remain unsolved;
// see the PRD's known limits.
var storageSize = resource.MustParse("1Ti")

// EnsureStorage makes one resource's binding exist at one access level.
//
// Idempotent by construction: an administrator retrying a half-failed create
// must not end up with a second access point, which on EFS would be a second
// empty directory that looks like the first.
func (b *Backend) EnsureStorage(ctx context.Context, res runtime.StorageResource) error {
	switch res.Kind {
	case runtime.MountVolume:
		return b.ensureVolumeClaim(ctx, res)
	case runtime.MountStore:
		return b.ensureStoreBinding(ctx, res)
	default:
		return fmt.Errorf("kube: unknown storage kind %q", res.Kind)
	}
}

// RemoveStorage releases every binding a resource could hold.
//
// It removes *bindings*, never data. Every level is swept rather than only the
// ones in use, because by the time a resource can be deleted its mappings are
// already gone and there is nothing left to ask which levels existed — and that
// includes the retired `overwrite`, whose bindings a promoted mapping left
// behind (runtime.Bindings).
func (b *Backend) RemoveStorage(ctx context.Context, res runtime.StorageResource) error {
	var firstErr error
	record := func(err error) {
		if err != nil && !apierrors.IsNotFound(err) && firstErr == nil {
			firstErr = err
		}
	}

	for _, binding := range runtime.Bindings(res) {
		name := runtime.BindingName(binding.Slug, binding.Kind, binding.Access, binding.Folder)
		record(b.deleteClaimIfKind(ctx, name, binding.Kind == runtime.MountStore))
		if binding.Kind == runtime.MountStore {
			record(b.client.CoreV1().PersistentVolumes().
				Delete(ctx, b.storePVName(binding), metav1.DeleteOptions{}))
		}
	}
	if firstErr != nil {
		return fmt.Errorf("kube: releasing storage %s: %w", res.Slug, firstErr)
	}
	return nil
}

// DestroyStorage deletes a volume resource's data by handing it to the driver.
//
// For each claim the resource was ever given: read the volume it is bound to,
// check that volume is this namespace's claim's and nothing else's, flip its
// reclaim policy to Delete, then release the claim. The PV controller asks the
// driver to delete the volume once nothing holds the claim — which a finished
// job pod can delay for up to jobTTL — so the volumes come back Pending and the
// caller holds the slug until VolumesGone. Every step is safe to repeat: a
// destroy cut short is finished by running it again.
//
// **What the driver will remove is the class's to say** (deleteVerdict). The
// flip happens either way — it is what frees the access point against the file
// system's quota — and when the class does not promise the directory goes, the
// report says it stays.
//
// A cluster that will not let the platform patch a volume (no fence, so no
// grant — ADR §12a) gets today's release and an honest report: a binary newer
// than its RBAC degrades rather than failing the delete, the rule cannotRead
// states. A store is a prefix in a bucket the operator lent: released, never
// destroyed.
func (b *Backend) DestroyStorage(ctx context.Context, res runtime.StorageResource, claims []string) (runtime.DestroyReport, error) {
	if res.Kind == runtime.MountStore {
		if err := b.RemoveStorage(ctx, res); err != nil {
			return runtime.DestroyReport{}, err
		}
		// RemoveStorage knows only the root bindings. A store's folder
		// bindings are in claims (ensureBinding records them), and one left
		// here would be inherited by the next store of this name.
		for _, claim := range claims {
			if err := b.releaseStoreBinding(ctx, claim, b.cfg.Namespace+"-"+claim); err != nil {
				return runtime.DestroyReport{}, err
			}
		}
		return runtime.DestroyReport{}, nil
	}
	class := b.storageClassFor(res)
	removes, why := b.deleteVerdict(ctx, class)
	report := runtime.DestroyReport{RemovesData: removes}

	for _, claim := range claims {
		pvc, err := b.client.CoreV1().PersistentVolumeClaims(b.cfg.Namespace).Get(ctx, claim, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			report.LeftBehind = append(report.LeftBehind, runtime.LeftBehind{
				What:   claim,
				Reason: "the claim had already been released, so nothing says which volume was behind it",
			})
			continue
		}
		if err != nil {
			return report, fmt.Errorf("kube: reading claim %s: %w", claim, err)
		}
		if b.claimIsStoreBinding(pvc, claim) {
			// A volume and a store whose names made one claim (volume
			// reports-full, store reports): the claim is the store's, and
			// flipping its volume would delete the store's binding.
			report.LeftBehind = append(report.LeftBehind, runtime.LeftBehind{
				What:   claim,
				Reason: "this claim is a store's binding, not this volume's, so it was neither flipped nor released",
			})
			continue
		}
		if pvc.Spec.VolumeName == "" {
			// Never bound: nothing was provisioned, so there is nothing to keep.
			if err := b.deleteClaim(ctx, claim); err != nil {
				return report, err
			}
			report.Removed = append(report.Removed, claim)
			continue
		}

		pvName := pvc.Spec.VolumeName
		flipped, err := b.flipToDelete(ctx, pvName, claim)
		if err != nil {
			return report, err
		}
		if err := b.deleteClaim(ctx, claim); err != nil {
			return report, err
		}
		if !flipped {
			report.LeftBehind = append(report.LeftBehind, runtime.LeftBehind{
				What: claim,
				Reason: "this cluster does not let the platform delete volume data — the platform has no " +
					"patch on persistentvolumes, which the chart grants only beside the admission policy " +
					"that fences it (Kubernetes 1.30 or later); the claim was released and its data kept",
			})
			continue
		}
		report.Pending = append(report.Pending, pvName)
		if !removes {
			report.LeftBehind = append(report.LeftBehind, runtime.LeftBehind{
				What:   volumeDirectory(pvc, claim),
				Reason: why,
			})
		}
	}
	if len(report.Pending) == 0 {
		report.RemovesData = false
	}
	return report, nil
}

// flipToDelete sets one volume's reclaim policy to Delete, after checking it is
// the volume of this namespace's claim of that name. It reports false, and no
// error, when the cluster refuses the patch on permission grounds.
func (b *Backend) flipToDelete(ctx context.Context, pvName, claim string) (bool, error) {
	pv, err := b.client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("kube: reading volume %s: %w", pvName, err)
	}
	// The claim says which volume; the volume has to agree. A volume bound
	// anywhere else is never this resource's, whatever the claim reads.
	ref := pv.Spec.ClaimRef
	if ref == nil || ref.Namespace != b.cfg.Namespace || ref.Name != claim {
		return false, fmt.Errorf("kube: volume %s is not bound to %s/%s, so it is not flipped", pvName, b.cfg.Namespace, claim)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete {
		return true, nil
	}
	patch := []byte(`{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`)
	_, err = b.client.CoreV1().PersistentVolumes().Patch(ctx, pvName, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		log.Printf("kube: the platform may not patch volume %s, so its data is kept: %v", pvName, err)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kube: flipping volume %s to Delete: %w", pvName, err)
	}
	return true, nil
}

func (b *Backend) deleteClaim(ctx context.Context, claim string) error {
	err := b.client.CoreV1().PersistentVolumeClaims(b.cfg.Namespace).Delete(ctx, claim, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: releasing claim %s: %w", claim, err)
	}
	return nil
}

// volumeDirectory names where a kept volume's files are, as an operator would
// look for them: the class's subPathPattern makes the directory the claim's
// name, on the file system the volume handle names.
func volumeDirectory(pvc *corev1.PersistentVolumeClaim, claim string) string {
	if pvc.Spec.VolumeName == "" {
		return claim
	}
	return "/" + claim + " (volume " + pvc.Spec.VolumeName + ")"
}

// VolumesGone reports whether every volume is deleted. A volume still there in
// Failed is the driver refusing, which the caller shows by the delete's age.
func (b *Backend) VolumesGone(ctx context.Context, volumes []string) (bool, error) {
	for _, name := range volumes {
		_, err := b.client.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("kube: reading volume %s: %w", name, err)
		}
		return false, nil
	}
	return true, nil
}

// DeleteRemovesDataAnnotation is how an operator says a class's driver deletes
// a volume's directory along with its access point. On EFS that is the
// controller's deleteAccessPointRootDir, a setting of the driver rather than of
// the class, so nothing Toft can read says it — and **the annotation also
// asserts the controller is v3.4.1 or later**, because before it that flag did
// not check an access point belonged to the volume's own file system (AWS
// security bulletin 2026-099).
const DeleteRemovesDataAnnotation = "toft.dev/delete-removes-data"

// deleteVerdict is whether deleting a volume on this class removes its files,
// and when not, why. Read from the class each time, because an operator
// annotates a class after the platform started.
func (b *Backend) deleteVerdict(ctx context.Context, class string) (bool, string) {
	if class == "" {
		return false, "the volume names no storage class, so nothing says what its driver removes"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	sc, err := b.client.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
	if err != nil {
		return false, "the storage class " + class + " could not be read, so nothing says what its driver removes"
	}
	switch sc.Provisioner {
	case "efs.csi.aws.com":
		if sc.Annotations[DeleteRemovesDataAnnotation] == "true" {
			return true, ""
		}
		return false, "the EFS CSI driver keeps an access point's directory unless its controller runs " +
			"deleteAccessPointRootDir (v3.4.1 or later) and the class " + class + " says so with " +
			DeleteRemovesDataAnnotation + `: "true"`
	case "file.csi.azure.com", "kubernetes.io/azure-file":
		if sc.Parameters["shareName"] == "" {
			return true, ""
		}
		return false, "the storage class " + class + " names one fixed share, which outlives every volume on it"
	}
	return false, "the storage class " + class + " uses " + sc.Provisioner + ", whose deletes Toft cannot vouch for"
}

// StorageKinds reports which kinds this backend can actually provision, so the
// admin surface offers only what will work rather than a form that fails on
// submit. A file store needs a StorageClass an operator pointed at *some*
// filesystem — either backing will do — and a store needs only the CSI driver,
// which has a usable default.
func (b *Backend) StorageKinds() []string {
	kinds := []string{}
	if len(b.StorageBackings(runtime.MountVolume)) > 0 {
		kinds = append(kinds, runtime.MountVolume)
	}
	if b.cfg.StoreCSIDriver != "" {
		kinds = append(kinds, runtime.MountStore)
	}
	return kinds
}

// StorageBackings reports what this backend can put underneath a kind.
//
// A backing an operator configured no class for is *absent*, not offered and
// broken: the create form never shows it, which is the same rule the kinds list
// above follows and the same rule the whole optional-capability section of the
// invariants follows.
func (b *Backend) StorageBackings(kind string) []string {
	if kind != runtime.MountVolume {
		out := []string{}
		if b.cfg.StoreCSIDriver != "" {
			out = append(out, runtime.BackingMountpoint)
		}
		// A `blob` store needs the account the containers live in, and that is
		// the operator saying the driver is installed. Absent, the backing is
		// not offered rather than offered and broken.
		if b.cfg.StoreAccount != "" {
			out = append(out, runtime.BackingBlob)
		}
		return out
	}
	// The union of what the declared classes serve. Azure Files provisions from
	// the same class variable EFS does — a class variable per cloud is how a
	// configuration surface becomes a matrix — and the class's provisioner says
	// which of the two it is; a class that cannot be read serves either.
	out := []string{}
	seen := map[string]bool{}
	for _, c := range b.DeclaredClasses() {
		served := []string{c.Backing}
		if c.Backing == "" {
			served = []string{runtime.BackingEFS, runtime.BackingAzureFiles}
		}
		for _, backing := range served {
			if !seen[backing] {
				seen[backing] = true
				out = append(out, backing)
			}
		}
	}
	return out
}

// Provisioners whose class can only be one of the two backings that share
// KUBE_STORAGE_CLASS. The in-tree Azure name is still what older AKS classes
// carry.
var classProvisionerBacking = map[string]string{
	"efs.csi.aws.com":          runtime.BackingEFS,
	"file.csi.azure.com":       runtime.BackingAzureFiles,
	"kubernetes.io/azure-file": runtime.BackingAzureFiles,
}

// classBackings is which of EFS and Azure Files a class can provide.
//
// No cluster has both, so offering both put the other cloud's file system on
// every create form. The class's provisioner answers it, and it is read per
// call — an admin-only listing — rather than cached, so a class created after
// the platform started is seen. **Anything it cannot read or does not know
// offers both**, which is the form as it was: a wrong guess would hide the one
// backing that works, and a choice too many costs a sentence.
func (b *Backend) classBackings(class string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sc, err := b.client.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
	if err == nil {
		if backing, ok := classProvisionerBacking[sc.Provisioner]; ok {
			return []string{backing}
		}
	} else if !apierrors.IsNotFound(err) {
		log.Printf("kube: reading storage class %s to tell EFS from Azure Files: %v", class, err)
	}
	return []string{runtime.BackingEFS, runtime.BackingAzureFiles}
}

// DeclaredClasses is every class this install offers volumes from — the
// drives a cluster has. The two defaults first, then KUBE_STORAGE_CLASSES in
// the order an operator wrote it. A resource may name only one of these: a
// class nobody declared is a claim that waits for a provisioner forever, and
// the create used to accept any string.
func (b *Backend) DeclaredClasses() []runtime.DeclaredClass {
	out := []runtime.DeclaredClass{}
	seen := map[string]bool{}
	add := func(c runtime.DeclaredClass) {
		if c.Name == "" || seen[c.Name] {
			return
		}
		seen[c.Name] = true
		c.DeleteRemovesData, _ = b.deleteVerdict(context.Background(), c.Name)
		out = append(out, c)
	}
	if b.cfg.StorageClass != "" {
		backing := ""
		if read := b.classBackings(b.cfg.StorageClass); len(read) == 1 {
			backing = read[0]
		}
		add(runtime.DeclaredClass{Name: b.cfg.StorageClass, Backing: backing, Default: true})
	}
	if b.cfg.S3FilesStorageClass != "" {
		add(runtime.DeclaredClass{Name: b.cfg.S3FilesStorageClass, Backing: runtime.BackingS3Files, Default: true})
	}
	for _, c := range b.cfg.ExtraStorageClasses {
		if c.Backing == "" {
			if read := b.classBackings(c.Name); len(read) == 1 {
				c.Backing = read[0]
			}
		}
		c.Default = false
		add(c)
	}
	return out
}

// ClassFileSystem reads what a class provisions on: its driver and, for EFS
// and S3 Files, the `fileSystemId` parameter. It is the target half of a
// cluster re-point's proof (ADR §12a).
func (b *Backend) ClassFileSystem(ctx context.Context, class string) (provisioner, fileSystem string, err error) {
	sc, err := b.client.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	return sc.Provisioner, sc.Parameters["fileSystemId"], nil
}

// BoundFileSystems is the file systems a volume's existing claims are bound
// to, read from each claim's PV — the EFS CSI volumeHandle is
// `<fileSystemId>::<accessPointId>`. It is the evidence half of a cluster
// re-point: **a renamed class is a new object and the old one is usually gone,
// but the volumes bound through it are not**. A claim that does not exist is
// skipped; one with no bound volume yet carries no evidence and is skipped too.
func (b *Backend) BoundFileSystems(ctx context.Context, res runtime.StorageResource, folders []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, binding := range runtime.Bindings(res, folders...) {
		name := runtime.BindingName(binding.Slug, binding.Kind, binding.Access, binding.Folder)
		pvc, err := b.client.CoreV1().PersistentVolumeClaims(b.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading claim %s: %w", name, err)
		}
		if pvc.Spec.VolumeName == "" {
			continue
		}
		pv, err := b.client.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading volume %s: %w", pvc.Spec.VolumeName, err)
		}
		if pv.Spec.CSI == nil {
			return nil, fmt.Errorf("volume %s is not a CSI volume, so nothing says which file system it is on", pv.Name)
		}
		fs, _, _ := strings.Cut(pv.Spec.CSI.VolumeHandle, "::")
		if fs != "" && !seen[fs] {
			seen[fs] = true
			out = append(out, fs)
		}
	}
	return out, nil
}

// storageClassFor resolves the class one resource is provisioned from.
//
// The resource wins when it names one, because that is what putting the class
// on the resource was for. Otherwise the backing's default answers, which is
// what every row created before the column existed relies on.
func (b *Backend) storageClassFor(res runtime.StorageResource) string {
	if res.StorageClass != "" {
		return res.StorageClass
	}
	if res.Backing == runtime.BackingS3Files {
		return b.cfg.S3FilesStorageClass
	}
	// EFS and Azure Files share KUBE_STORAGE_CLASS deliberately. The class's
	// provisioner decides which the form *offers* (classBackings), but a
	// resource still states its backing, so the two are the same code path here
	// and an install offering both names the second class on the resource.
	return b.cfg.StorageClass
}

// --- Volumes ---

// ensureVolumeClaim creates the dynamically provisioned claim for a volume.
//
// The StorageClass an operator configures carries the provisioning mode and the
// filesystem id, so one claim is one access point: a directory jail the workload
// cannot escape, and the reason a file store needs no per-app permission scheme
// on top. That is true of both backings — an S3 Files access point carries a
// root directory the same way an EFS one does — which is why this function is
// the same function for both and only the class differs.
func (b *Backend) ensureVolumeClaim(ctx context.Context, res runtime.StorageResource) error {
	class := b.storageClassFor(res)
	if class == "" {
		return errors.New(missingClassFor(res.Backing))
	}

	name := runtime.BindingName(res.Slug, res.Kind, res.Access, res.Folder)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: b.cfg.Namespace,
			Labels: b.storageLabels(res),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			// Many pods, one filesystem: an app's service pods and its job pods
			// mount the same claim, which is what makes sharing between them
			// need no additional concept.
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize},
			},
		},
	}

	_, err := b.client.CoreV1().PersistentVolumeClaims(b.cfg.Namespace).
		Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("kube: creating claim %s: %w", name, err)
		}
		live, err := b.refuseTerminatingClaim(ctx, name)
		if err != nil {
			return err
		}
		if live != nil && b.claimIsStoreBinding(live, name) {
			return fmt.Errorf("kube: claim %s is a store's binding, not this volume's — another "+
				"storage resource holds it; give one of the two another name", name)
		}
	}
	return nil
}

// claimIsStoreBinding reports whether a live claim is what ensureStoreBinding
// writes: bound by name to the store's own <namespace>-lpv-… volume, or with
// the empty class only a static binding carries. A volume's claim names a
// class and binds to the provisioner's pvc-<uid>. The class is not compared
// for equality: a drive re-point legitimately moves a live volume's class.
//
// **Why it is asked at all**: a volume's claim is lpv-<slug> and a store's
// lpv-<slug>-<level>, in one namespace, so volume reports-full and store
// reports name one claim. The create refuses that pair now
// (runtime.BindingNamesClash); an install that already made one is kept from
// adopting or deleting the other's claim by this.
func (b *Backend) claimIsStoreBinding(pvc *corev1.PersistentVolumeClaim, name string) bool {
	if pvc.Spec.VolumeName == b.cfg.Namespace+"-"+name {
		return true
	}
	return pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == ""
}

// deleteClaimIfKind releases a claim only when it is the kind its caller
// expects — a store binding for a store, anything else for a volume — so a
// delete of one never takes the other's claim of the same name. NotFound is
// done; a claim the platform cannot read is deleted as before (cannotRead).
func (b *Backend) deleteClaimIfKind(ctx context.Context, name string, store bool) error {
	pvc, err := b.client.CoreV1().PersistentVolumeClaims(b.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil && !cannotRead(err):
		return fmt.Errorf("kube: reading claim %s: %w", name, err)
	case err == nil && b.claimIsStoreBinding(pvc, name) != store:
		log.Printf("kube: claim %s belongs to another storage resource of the same name, so it is kept", name)
		return nil
	}
	return b.deleteClaim(ctx, name)
}

// cannotRead is the platform being unable to *ask* the question, which is not
// an answer to it.
//
// **A binary newer than the RBAC it is running under is the case this exists
// for, and it has happened.** The two guards above were shipped reading the
// object back while both Role manifests granted `create` and `delete` and no
// `get`, so every re-provision — including the ordinary live-claim one the
// AlreadyExists swallow was written for — failed with a forbidden, and the
// resource row survived with no binding: the same unbacked state by a different
// road. A chart or a manifest is upgraded by an operator on their own schedule,
// so a binary that *requires* a permission it did not use yesterday is a binary
// that breaks on upgrade.
//
// So an unanswerable question keeps the old behaviour and says nothing new: the
// terminating case goes back to being silent, which is what it was, rather than
// taking working installs down with it. Only a definite *this is terminating*
// refuses — the same direction runInputKey takes when its store cannot say.
//
// Logged rather than swallowed, because an operator whose RBAC is behind should
// be able to find out why the guard is not guarding.
func cannotRead(err error) bool {
	if !apierrors.IsForbidden(err) && !apierrors.IsUnauthorized(err) {
		return false
	}
	log.Printf("kube: cannot read back a storage object to tell a live name from one being "+
		"deleted, so that check is skipped — the platform's Role needs `get` on "+
		"persistentvolumeclaims and persistentvolumes: %v", err)
	return true
}

// refuseTerminatingVolume is refuseTerminatingClaim one object up, for the
// statically provisioned volume a store binding needs under its claim. It
// returns the live volume it read, or nil when the platform cannot read it
// (cannotRead), so the caller can check what that volume points at.
func (b *Backend) refuseTerminatingVolume(ctx context.Context, name string) (*corev1.PersistentVolume, error) {
	pv, err := b.client.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("kube: volume %s was released while it was being created — try again", name)
		}
		if cannotRead(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("kube: reading volume %s: %w", name, err)
	}
	if pv.DeletionTimestamp != nil {
		return nil, fmt.Errorf("kube: volume %s still exists but is being released, so nothing was "+
			"provisioned under that name", name)
	}
	return pv, nil
}

// storeVolumeMatches reports whether an existing store volume points where
// this binding would: the same driver, the same volume attributes (the bucket,
// or the account and container) and the mount option that carries the prefix.
//
// **A binding's name carries no bucket and no prefix**, so a store deleted and
// created again under the same name — the only way to change its bucket —
// would otherwise inherit any folder binding the old one left behind, and an
// app would mount the old bucket's data under the new store's name. Only the
// prefix option is compared, not the whole slice: the others include `region`,
// which is operator configuration and may legitimately have moved.
func (b *Backend) storeVolumeMatches(pv *corev1.PersistentVolume, res runtime.StorageResource) bool {
	csi := pv.Spec.CSI
	if csi == nil || csi.Driver != b.storeCSIDriver(res) {
		return false
	}
	want := b.storeVolumeAttributes(res)
	if len(csi.VolumeAttributes) != len(want) {
		return false
	}
	for k, v := range want {
		if got, ok := csi.VolumeAttributes[k]; !ok || got != v {
			return false
		}
	}
	prefixOption := "prefix " + StorePrefix(res)
	if res.Backing == runtime.BackingBlob {
		prefixOption = "--subdirectory=" + StorePrefix(res)
	}
	return slices.Contains(pv.Spec.MountOptions, prefixOption)
}

// refuseTerminatingClaim turns an AlreadyExists on a claim name into the two
// answers it actually carries.
//
// **A name that exists and a name that is on its way out are not the same
// thing.** Creating a claim whose name is still held by one being deleted
// returns AlreadyExists, and swallowing that as success provisioned nothing:
// when the old claim finalized it took the name with it, leaving a resource the
// platform believed was backed, with no volume anywhere and no later deploy
// repairing it — nothing asks again once the answer was success.
//
// The window is not theoretical and it is not short. A finished job pod holds
// its claim through `pvc-protection` for as long as the pod is kept (`jobTTL`,
// an hour), so deleting a storage resource and immediately re-creating it —
// exactly what a second run of the drills does — lands inside it.
//
// A live claim of the same name stays success, which is the idempotence this
// swallow was written for: provisioning twice must not fail.
// It returns that live claim (nil when the platform cannot read it), so the
// caller can ask whether it is the claim it would have written.
func (b *Backend) refuseTerminatingClaim(ctx context.Context, name string) (*corev1.PersistentVolumeClaim, error) {
	pvc, err := b.client.CoreV1().PersistentVolumeClaims(b.cfg.Namespace).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Gone between the failed create and this read: the name is free
			// again and nothing holds it, but *this* attempt still provisioned
			// nothing. Reporting it is what makes the retry happen.
			return nil, fmt.Errorf("kube: claim %s was released while it was being created — try again", name)
		}
		if cannotRead(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("kube: reading claim %s: %w", name, err)
	}
	if pvc.DeletionTimestamp != nil {
		// What is wrong, and not what to do about it: this layer does not know
		// whether its caller has just written a row that a retry would now
		// collide with. The handler that did says so.
		return nil, fmt.Errorf("kube: claim %s still exists but is being deleted, so nothing was "+
			"provisioned under that name — a finished job pod holds a claim for up to an hour "+
			"after it ends", name)
	}
	return pvc, nil
}

// missingClassFor names the setting a backing needs, so an administrator reads a
// decision rather than a malfunction. The sentences differ because the
// prerequisites do, which is one of the two things the stored backing exists to
// tell the platform.
func missingClassFor(backing string) string {
	if backing == runtime.BackingS3Files {
		return "kube: an S3 Files file store needs a StorageClass backed by an Amazon S3 Files " +
			"file system — set KUBE_S3FILES_STORAGE_CLASS to a class an operator created against " +
			"it, through the EFS CSI driver, with reclaimPolicy: Retain. The file system, its " +
			"mount targets and its bucket all pre-exist; Toft binds to them and creates none " +
			"of them"
	}
	if backing == runtime.BackingAzureFiles {
		return "kube: an Azure Files volume needs a StorageClass backed by a file share — " +
			"set KUBE_STORAGE_CLASS to a class an operator created against one, with " +
			"reclaimPolicy: Retain. It is the same variable EFS uses, because a class variable " +
			"per cloud is how a configuration surface becomes a matrix; an install offering both " +
			"names the second class on the resource itself"
	}
	return "kube: file stores need a StorageClass backed by a shared filesystem — " +
		"set KUBE_STORAGE_CLASS (the class an operator created with provisioningMode: efs-ap " +
		"and reclaimPolicy: Retain). Object stores need no such class"
}

// --- Stores ---

// ensureStoreBinding creates the static PV and its claim for one access level.
//
// The Mountpoint CSI driver has no dynamic provisioning, so the PV is written
// out by hand. Everything that decides what the mount permits lives in its
// mountOptions, which is precisely why a store gets one binding per level
// rather than one per resource: options are a property of the volume, not of
// the pod that consumes it.
func (b *Backend) ensureStoreBinding(ctx context.Context, res runtime.StorageResource) error {
	if res.Bucket == "" {
		return fmt.Errorf("kube: store %s has no bucket", res.Slug)
	}
	// An empty prefix is the whole bucket, which would make every app mapped to
	// any store in that bucket able to read every other's data. The prefix *is*
	// the containment, so its absence is refused rather than defaulted.
	if strings.TrimSpace(res.Prefix) == "" {
		return fmt.Errorf("kube: store %s has no prefix — the prefix is the isolation "+
			"boundary, so mounting a bucket root is refused", res.Slug)
	}
	// Whitespace in a blob binding's subdirectory is an argv boundary, not a
	// character: the blob driver splits mount options on spaces, so it would
	// become a second blobfuse2 flag. The API refuses it where a prefix or a
	// folder is written; this is the backstop for a row that predates that.
	if res.Backing == runtime.BackingBlob && strings.IndexFunc(StorePrefix(res), unicode.IsSpace) >= 0 {
		return fmt.Errorf("kube: store %s has whitespace in its blob subdirectory %q, which "+
			"the blob driver would split into extra mount arguments", res.Slug, StorePrefix(res))
	}

	name := runtime.BindingName(res.Slug, res.Kind, res.Access, res.Folder)
	pvName := b.storePVName(res)
	labels := b.storageLabels(res)
	empty := ""

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName, Labels: labels},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: storageSize},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			// Retain. Under Delete, releasing this binding would ask the driver
			// to remove the objects behind it — data an administrator
			// provisioned deliberately and Toft has no business destroying.
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			// Empty rather than unset: this PV is statically provisioned, and a
			// class name here would invite a provisioner to adopt it.
			StorageClassName: "",
			MountOptions:     StoreMountOptions(res, b.cfg.StoreRegion),
			// Bound to its claim by name, so a claim from somewhere else in the
			// namespace cannot land on a volume carrying another app's prefix.
			ClaimRef: &corev1.ObjectReference{
				Kind: "PersistentVolumeClaim", Namespace: b.cfg.Namespace, Name: name,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver: b.storeCSIDriver(res),
					// Unique across the cluster, and stable: the driver keys its
					// own bookkeeping on it.
					VolumeHandle:     pvName,
					VolumeAttributes: b.storeVolumeAttributes(res),
				},
			},
		},
	}
	if _, err := b.client.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("kube: creating volume %s: %w", pvName, err)
		}
		// The same trap as the claim below, one object up: a volume name still
		// held by one being released takes the name with it when it finalizes,
		// and the claim beneath then binds to nothing and stays Pending
		// forever. A live volume of the same name is the idempotent case and
		// stays success.
		existing, err := b.refuseTerminatingVolume(ctx, pvName)
		if err != nil {
			return err
		}
		// Live, but possibly someone else's: a binding left by an earlier
		// resource of this name points at that resource's bucket and prefix.
		// Released (bindings only — the reclaim policy is Retain, so no object
		// goes) and refused, so the retry creates the binding this resource
		// asked for.
		if existing != nil && !b.storeVolumeMatches(existing, res) {
			if err := b.releaseStoreBinding(ctx, name, pvName); err != nil {
				return err
			}
			return fmt.Errorf("kube: volume %s was left by an earlier storage resource of this "+
				"name and pointed at its bucket or prefix; it was released — try again", pvName)
		}
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.cfg.Namespace, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			// Empty class and an explicit volume name: this claim binds to the
			// PV above and to nothing else.
			StorageClassName: &empty,
			VolumeName:       pvName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize},
			},
		},
	}
	if _, err := b.client.CoreV1().PersistentVolumeClaims(b.cfg.Namespace).
		Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("kube: creating claim %s: %w", name, err)
		}
		live, err := b.refuseTerminatingClaim(ctx, name)
		if err != nil {
			return err
		}
		if live != nil && live.Spec.VolumeName != pvName {
			// Another resource's claim of this name — a volume called
			// <store>-<level>. The volume above is only ever this store's,
			// and nothing binds it while that claim holds the name.
			if err := b.client.CoreV1().PersistentVolumes().Delete(ctx, pvName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("kube: releasing volume %s: %w", pvName, err)
			}
			return fmt.Errorf("kube: claim %s is not this store's binding — another storage "+
				"resource holds it; give one of the two another name", name)
		}
	}
	return nil
}

// releaseStoreBinding deletes one store binding's claim and volume by name,
// NotFound being success. Bindings only: a store volume is Retain, so no
// object in the bucket goes with it.
func (b *Backend) releaseStoreBinding(ctx context.Context, claim, pvName string) error {
	if err := b.deleteClaimIfKind(ctx, claim, true); err != nil {
		return err
	}
	err := b.client.CoreV1().PersistentVolumes().Delete(ctx, pvName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: releasing volume %s: %w", pvName, err)
	}
	return nil
}

// storeCSIDriver names the driver this backing mounts through.
//
// The backing selects the whole set — driver, volume attributes and mount
// options — rather than the driver name selecting itself, because the three do
// not vary independently: a Mountpoint PV whose driver was swapped for Azure's
// would be accepted by the API server and rejected at attach time, inside a pod,
// on the first deploy that used it.
func (b *Backend) storeCSIDriver(res runtime.StorageResource) string {
	if res.Backing == runtime.BackingBlob {
		return BlobCSIDriver
	}
	return b.cfg.StoreCSIDriver
}

// storeVolumeAttributes is what the driver is told about the thing it is
// mounting. Mountpoint takes the bucket; blobfuse2 takes the account, the
// container, and the subdirectory that is this binding's containment.
func (b *Backend) storeVolumeAttributes(res runtime.StorageResource) map[string]string {
	if res.Backing != runtime.BackingBlob {
		return map[string]string{"bucketName": res.Bucket}
	}
	return map[string]string{
		"storageaccount": b.cfg.StoreAccount,
		"containerName":  res.Bucket,
		// The protocol is stated rather than left to the driver's default,
		// because blobfuse2 and the NFS 3.0 flavour mount the same bytes with
		// different semantics and only one of them is what a store means.
		"protocol": "fuse2",
	}
}

// StoreMountOptions renders what the mount will and will not permit.
//
// This is the enforcement mechanism for a store, and the reason the PRD calls
// the filesystem the teacher: Mountpoint refuses an unsupported operation
// rather than emulating it, so an app that tries to append to an object, rename
// one, or delete one it was not granted delete on fails at the first wrong call
// instead of corrupting data slowly.
// Exported because it is the ladder's oracle. The same four rungs are
// expressed a second time in internal/api, as the conditions on a presigned
// URL, and that package's test compares the two expressions to each other
// rather than each to a copied constant — which is the only way a rung added
// on one side and not the other becomes a test failure instead of a permission
// escalation nobody notices.
func StoreMountOptions(res runtime.StorageResource, region string) []string {
	if res.Backing == runtime.BackingBlob {
		return blobMountOptions(res)
	}
	prefix := StorePrefix(res)

	opts := []string{
		"prefix " + prefix,
		// The pod runs as runnerUID and cannot chown anything it mounts, so the
		// mount has to present files as that user or app code cannot read them.
		fmt.Sprintf("uid=%d", runnerUID),
		fmt.Sprintf("gid=%d", runnerUID),
		"allow-other",
	}
	if region != "" {
		opts = append(opts, "region "+region)
	}

	// The ladder. Each level adds flags to the one below; nothing above the
	// mount re-checks them, which is what makes them worth having. `full` takes
	// both at once: a step that replaced but never removed withheld a name, not
	// the contents, so it was never a boundary (migration 00074).
	switch res.Access {
	case runtime.AccessFull:
		opts = append(opts, "allow-delete", "allow-overwrite")
	case runtime.AccessWrite:
		// New objects only. An app given a dataset to add to cannot clobber
		// what is already there, which is the default worth having.
	default:
		// read: nothing is added, and the pod mounts it read-only besides.
	}
	return opts
}

// blobMountOptions renders the same intent for blobfuse2, which has **two rungs
// where Mountpoint has three**.
//
// There is no allow-delete and no allow-overwrite here, and no flag that
// permits creating a blob while forbidding the clobbering of one. So `write`
// cannot be expressed on this mount, and the answer is a refusal at attach time
// (database.StorageMountableAccessLevels) rather than an approximation: this
// function is only ever reached with `read` or `full`, and a rung that got past
// that refusal must land on the *narrower* mount rather than the wider one.
//
// The subdirectory is the containment, exactly as `prefix` is on Mountpoint.
// StorePrefix is the same function for both, so a folder-scoped binding narrows
// identically on either driver and there is one definition of what this binding
// can reach.
//
// **Verified, and the mechanism is not the one the name suggests.** blobfuse2
// composes every request as `filepath.Join(subdirectory, name)` — which
// *cleans* the path, so the driver itself would follow a `..` straight out of
// the subdirectory. What makes it a boundary is that `name` never carries one:
// the kernel resolves `..` in the VFS and never sends it to FUSE, so a traversal
// out of the mount lands on the pod's own filesystem rather than on another
// prefix. Under a real blobfuse2 against a seeded container, every request it
// made was under the subdirectory and nothing above it was readable by any
// path, symlink or encoded name.
//
// So the boundary holds for what an app can do, and the load-bearing part moves
// to *this* side: the string below must carry no `..` segment, because one in
// the configuration escapes where one from the app cannot. That is refused
// where a resource is written — the prefix and the folder both go through
// api.ValidateStorePath — and asserted here by TestStorePrefixCannotEscape.
//
// **And it must carry no whitespace**, which is the sharper of the two: the
// blob CSI driver joins these options with spaces and runs blobfuse2 with
// `strings.Split(args, " ")` — the `-o uid=…` options below rely on exactly
// that split — so a space inside the subdirectory is an argv boundary, and the
// rest of the folder becomes blobfuse2 flags, a second `--subdirectory=`
// among them. The API refuses whitespace in a blob prefix and a blob folder,
// and ensureStoreBinding refuses to write a binding that carries one.
func blobMountOptions(res runtime.StorageResource) []string {
	opts := []string{
		"--subdirectory=" + StorePrefix(res),
		// The pod runs as runnerUID and cannot chown what it mounts.
		fmt.Sprintf("-o uid=%d", runnerUID),
		fmt.Sprintf("-o gid=%d", runnerUID),
		"-o allow_other",
	}
	if res.Access != runtime.AccessFull {
		// Everything that is not `full` mounts read-only. `write` never
		// reaches here — it is refused at attach — and if it ever did, landing on read-only is the failure that costs nothing,
		// where landing on read-write would be a silent promotion to `full`.
		opts = append(opts, "-o ro")
	}
	return opts
}

// StorePrefix is what this binding mounts: the resource's prefix, and the
// folder under it when the binding is folder-scoped.
//
// **This is where "the folder is mounted" is actually true.** A folder-scoped
// mapping does not narrow a wider mount somebody else holds; it gets a binding
// whose prefix is this, so the app cannot name a sibling because the sibling is
// not in the mount — which is the sentence ADR §12a already writes about a
// resource's own prefix, applied one level down.
//
// Every segment ends in a slash, and that is the containment rather than
// tidiness: Mountpoint treats a prefix as a literal string, so "team" without
// the slash also matches "team-archive", and the same is true of a folder.
// `database.coversKey` makes the identical test on the grant side, and the two
// have to agree or a person could be signed a URL for an object no mount of
// theirs would ever show them.
func StorePrefix(res runtime.StorageResource) string {
	prefix := strings.Trim(res.Prefix, "/")
	if folder := strings.Trim(res.Folder, "/"); folder != "" {
		if prefix != "" {
			prefix += "/"
		}
		prefix += folder
	}
	return prefix + "/"
}

func (b *Backend) storePVName(res runtime.StorageResource) string {
	// Namespaced: a PersistentVolume is cluster-scoped, so two Toft
	// installs sharing a cluster would otherwise collide on the resource slug.
	return b.cfg.Namespace + "-" + runtime.BindingName(res.Slug, res.Kind, res.Access, res.Folder)
}

func (b *Backend) storageLabels(res runtime.StorageResource) map[string]string {
	return map[string]string{
		labelManagedBy: managedByValue,
		labelStorage:   objectName(res.Slug),
	}
}

// --- Attaching mounts to a pod ---

// storageVolumes turns a spec's mounts into the pod's volumes and the
// container's mount points.
//
// The two are built together, from one slice, so a pod can never carry a mount
// point with no volume behind it — which the kubelet reports as a
// CreateContainerConfigError that names neither the app nor the resource.
// storeSubPath is the folder as the kubelet takes it, and it is empty for a
// store: a store's folder is already in the binding's prefix, so applying it
// again here would mount `<prefix>/<folder>/<folder>`.
func storeSubPath(m runtime.Mount) string {
	if m.Kind == runtime.MountStore {
		return ""
	}
	return strings.Trim(m.Folder, "/")
}

func storageVolumes(mounts []runtime.Mount) ([]corev1.Volume, []corev1.VolumeMount) {
	if len(mounts) == 0 {
		return nil, nil
	}

	volumes := make([]corev1.Volume, 0, len(mounts))
	mountPoints := make([]corev1.VolumeMount, 0, len(mounts))
	for i, m := range mounts {
		// Named positionally rather than by slug: a volume name is a DNS label
		// and a resource slug is not guaranteed to survive that normalisation
		// distinctly, whereas the index always does.
		name := fmt.Sprintf("storage-%d", i)
		sub := storeSubPath(m)
		volumes = append(volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: m.Claim,
					// Read-only at the pod as well as at the volume. For a
					// volume this is the *only* thing enforcing the level, since
					// one claim serves every level; for a store it is a second
					// lock on a door already bolted by the mount options.
					//
					// Except a volume folder: the kubelet creates a missing
					// subPath directory through the volume, so a read-only
					// source would fail the container on a folder nobody has
					// written yet. The source stays read-write there and the
					// container's mount below carries the level — the bind
					// mount is all the container can address.
					ReadOnly: m.ReadOnly() && sub == "",
				},
			},
		})
		mountPoints = append(mountPoints, corev1.VolumeMount{
			Name: name, MountPath: m.Path, ReadOnly: m.ReadOnly(),
			// A store's folder is in the binding's own prefix, below the
			// driver. A volume's cannot be: its claim is dynamically
			// provisioned and the access point's root directory is the driver's
			// to choose, so the folder is a subdirectory *inside* the volume's
			// one access point and the kubelet bind-mounts it — the same tree a
			// whole-volume mapping sees. That is still a jail rather than a
			// permission — a path outside the bind mount is not addressable
			// from the container at all — but it is honestly a different
			// enforcer from the access point's own root-directory jail, and
			// the ADR says so rather than eliding it.
			SubPath: sub,
		})
	}
	return volumes, mountPoints
}
