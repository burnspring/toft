package appout

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/internal/runtime"
	"toft/internal/telemetry"
)

// sinkOver is a sink writing through h, with a clock the test moves.
func sinkOver(h slog.Handler, clock *time.Time) *Sink {
	s := New(func(r slog.Record) error { return h.Handle(context.Background(), r) }, nil)
	s.now = func() time.Time { return *clock }
	return s
}

// settle waits for the writer to drain the queue.
func settle(s *Sink) {
	for i := 0; i < 200 && len(s.queue) > 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
}

func records(t *testing.T, out string) []map[string]any {
	t.Helper()
	var rs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a line on the stream is not JSON: %v\n%.200s", err, line)
		}
		rs = append(rs, m)
	}
	return rs
}

// The one stream stays parseable. App records of 15 KiB, past the 4 KiB a
// pipe writes atomically, go through the same handler instance as
// access-shaped lines written concurrently, all to one pipe: every line read
// back must be JSON. The failure is a collector quietly discarding records.
func TestOneStreamStaysParseable(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	h := slog.NewJSONHandler(w, nil)
	now := time.Now()
	s := sinkOver(h, &now)
	s.uncapped.Store(true)

	var got strings.Builder
	read := make(chan struct{})
	go func() {
		_, _ = io.Copy(&got, bufio.NewReader(r))
		close(read)
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger := slog.New(h)
		for i := 0; i < 300; i++ {
			logger.Info("request", "kind", "access", "route", "/apps/{slug}/*", "status", 200)
		}
	}()
	big := strings.Repeat("z", 15<<10)
	for i := 0; i < 300; i++ {
		s.Line(runtime.OutputLine{AppID: "a1", Workload: "demo-1a2b3c4d", DeploymentID: "1a2b3c4d-0000",
			Stream: runtime.LogStreamStdout, Line: big})
	}
	wg.Wait()
	settle(s)
	w.Close()
	<-read

	rs := records(t, got.String())
	var apps, access int
	for _, m := range rs {
		switch m["kind"] {
		case "app":
			apps++
		case "access":
			access++
		}
	}
	if apps != 300 || access != 300 {
		t.Errorf("read %d app and %d access records, want 300 of each", apps, access)
	}
}

// The level is how much the platform says and has no hold on an app's lines.
func TestTheLevelDoesNotGateTheCopy(t *testing.T) {
	var buf strings.Builder
	h := slog.NewJSONHandler(&syncWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelError})
	now := time.Now()
	s := sinkOver(h, &now)
	s.Line(runtime.OutputLine{AppID: "a1", Workload: "demo-1a2b3c4d", DeploymentID: "1a2b3c4d", Stream: "stdout", Line: "hello"})
	settle(s)
	if !strings.Contains(buf.String(), `"msg":"hello"`) {
		t.Errorf("at level error the app line is missing: %q", buf.String())
	}
}

// An app cannot forge a platform line: a whole access-line object is the
// text of a kind=app record.
func TestAnAppCannotForgeAPlatformLine(t *testing.T) {
	var buf strings.Builder
	h := slog.NewJSONHandler(&syncWriter{w: &buf}, nil)
	now := time.Now()
	s := sinkOver(h, &now)
	forged := `{"kind":"access","status":200,"user":"admin"}`
	s.Line(runtime.OutputLine{AppID: "a1", Workload: "demo-1a2b3c4d", DeploymentID: "1a2b3c4d", Stream: "stdout", Line: forged})
	settle(s)
	rs := records(t, buf.String())
	if len(rs) != 1 || rs[0]["kind"] != "app" || rs[0]["msg"] != forged {
		t.Errorf("records = %v, want one kind=app with the object as its text", rs)
	}
	if rs[0]["slug"] != "demo" || rs[0]["workload"] != "demo-1a2b3c4d" {
		t.Errorf("slug/workload = %v/%v", rs[0]["slug"], rs[0]["workload"])
	}
}

// A flooding app is capped with an exact count, said before its next admitted
// line, while a second app's lines all get through.
func TestAFloodingAppIsCappedAndCounted(t *testing.T) {
	var buf strings.Builder
	h := slog.NewJSONHandler(&syncWriter{w: &buf}, nil)
	clock := time.Now()
	s := sinkOver(h, &clock)
	line := func(app string) {
		s.Line(runtime.OutputLine{AppID: app, Workload: "x-00000000", DeploymentID: "00000000", Stream: "stdout", Line: "l"})
	}
	for i := 0; i < capBurst+500; i++ {
		line("noisy")
	}
	for i := 0; i < 10; i++ {
		line("quiet")
	}
	if got := s.DroppedSinceStart("noisy"); got != 500 {
		t.Errorf("dropped = %d, want 500", got)
	}
	clock = clock.Add(2 * time.Second) // the bucket refills; the next line is admitted
	line("noisy")
	settle(s)

	var copied map[string]int
	copied = map[string]int{}
	var drops []map[string]any
	for _, m := range records(t, buf.String()) {
		switch m["kind"] {
		case "app":
			copied[m["app_id"].(string)]++
		case "app_dropped":
			drops = append(drops, m)
		}
	}
	if copied["quiet"] != 10 || copied["noisy"] != capBurst+1 {
		t.Errorf("copied = %v, want quiet 10 and noisy %d", copied, capBurst+1)
	}
	var total float64
	for _, d := range drops {
		total += d["dropped"].(float64)
		if d["reason"] != ReasonCap {
			t.Errorf("reason = %v", d["reason"])
		}
	}
	if total != 500 {
		t.Errorf("the drop records count %v, want 500", total)
	}
}

// The masks are the shapes Toft minted, and nothing else.
func TestMaskTakesWhatToftMinted(t *testing.T) {
	in := "TOFT_APP_TOKEN=tofta_abcdefgh12345678 key toftu_QWERTYUIOPasdf url https://b.s3.amazonaws.com/k?X-Amz-Signature=abc123&x=1 db=hunter2"
	got := Mask(in)
	for _, gone := range []string{"abcdefgh12345678", "QWERTYUIOPasdf", "abc123"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q survived the mask: %s", gone, got)
		}
	}
	if !strings.Contains(got, "hunter2") {
		t.Error("the mask reached a value it has no shape for")
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// The counter's reason attribute is exactly these constants: a reason added
// here without the inventory's bound and note is a label the budget never
// counted.
func TestTheDropReasonsAreTheCountersEnum(t *testing.T) {
	for _, spec := range telemetry.Inventory {
		if spec.Name != telemetry.MetricAppLogDropped {
			continue
		}
		for _, a := range spec.Attrs {
			if a.Key == "reason" && (a.Bound != 2 || a.Note != ReasonCap+" | "+ReasonBackpressure) {
				t.Errorf("reason attr = %+v, want the two constants", a)
			}
		}
		return
	}
	t.Fatal("toft.app.log.dropped is not in the inventory")
}
