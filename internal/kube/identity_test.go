package kube

import (
	"context"
	"strings"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	lpruntime "toft/internal/runtime"
)

// reviewAnswer makes the fake clientset answer TokenReviews, which it otherwise
// returns empty — the object is a *request* with a status the API server fills
// in, so there is nothing for a plain fake to reflect back.
func reviewAnswer(b *Backend, status authv1.TokenReviewStatus) {
	client := b.client.(interface {
		PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
	})
	client.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: status}, nil
	})
}

func boundStatus(ns, sa, podName, podUID string) authv1.TokenReviewStatus {
	return authv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{IdentityAudience},
		User: authv1.UserInfo{
			Username: "system:serviceaccount:" + ns + ":" + sa,
			Extra: map[string]authv1.ExtraValue{
				extraPodName: {podName},
				extraPodUID:  {podUID},
			},
		},
	}
}

// seedPod puts a pod in the namespace with the labels the platform writes.
func seedPod(t *testing.T, b *Backend, name, uid, appID, workload, runID string, managed bool) {
	t.Helper()
	labels := map[string]string{labelAppID: appID, labelWorkload: workload}
	if managed {
		labels[labelManagedBy] = managedByValue
	}
	if runID != "" {
		labels[labelRunID] = runID
	}
	_, err := b.client.CoreV1().Pods(b.cfg.Namespace).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.cfg.Namespace, UID: types.UID(uid), Labels: labels},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

// The whole point, in one test: the identity comes back from the pod's labels,
// which the platform wrote, and not from anything the caller said.
func TestAVerifiedWorkloadIsIdentifiedByItsPodsLabels(t *testing.T) {
	b, _ := testBackend(t)
	seedPod(t, b, "demo-abc123", "pod-uid-1", "app-1", "demo-abc123", "", true)
	reviewAnswer(b, boundStatus("toft-apps", WorkloadServiceAccount, "demo-abc123", "pod-uid-1"))

	id, err := b.VerifyWorkloadIdentity(context.Background(), "a-projected-token")
	if err != nil {
		t.Fatalf("a legitimate workload was refused: %v", err)
	}
	if (id != lpruntime.WorkloadIdentity{AppID: "app-1", WorkloadName: "demo-abc123"}) {
		t.Errorf("identity = %+v", id)
	}
}

