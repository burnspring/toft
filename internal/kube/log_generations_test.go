package kube

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// seedRunningPod puts a pod in the fake cluster whose container reports the
// given restart count — the fact the two-generation tail's guard reads.
func seedRunningPod(t *testing.T, client *fake.Clientset, name string, restarts int32) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "toft-apps"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				RestartCount: restarts,
				State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	if _, err := client.CoreV1().Pods("toft-apps").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}
}

// A container that has never restarted still returns its log.
//
// This is the easiest defect in the two-generation design to ship, and it
// breaks *every healthy app* rather than a rare one: the API server answers a
// Previous request against a container that has never restarted with a 400, and
// a tail that propagated it would replace a working log with a failure meaning
// "this app has not crashed". A fixture that has already crashed will not catch
// it, which is the trap — the natural test app for a log feature is a broken
// one.
func TestTailOfAContainerThatNeverRestarted(t *testing.T) {
	b, client := testBackend(t)
	seedRunningPod(t, client, "demo-abc123", 0)

	lines, err := b.Logs("demo-abc123", 100)
	if err != nil {
		t.Fatalf("Logs: %v — a healthy app's log must not fail", err)
	}
	if len(lines) == 0 {
		t.Fatal("no lines for a healthy container")
	}
	for _, l := range lines {
		if strings.Contains(l.Line, "restarted here") {
			t.Errorf("a container that never restarted got a join line: %q", l.Line)
		}
	}
}

// The budget is spent newest first: when the running container already fills
// the request, no second generation is fetched at all — a chatty running
// container never has its tail cut short to make room for history.
func TestTailSpendsItsBudgetOnTheRunningContainerFirst(t *testing.T) {
	b, client := testBackend(t)
	seedRunningPod(t, client, "demo-abc123", 7)

	// One line of budget, which the current generation fills on its own.
	lines, err := b.Logs("demo-abc123", 1)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines for a budget of 1: %#v", len(lines), lines)
	}
	if strings.Contains(lines[0].Line, "restarted here") {
		t.Error("history displaced the running container's own tail")
	}
}

// The absence mapping itself is TestAbsentLogReason's, not this tier's: the
// fake clientset answers GetLogs for a pod it has never heard of, so a missing
// workload cannot be staged here. Tier two is kube_cluster_test.go.

// The kubelet's own notice inside a log stream is not a line the app wrote.
func TestTheKubeletsLogNoticeIsNotTheApps(t *testing.T) {
	if !isKubeletLogNotice(`unable to retrieve container logs for containerd://0123abcd`) {
		t.Error("the kubelet's notice was kept as an app line")
	}
	for _, line := range []string{"Traceback (most recent call last):", "listening on :8080",
		"error: unable to retrieve container logs for the dashboard"} {
		if isKubeletLogNotice(line) {
			t.Errorf("an app's line %q was taken for the kubelet's", line)
		}
	}
}
