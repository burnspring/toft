package builder

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/deploysteps"
)

// A build that panics fails its own deploy and leaves the worker taking the
// next job: one app's content must not take the platform process down.
func TestAPanickingBuildFailsItsDeployAndTheWorkerGoesOn(t *testing.T) {
	db := dbtest.New(t)
	// No runtimes at all: resolving the backend dereferences nil, which is the
	// panic — standing in for any parser handed content nobody anticipated.
	b := &Builder{db: db, pool: 1, starts: make(chan struct{}, 1), queue: make(chan *BuildJob, 2)}
	go b.worker()
	defer close(b.queue)

	var deploys []*database.Deployment
	for _, slug := range []string{"first", "second"} {
		app := mkApp(t, db, slug, "")
		d := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusPending}
		if err := db.CreateDeployment(d, nil); err != nil {
			t.Fatalf("CreateDeployment: %v", err)
		}
		deploys = append(deploys, d)
		b.queue <- &BuildJob{App: app, Deployment: d, enqueuedAt: time.Now()}
	}

	for _, d := range deploys {
		deadline := time.Now().Add(10 * time.Second)
		for {
			got, err := db.GetDeployment(d.ID)
			if err != nil {
				t.Fatalf("GetDeployment: %v", err)
			}
			if got.Status == database.DeployStatusFailed {
				if got.FailureReason != string(deploysteps.ReasonInternal) {
					t.Errorf("deployment %s failure reason = %q, want %q", d.ID, got.FailureReason, deploysteps.ReasonInternal)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("deployment %s still %q: the worker did not survive the panic", d.ID, got.Status)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}
