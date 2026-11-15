package builder

import (
	"strings"
	"testing"
	"time"
)

func TestLineTimesRoundTripOneTimePerLine(t *testing.T) {
	t0 := time.UnixMilli(1_762_542_202_000)
	entries := []string{"==> Fetch", "a\nb", "done"}
	at := []time.Time{t0, t0.Add(1500 * time.Millisecond), t0.Add(4 * time.Second)}

	enc := EncodeLineTimes(entries, at)
	lt := DecodeLineTimes(enc)
	if lt == nil || lt.StartMS != t0.UnixMilli() {
		t.Fatalf("decode %q = %+v", enc, lt)
	}
	// "a\nb" is two lines of the stored text, so two times.
	text := strings.Join(entries, "\n") + "\n"
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	want := []int64{0, 1500, 1500, 4000}
	if len(lt.OffsetsMS) != len(lines) {
		t.Fatalf("%d times for %d lines", len(lt.OffsetsMS), len(lines))
	}
	for i, w := range want {
		if lt.OffsetsMS[i] != w {
			t.Errorf("line %d offset = %d, want %d", i, lt.OffsetsMS[i], w)
		}
	}
}

func TestLineTimesEmptyAndUnreadableAreAbsent(t *testing.T) {
	if EncodeLineTimes(nil, nil) != "" {
		t.Error("nothing to encode should be empty")
	}
	for _, bad := range []string{"", "v2:1:0", "v1:x:0", "v1:1:0,zz", "nonsense"} {
		if DecodeLineTimes(bad) != nil {
			t.Errorf("DecodeLineTimes(%q) should be nil", bad)
		}
	}
}

func TestLiveLogTimesFollowTheBroadcast(t *testing.T) {
	b := &Builder{
		listeners: map[string][]chan string{},
		live:      map[string][]string{"d1": {}},
		liveAt:    map[string][]time.Time{"d1": {}},
	}
	b.broadcast("d1", "one")
	b.broadcast("d1", "two\nthree")
	b.broadcast("d1", "DONE") // framing: neither a line nor a time

	backlog, at, ch := b.SubscribeLogsTimed("d1")
	defer b.UnsubscribeLogs("d1", ch)
	if len(backlog) != 2 || len(at) != 2 {
		t.Fatalf("backlog %d entries, %d times; want 2 and 2", len(backlog), len(at))
	}
	enc, ok := b.LiveLogTimes("d1")
	if !ok {
		t.Fatal("not live")
	}
	if lt := DecodeLineTimes(enc); lt == nil || len(lt.OffsetsMS) != 3 {
		t.Fatalf("live times = %q, want three lines", enc)
	}
	lines, ms := StampLines(backlog, at)
	if len(lines) != 3 || len(ms) != 3 || ms[1] != ms[2] {
		t.Fatalf("StampLines = %v %v", lines, ms)
	}
}
