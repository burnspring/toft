package resource

import (
	"testing"
	"time"
)

// Throttling is differenced like CPU, across a flush and a restart, and is
// absent — never zero — where no reading carried it.
func TestThrottlingIsDifferencedAndAbsentWhereUnmeasured(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)
	th := func(p, tp int64, ts float64) *Throttling {
		return &Throttling{Periods: p, ThrottledPeriods: tp, ThrottledSeconds: ts}
	}

	r.Record(Sample{AppID: "a", WorkloadID: "w#1", Throttling: th(1000, 100, 5), ObservedAt: clk.now()})
	r.Record(Sample{AppID: "a", WorkloadID: "w#1", Throttling: th(1600, 400, 20), ObservedAt: clk.advance(10 * time.Second)})
	r.Flush()
	r.Record(Sample{AppID: "a", WorkloadID: "w#1", Throttling: th(2200, 500, 25), ObservedAt: clk.advance(10 * time.Second)})
	// A restart: the counters start again, and are taken whole.
	r.Record(Sample{AppID: "a", WorkloadID: "w#2", Throttling: th(100, 10, 1), ObservedAt: clk.advance(10 * time.Second)})
	r.Flush()
	// An app with no cgroup of its own reports none.
	r.Record(Sample{AppID: "b", WorkloadID: "p#1", CPUSeconds: 1, ObservedAt: clk.now()})
	r.Record(Sample{AppID: "b", WorkloadID: "p#1", CPUSeconds: 2, ObservedAt: clk.advance(10 * time.Second)})
	r.Flush()

	ivs := store.all()
	if len(ivs) != 3 {
		t.Fatalf("got %d intervals: %+v", len(ivs), ivs)
	}
	if got := ivs[0].Throttling; got == nil || *got != *th(600, 300, 15) {
		t.Errorf("first window = %+v, want 600 periods, 300 throttled, 15 s", got)
	}
	if got := ivs[1].Throttling; got == nil || *got != *th(700, 110, 6) {
		t.Errorf("window across a restart = %+v, want 600+100 periods, 100+10 throttled, 5+1 s", got)
	}
	if ivs[2].Throttling != nil {
		t.Errorf("an unmeasured workload reported throttling %+v", ivs[2].Throttling)
	}
}
