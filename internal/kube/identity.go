package kube

import (
	"context"
	"fmt"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"toft/internal/runtime"
)

// The extras a bound ServiceAccount token carries. Their presence is what
// distinguishes a token kubelet projected into one pod from a free-floating
// ServiceAccount token somebody minted with `kubectl create token` — and with
// one workload account shared by every app, that binding is the only thing that
// tells one app's pod from another's.
const (
	extraPodName = "authentication.kubernetes.io/pod-name"
	extraPodUID  = "authentication.kubernetes.io/pod-uid"
)

// identityTimeout bounds the two API calls a verification makes. Short: this is
// on the path of a workload starting up, and a cluster that cannot answer in
// this long has a problem the runner should be told about rather than wait on.
const identityTimeout = 10 * time.Second

// VerifyWorkloadIdentity asks the cluster who a caller is, and returns the app
// and workload the platform itself labelled that pod with.
//
// This is the inversion the whole design turns on: the platform used to *issue*
// the credential a workload presented, and now it verifies one the
// infrastructure issued. Nothing here trusts a claim travelling through the
// workload — the app id is read from a label the platform wrote on a pod whose
// identity the API server has just confirmed, which is a stronger position than
// a signed assertion the workload carried.
//
// Four checks, and each one is load-bearing:
//
//  1. The API server says the token authenticates for this audience.
//  2. The username is exactly this install's workload ServiceAccount. Any other
//     account in the namespace — or any user — is refused by name.
//  3. The token is *bound to a pod*. Without this, anyone who could mint a
//     token for the workload account could speak as every app at once.
//  4. That pod exists, is one of ours, and its UID matches the one in the
//     review — so a pod name reused after a deletion cannot be replayed.
func (b *Backend) VerifyWorkloadIdentity(ctx context.Context, token string) (runtime.WorkloadIdentity, error) {
	var none runtime.WorkloadIdentity
	if token == "" {
		return none, fmt.Errorf("kube: no identity token presented")
	}

	ctx, cancel := context.WithTimeout(ctx, identityTimeout)
	defer cancel()

	review, err := b.client.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token: token,
			// Scoped, and the scoping is half the design: a token bearing this
			// audience is refused by the API server for its own, so a workload
			// that can read the file still holds no credential against the
			// cluster.
			Audiences: []string{IdentityAudience},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return none, fmt.Errorf("kube: reviewing workload identity: %w", err)
	}

	if !review.Status.Authenticated {
		reason := review.Status.Error
		if reason == "" {
			reason = "the cluster did not authenticate this token"
		}
		return none, fmt.Errorf("kube: workload identity refused: %s", reason)
	}

	want := "system:serviceaccount:" + b.cfg.Namespace + ":" + WorkloadServiceAccount
	if review.Status.User.Username != want {
		return none, fmt.Errorf("kube: %q is not this install's workload account", review.Status.User.Username)
	}

	podName := firstExtra(review.Status.User.Extra, extraPodName)
	podUID := firstExtra(review.Status.User.Extra, extraPodUID)
	if podName == "" || podUID == "" {
		return none, fmt.Errorf("kube: this token is not bound to a pod — a workload identity must name the pod it was issued to")
	}

	pod, err := b.client.CoreV1().Pods(b.cfg.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return none, fmt.Errorf("kube: reading pod %s: %w", podName, err)
	}
	if string(pod.UID) != podUID {
		// The name was reused. The token belongs to a pod that no longer
		// exists, and the one wearing its name now is somebody else's.
		return none, fmt.Errorf("kube: pod %s is not the pod this token was issued to", podName)
	}
	if pod.Labels[labelManagedBy] != managedByValue {
		return none, fmt.Errorf("kube: pod %s is not managed by Toft", podName)
	}

	appID := pod.Labels[labelAppID]
	if appID == "" {
		return none, fmt.Errorf("kube: pod %s carries no app label", podName)
	}
	return runtime.WorkloadIdentity{
		AppID:        appID,
		WorkloadName: workloadNameOf(pod),
		RunID:        pod.Labels[labelRunID],
	}, nil
}

func firstExtra(extra map[string]authv1.ExtraValue, key string) string {
	if v, ok := extra[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}
