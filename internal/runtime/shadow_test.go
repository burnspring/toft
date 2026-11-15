package runtime

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every write to the account database is serialized, whichever caller makes it.
//
// The shadow suite's lock file fails a second writer rather than queueing it, so
// two starts at once — Remount brings four up together — were two chances at
// "cannot lock /etc/group". What is asserted is the property the lock file does
// not give: that no two shadow-suite commands are ever in flight together,
// across the account path and both storage paths.
func TestShadowSuiteWritesNeverOverlap(t *testing.T) {
	var inFlight, peak, calls atomic.Int32
	orig := runShadow
	runShadow = func(name string, args ...string) ([]byte, error) {
		calls.Add(1)
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
		return nil, nil
	}
	t.Cleanup(func() { runShadow = orig })

	iso := Isolation{PerApp: true, UserPrefix: "toft-shadow-test-", AutoCreate: true,
		Group: "toft-shadow-test-group"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _, _ = iso.lookupOrCreate("toft-shadow-test-app") }()
		go func() { defer wg.Done(); _ = systemGroupEnsure(Gate{Group: "toft-shadow-test-store", GID: 64999}) }()
		// root exists everywhere and holds no gate, so this always reaches the join.
		go func() {
			defer wg.Done()
			_ = systemSetGates(&AppAccount{Username: "root"}, []string{"toft-store-shadow-test"})
		}()
	}
	wg.Wait()

	if calls.Load() < 16 {
		t.Fatalf("only %d shadow-suite calls were made; the test is not exercising the paths it names", calls.Load())
	}
	if got := peak.Load(); got != 1 {
		t.Errorf("%d shadow-suite commands ran at once, want 1", got)
	}
}
