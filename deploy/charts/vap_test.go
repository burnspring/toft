package charts

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"toft/internal/kube"
)

const vapAPI = "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy"

// **No patch on PersistentVolumes without the policy that fences it.** PVs are
// cluster-scoped, so the grant reaches every volume in the cluster; patch to
// Delete and then delete is the ability to destroy any of them. The chart may
// render the grant only where it also renders the policy that limits the
// platform to volumes claimed from its own namespace — and on a cluster without
// the admission API it renders neither, which the platform reports as data
// kept rather than failing the delete.
func TestPatchOnVolumesComesOnlyWithItsFence(t *testing.T) {
	patch := regexp.MustCompile(`resources: \[persistentvolumes\]\s+verbs: \[patch\]`)

	with := mustRender(t, "--api-versions", vapAPI)
	if !patch.MatchString(with) {
		t.Error("with the admission API, the chart does not grant patch on persistentvolumes")
	}
	for _, want := range []string{
		"kind: ValidatingAdmissionPolicy\n",
		"kind: ValidatingAdmissionPolicyBinding",
		"validationActions: [Deny]",
		"failurePolicy: Fail",
		"system:serviceaccount:default:toft",
		"oldObject.spec.claimRef.namespace == 'toft-apps'",
	} {
		if !strings.Contains(with, want) {
			t.Errorf("the fence is missing %q", want)
		}
	}

	// What the platform creates is an allow-list: a CSI volume on the drivers
	// internal/kube writes, with no secret reference. A deny of EFS alone let
	// a hostPath PV through, which a restricted pod may mount through a claim.
	docs := map[string]string{"the chart": with}
	if fixture, err := os.ReadFile("../k8s/10-rbac.yaml"); err == nil {
		docs["deploy/k8s/10-rbac.yaml"] = string(fixture)
	} else if !os.IsNotExist(err) { // absent from the public source snapshot
		t.Fatal(err)
	}
	for name, doc := range docs {
		for _, want := range []string{
			"(has(object.spec.csi) &&",
			"'" + kube.DefaultStoreCSIDriver + "'",
			"'" + kube.BlobCSIDriver + "'",
			"!has(object.spec.csi.nodePublishSecretRef)",
			"!has(object.spec.csi.nodeStageSecretRef)",
		} {
			if !strings.Contains(doc, want) {
				t.Errorf("%s: the create rule is missing %q", name, want)
			}
		}
		for _, bad := range []string{"!has(object.spec.csi) ||", "efs.csi.aws.com'"} {
			if strings.Contains(doc, bad) {
				t.Errorf("%s: the create rule still carries the deny-list form %q", name, bad)
			}
		}
	}
	if !strings.Contains(mustRender(t, "--api-versions", vapAPI, "--set", "extraEnv.KUBE_STORE_CSI_DRIVER=s3.example.com"),
		"'s3.example.com'") {
		t.Error("an operator's KUBE_STORE_CSI_DRIVER is not admitted by the create rule")
	}

	without := mustRender(t)
	if patch.MatchString(without) {
		t.Error("without the admission API, the chart still grants patch on persistentvolumes — unfenced")
	}
	if strings.Contains(without, "ValidatingAdmissionPolicy") {
		t.Error("the chart rendered a policy for an API the cluster does not have")
	}
}

// The class says a delete removes a volume's files exactly when the operator
// said the driver does — and not by default, because the driver setting it
// stands for is off by default and Toft cannot read it.
func TestTheClassAnnotationFollowsTheValue(t *testing.T) {
	const ann = `toft.dev/delete-removes-data: "true"`
	if strings.Contains(mustRender(t), ann) {
		t.Error("the EFS class claims a delete removes files by default")
	}
	if !strings.Contains(mustRender(t, "--set", "appStorage.efs.deleteRemovesData=true"), ann) {
		t.Error("appStorage.efs.deleteRemovesData=true did not annotate the class")
	}
}

// The ClusterRole holding PV create/get/delete and StorageClass get, and the
// policy beside it, used to render only with appStorage.efs.enabled — but the
// isolated backend offers object stores on every install, and S3 Files and
// extra-class volumes without EFS. Each then failed at the PV call: a store
// create answered 502, and a volume delete retried for ever.
func TestTheVolumeGrantDoesNotDependOnEFS(t *testing.T) {
	out := mustRender(t, "--set", "appStorage.efs.enabled=false", "--api-versions", vapAPI)
	for _, want := range []string{
		"name: toft-store-volumes",
		"resources: [persistentvolumes]\n    verbs: [create, get, delete]",
		"resources: [storageclasses]",
		"kind: ValidatingAdmissionPolicy\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("with efs.enabled=false the render lacks %q", want)
		}
	}
}
