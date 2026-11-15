// Package ports hands out listener ports for deployed apps.
//
// Allocation has to survive two races the database alone cannot see:
//
//   - two concurrent creates reading the same "used" set and picking the same
//     free port, and
//   - a port that a database row no longer claims but whose process is still
//     listening (the window between flipping a release and stopping the old one).
//
// A mutex closes the first, a bind probe closes the second, and reservations
// cover the gap between allocating a port and something actually listening on it.
package ports

import (
	"fmt"
	"net"
	"sync"
	"time"
)

type Allocator struct {
	mu       sync.Mutex
	start    int
	end      int
	usedFn   func() ([]int, error)
	reserved map[int]time.Time
	ttl      time.Duration
}

// New returns an allocator over [start, end]. usedFn reports ports already
// claimed by persisted apps.
func New(start, end int, usedFn func() ([]int, error)) *Allocator {
	return &Allocator{
		start:    start,
		end:      end,
		usedFn:   usedFn,
		reserved: make(map[int]time.Time),
		ttl:      30 * time.Minute,
	}
}

// Allocate reserves and returns a free port. The reservation is released by
// Release, or automatically after the TTL if a build dies without cleaning up.
func (a *Allocator) Allocate() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.expireLocked()

	used := make(map[int]bool)
	if a.usedFn != nil {
		claimed, err := a.usedFn()
		if err != nil {
			return 0, err
		}
		for _, p := range claimed {
			used[p] = true
		}
	}

	for port := a.start; port <= a.end; port++ {
		if used[port] {
			continue
		}
		if _, held := a.reserved[port]; held {
			continue
		}
		if !isFree(port) {
			continue
		}
		a.reserved[port] = time.Now()
		return port, nil
	}

	return 0, fmt.Errorf("no available ports in range %d-%d", a.start, a.end)
}

// Release drops a reservation. Safe to call for ports that were never reserved.
func (a *Allocator) Release(port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.reserved, port)
}

func (a *Allocator) expireLocked() {
	cutoff := time.Now().Add(-a.ttl)
	for port, at := range a.reserved {
		if at.Before(cutoff) {
			delete(a.reserved, port)
		}
	}
}

// isFree reports whether anything is currently listening on the loopback port.
func isFree(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}