// A workload whose name is not a legal label value — every first-party guide,
// whose slug begins with `_` — is still identified by the name the platform
// gave it, not by the sanitised copy the label holds. The runner presents the
// name from its environment, and the bootstrap replay guard compares the two.
func TestAWorkloadIsIdentifiedByTheNameItWasGivenNotTheLabel(t *testing.T) {
	b, _ := testBackend(t)
	seedPod(t, b, "user-guide-abc123", "pod-uid-3", "app-1", "user-guide-abc123", "", true)
	pod, _ := b.client.CoreV1().Pods(b.cfg.Namespace).Get(context.Background(), "user-guide-abc123", metav1.GetOptions{})
	pod.Annotations = map[string]string{annotationWorkload: "_user-guide-abc123"}
	if _, err := b.client.CoreV1().Pods(b.cfg.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	reviewAnswer(b, boundStatus("toft-apps", WorkloadServiceAccount, "user-guide-abc123", "pod-uid-3"))

	id, err := b.VerifyWorkloadIdentity(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if id.WorkloadName != "_user-guide-abc123" {
		t.Errorf("workload = %q, want the name the platform gave, not the label", id.WorkloadName)
	}
}

// A job's pod carries its run, so the bootstrap response can be the job-shaped
// one without the caller having to say which run it is.
func TestAJobPodCarriesItsRun(t *testing.T) {
	b, _ := testBackend(t)
	seedPod(t, b, "job-demo", "pod-uid-2", "app-1", "demo-abc123", "run-7", true)
	reviewAnswer(b, boundStatus("toft-apps", WorkloadServiceAccount, "job-demo", "pod-uid-2"))

	id, err := b.VerifyWorkloadIdentity(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if id.RunID != "run-7" {
		t.Errorf("run id = %q, want the pod's label", id.RunID)
	}
}

// The four checks, each refused for its own reason. Every one of these is a
// caller the cluster would happily authenticate.
func TestIdentityVerificationRefusals(t *testing.T) {
	cases := []struct {
		name   string
		status authv1.TokenReviewStatus
		// pod seeds a pod under this name and UID; empty name seeds none.
		podName, podUID string
		managed         bool
		want            string
	}{
		{
			name:   "the cluster did not authenticate it",
			status: authv1.TokenReviewStatus{Authenticated: false, Error: "expired"},
			want:   "refused",
		},
		{
			name:   "another account in the namespace",
			status: boundStatus("toft-apps", "some-other-account", "demo-abc123", "pod-uid-1"),
			want:   "not this install's workload account",
		},
		{
			name: "authentic but not bound to a pod",
			status: authv1.TokenReviewStatus{
				Authenticated: true,
				User:          authv1.UserInfo{Username: "system:serviceaccount:toft-apps:" + WorkloadServiceAccount},
			},
			want: "not bound to a pod",
		},
		{
			name:    "the name was reused by a different pod",
			status:  boundStatus("toft-apps", WorkloadServiceAccount, "demo-abc123", "pod-uid-1"),
			podName: "demo-abc123", podUID: "a-completely-different-uid", managed: true,
			want: "not the pod this token was issued to",
		},
		{
			name:    "a pod Toft does not manage",
			status:  boundStatus("toft-apps", WorkloadServiceAccount, "demo-abc123", "pod-uid-1"),
			podName: "demo-abc123", podUID: "pod-uid-1", managed: false,
			want: "not managed by Toft",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := testBackend(t)
			if tc.podName != "" {
				seedPod(t, b, tc.podName, tc.podUID, "app-1", "demo-abc123", "", tc.managed)
			}
			reviewAnswer(b, tc.status)

			_, err := b.VerifyWorkloadIdentity(context.Background(), "t")
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refused with %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// A token from another app's pod names that pod, and that pod's labels name
// that app — so there is no way to ask as somebody else. This is the property
// one shared workload ServiceAccount rests on entirely.
func TestOnePodsTokenCannotSpeakForAnother(t *testing.T) {
	b, _ := testBackend(t)
	seedPod(t, b, "other-xyz789", "pod-uid-9", "app-2", "other-xyz789", "", true)
	reviewAnswer(b, boundStatus("toft-apps", WorkloadServiceAccount, "other-xyz789", "pod-uid-9"))

	id, err := b.VerifyWorkloadIdentity(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if id.AppID != "app-2" {
		t.Fatalf("identity = %+v — a pod spoke for an app that is not the one it belongs to", id)
	}
}

// The audience the platform asks about is the one that scopes the token; asking
// with a different one would accept a token the API server itself accepts.
func TestVerificationAsksForItsOwnAudience(t *testing.T) {
	b, _ := testBackend(t)
	seedPod(t, b, "demo-abc123", "pod-uid-1", "app-1", "demo-abc123", "", true)

	var asked []string
	b.client.(interface {
		PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
	}).PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		review := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		asked = review.Spec.Audiences
		return true, &authv1.TokenReview{
			Status: boundStatus("toft-apps", WorkloadServiceAccount, "demo-abc123", "pod-uid-1"),
		}, nil
	})

	if _, err := b.VerifyWorkloadIdentity(context.Background(), "t"); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != IdentityAudience {
		t.Errorf("reviewed for audiences %v, want only %q", asked, IdentityAudience)
	}
}

func TestAnEmptyTokenIsRefusedWithoutAskingTheCluster(t *testing.T) {
	b, _ := testBackend(t)
	if _, err := b.VerifyWorkloadIdentity(context.Background(), ""); err == nil {
		t.Fatal("an empty credential was accepted")
	}
}
