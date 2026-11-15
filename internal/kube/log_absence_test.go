package kube

import (
	"errors"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"toft/internal/runtime"
)

// The API server's refusals, read as what they mean rather than as failures.
//
// The case that matters most is the one that looks least interesting: a
// BadRequest is what asking for a not-yet-running container's log returns, and
// treating it as a server error turns every app that is pulling its image into
// a 500 on a tab the owner is watching precisely because the app has not come
// up yet.
func TestAbsentLogReason(t *testing.T) {
	podGR := schema.GroupResource{Resource: "pods"}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "pod does not exist",
			err:  apierrors.NewNotFound(podGR, "toft-app-abc"),
			want: runtime.LogAbsentNoWorkload,
		},
		{
			name: "container is not running yet",
			err:  apierrors.NewBadRequest("container \"app\" in pod \"x\" is waiting to start: ContainerCreating"),
			want: runtime.LogAbsentLaunching,
		},
		{
			// Everything else is a genuine failure to reach the cluster and
			// must keep its 500. An absence swallowing this would report an
			// unreachable API server as a quiet app.
			name: "api server is unreachable",
			err:  errors.New("dial tcp 10.0.0.1:6443: connect: connection refused"),
			want: "",
		},
		{
			name: "forbidden",
			err: apierrors.NewForbidden(podGR, "toft-app-abc",
				errors.New("pods/log is not granted")),
			want: "",
		},
		{
			name: "server timeout",
			err: &apierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusGatewayTimeout,
				Reason: metav1.StatusReasonTimeout,
			}},
			want: "",
		},
		{
			name: "no error at all",
			err:  nil,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := absentLogReason(tc.err); got != tc.want {
				t.Errorf("absentLogReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
