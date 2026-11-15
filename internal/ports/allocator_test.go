package ports

import (
	"fmt"
	"net"
	"sync"
	"testing"
)

// freeRange returns [start, end] — n consecutive ports nothing is listening on.
//
// The ranges in this file used to be written down: 3100-3102, 3200-3299, 3300.
// Every one of them was a bet on what is not running on the machine the tests
// happen to be on, and the machine this product is developed on is the one
// machine guaranteed to be running deployed apps. It lost the bet — two apps
// hold 3101 and 3102, so `Allocate` correctly reported "no available ports in
// range 3100-3102" and the suite read a busy host as a broken allocator.
//
// Found rather than declared, and found with `isFree` — the same bind probe the
// allocator uses, so the window a test is handed is free by the definition the
// code under test applies. The search sits below the ephemeral range
// (`ip_local_port_range`, 32768 up on Linux) so it is not competing with the
// kernel for the ports it just checked.
func freeRange(t *testing.T, n int) (int, int) {
	t.Helper()
	for base := 21000; base+n-1 < 32000; base += n {
		free := true
		for p := base; p < base+n; p++ {
			if !isFree(p) {
				free = false
				break
			}
		}
		if free {
			return base, base + n - 1
		}
	}
	t.Skipf("no run of %d consecutive free ports between 21000 and 32000 on "+
		"this machine, so there is no window to allocate from", n)
	return 0, 0
}

func TestAllocateSkipsClaimedPorts(t *testing.T) {
	start, end := freeRange(t, 3)
	a := New(start, end, func() ([]int, error) { return []int{start, start + 1}, nil })

	port, err := a.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if port != end {
		t.Errorf("port = %d, want %d — the two the database claims are skipped",
			port, end)
	}
}

func TestAllocateNeverHandsOutTheSamePortTwice(t *testing.T) {
	// The database still reports nothing in use — the reservation set is the
	// only thing preventing a collision between two in-flight deploys.
	const n = 20
	start, end := freeRange(t, n)
	a := New(start, end, func() ([]int, error) { return nil, nil })

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		seen  = map[int]bool{}
		fails []error
	)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port, err := a.Allocate()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fails = append(fails, err)
				return
			}
			if seen[port] {
				fails = append(fails, fmt.Errorf("port %d allocated twice", port))
			}
			seen[port] = true
		}()
	}
	wg.Wait()

	for _, err := range fails {
		t.Error(err)
	}
	if len(seen) != n {
		t.Errorf("got %d distinct ports, want %d", len(seen), n)
	}
}

func TestAllocateSkipsPortsStillListening(t *testing.T) {
	// A release that has been flipped away from still holds its port until the
	// old process is stopped; the database no longer claims it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	busy := l.Addr().(*net.TCPAddr).Port

	a := New(busy, busy+1, func() ([]int, error) { return nil, nil })

	port, err := a.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if port == busy {
		t.Errorf("allocated port %d, which is still in use", busy)
	}
}

func TestReleaseMakesPortAvailableAgain(t *testing.T) {
	only, _ := freeRange(t, 1)
	a := New(only, only, func() ([]int, error) { return nil, nil })

	port, err := a.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	if _, err := a.Allocate(); err == nil {
		t.Error("expected exhaustion while the only port is reserved")
	}

	a.Release(port)

	if _, err := a.Allocate(); err != nil {
		t.Errorf("Allocate after Release: %v", err)
	}
}

func TestAllocateExhaustionIsAnError(t *testing.T) {
	a := New(3400, 3401, func() ([]int, error) { return []int{3400, 3401}, nil })

	if _, err := a.Allocate(); err == nil {
		t.Error("expected an error when every port is claimed")
	}
}

func TestAllocatePropagatesLookupErrors(t *testing.T) {
	a := New(3500, 3501, func() ([]int, error) { return nil, fmt.Errorf("db down") })

	if _, err := a.Allocate(); err == nil {
		t.Error("expected the lookup error to surface rather than a bad allocation")
	}
}
