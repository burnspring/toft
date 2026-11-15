package builder

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/runtime"
)

// startCounter counts the starts each workload was asked for.
type startCounter struct {
	*runtime.MockRuntime
	mu     sync.Mutex
	starts map[string]int
	// at is a listener per workload, so readiness answers and no two apps
	// share an address.
	at map[string]runtime.Endpoint
}

func (c *startCounter) Deploy(spec runtime.WorkloadSpec) (runtime.Endpoint, error) {
	c.mu.Lock()
	c.starts[spec.Name]++
	c.mu.Unlock()
	return c.at[spec.Name], nil
}

// The boot pass starts several apps at once, and every app exactly once: more
// apps than workers, all of them running with nothing behind them, all of them
// started and marked running.
func TestReconcileStartsEveryAppOnceInParallel(t *testing.T) {
	db := dbtest.New(t)
	counter := &startCounter{MockRuntime: runtime.NewMockRuntime(nil), starts: map[string]int{},
		at: map[string]runtime.Endpoint{}}
	appsDir := t.TempDir()
	b := &Builder{db: db, rts: runtime.NewRuntimes(counter, nil), opts: Options{AppsDir: appsDir}}

	n := reconcileParallelism*2 + 1
	var ids []string
	for i := 0; i < n; i++ {
		app := mkApp(t, db, fmt.Sprintf("boot%d", i), string(runtime.ModeShared))
		app.CurrentRelease = fmt.Sprintf("boot%d-r1", i)
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		port, _ := strconv.Atoi(u.Port())
		counter.at[app.CurrentRelease] = runtime.Endpoint{Host: u.Hostname(), Port: port}
		app.Status = database.AppStatusRunning
		if err := db.UpdateApp(app); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateAppStatus(app.ID, database.AppStatusRunning); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, app.ID)
		if err := os.MkdirAll(ReleaseDir(appsDir, app.ID, "r1"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := linkCurrent(appsDir, app.ID, "r1"); err != nil {
			t.Fatal(err)
		}
	}

	b.Reconcile()

	if len(counter.starts) != n {
		t.Errorf("%d workloads started, want %d", len(counter.starts), n)
	}
	for name, k := range counter.starts {
		if k != 1 {
			t.Errorf("%s started %d times", name, k)
		}
	}
	for _, id := range ids {
		got, _ := db.GetApp(id)
		if got == nil || got.Status != database.AppStatusRunning {
			t.Errorf("%s not marked running", id)
		}
	}
}
