//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"toft/internal/runner"
)

// The agent runs `<its binary> runner`. Here its binary is this test binary,
// and TestMain stands in for the runner when the description says so — which
// lets every test drive a real process through a real socket.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ShimCommand {
		if err := ShimMain(); err != nil {
			fmt.Fprintln(os.Stderr, "shim:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if role := os.Getenv("AGENT_TEST_ROLE"); role == "agent" {
		// An agent of its own process, for the tests that need one to die.
		// Kept in the environment: a re-exec runs this binary again, and it
		// must come back as the agent. A runner's environment is its spec's.
		os.WriteFile(filepath.Join(os.Args[3], "agent.pid"), []byte(fmt.Sprint(os.Getpid())), 0o600)
		if err := Main(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	} else if role != "" {
		os.Exit(fakeRunner(role))
	}
	os.Exit(m.Run())
}

// fakeRunner behaves as a runner would, as far as the agent can tell.
func fakeRunner(role string) int {
	signal.Ignore(syscall.SIGPIPE)
	say := func(stream, line string) {
		w := os.Stdout
		if stream == "stderr" {
			w = os.Stderr
		}
		fmt.Fprintln(w, runner.LogMarker(time.Now(), runner.LogSourceApp, stream)+line)
	}
	tok, _ := os.ReadFile(os.Getenv(runner.EnvIdentityFile))
	say("stdout", "started token="+strings.TrimSpace(string(tok)))
	switch {
	case role == "serve":
		fmt.Fprintln(os.Stderr, "an unmarked line")
		if port := os.Getenv("FAKE_PORT"); port != "" {
			ln, err := net.Listen("tcp", "127.0.0.1:"+port)
			if err == nil {
				go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
			}
		}
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM)
		<-sigs
		say("stdout", "stopping")
		return 0
	case role == "ns-serve":
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", WorkloadPort))
		if err != nil {
			say("stderr", "listen: "+err.Error())
			return 3
		}
		go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("b")) }))
		select {}
	case role == "ns-resolve":
		// The pure-Go resolver reads /etc/resolv.conf as any program does.
		r := &net.Resolver{PreferGo: true}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		addrs, err := r.LookupHost(ctx, os.Getenv("PROBE_NAME"))
		conf, _ := os.ReadFile("/etc/resolv.conf")
		say("stdout", fmt.Sprintf("resolved=%v err=%v conf=%q", addrs, err, conf))
		return 0
	case role == "ns-probe":
		dial := func(addr string) string {
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				return "refused"
			}
			c.Close()
			return "ok"
		}
		var apps []string
		entries, _ := os.ReadDir(os.Getenv("PROBE_APPS"))
		for _, e := range entries {
			apps = append(apps, e.Name())
		}
		tmp, _ := os.ReadDir("/tmp")
		tmpState := "empty"
		if len(tmp) > 0 {
			tmpState = fmt.Sprintf("%d entries", len(tmp))
		}
		ext := "skipped"
		if e := os.Getenv("TOFT_TEST_EXTERNAL"); e != "" {
			ext = dial(e)
		}
		// Every IPv6 address in the namespace off loopback: none, or the
		// agent's IPv4 gate has a way round it.
		v6 := 0
		if raw, err := os.ReadFile("/proc/net/if_inet6"); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if f := strings.Fields(line); len(f) == 6 && f[5] != "lo" {
					v6++
				}
			}
		}
		say("stdout", fmt.Sprintf("peer=%s gateway=%s metadata=%s external=%s apps=%s pid1=%v tmp=%s ipv6=%d",
			dial(os.Getenv("PROBE_PEER")), dial(os.Getenv("PROBE_GATEWAY")), dial("169.254.169.254:80"), ext,
			strings.Join(apps, ","), os.Getpid() == 1, tmpState, v6))
		return 0
	case strings.HasPrefix(role, "sleep-exit:"):
		var code int
		fmt.Sscan(strings.TrimPrefix(role, "sleep-exit:"), &code)
		time.Sleep(1500 * time.Millisecond)
		return code
	case role == "last-words":
		say("stderr", "the run's last line")
		return 1
	case strings.HasPrefix(role, "exit:"):
		var code int
		fmt.Sscan(strings.TrimPrefix(role, "exit:"), &code)
		return code
	case role == "segv":
		// Go's runtime catches a SIGSEGV it is sent and exits 2; a shell, in
		// this same process, dies of it.
		syscall.Exec("/bin/sh", []string{"sh", "-c", "kill -SEGV $$"}, os.Environ())
		return 0
	case strings.HasPrefix(role, "flood:"):
		var n int
		fmt.Sscan(strings.TrimPrefix(role, "flood:"), &n)
		for i := 0; i < n; i++ {
			say("stdout", fmt.Sprintf("line %05d %s", i, strings.Repeat("x", 100)))
		}
		select {}
	case strings.HasPrefix(role, "alloc:"):
		var mb int
		fmt.Sscan(strings.TrimPrefix(role, "alloc:"), &mb)
		// A child holding the memory, as an app under a runner would.
		cmd := os.Args[0]
		p, _ := os.StartProcess(cmd, []string{cmd}, &os.ProcAttr{
			Env:   withRole("hold:" + fmt.Sprint(mb)),
			Files: []*os.File{nil, os.Stdout, os.Stderr},
		})
		p.Wait()
		return 1
	case role == "escape":
		// A daemon: a child in a session of its own, out of the group the
		// agent signals, then the runner serving until it is stopped.
		cmd := os.Args[0]
		p, err := os.StartProcess(cmd, []string{cmd}, &os.ProcAttr{
			Env:   withRole("hold:0"),
			Files: []*os.File{nil, nil, nil},
			Sys:   &syscall.SysProcAttr{Setsid: true},
		})
		if err != nil {
			say("stderr", "escape: "+err.Error())
			return 3
		}
		os.WriteFile(os.Getenv("ESCAPE_PID_FILE"), []byte(fmt.Sprint(p.Pid)), 0o600)
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM)
		<-sigs
		return 0
	case strings.HasPrefix(role, "hold:"):
		var mb int
		fmt.Sscan(strings.TrimPrefix(role, "hold:"), &mb)
		buf := make([]byte, mb<<20)
		for i := range buf {
			buf[i] = byte(i)
		}
		select {}
	}
	return 2
}

// withRole is this process's environment with the fake runner's role replaced.
// Appended instead, the child read the first copy — its parent's role — and
// an escape role started escapes until somebody killed the chain.
func withRole(role string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "AGENT_TEST_ROLE=") {
			env = append(env, kv)
		}
	}
	return append(env, "AGENT_TEST_ROLE="+role)
}

type fixture struct {
	t   *testing.T
	a   *Agent
	c   *Client
	dir string
	log string
}

func newFixture(t *testing.T, mutate ...func(*Config)) *fixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := shortTempDir(t)
	cfg := Config{Dir: filepath.Join(dir, "agent"), Exe: exe, Args: []string{},
		StopGrace: 2 * time.Second, WatchInterval: 50 * time.Millisecond,
		Backoff:      Backoff{First: 0, Base: 100 * time.Millisecond, Cap: 400 * time.Millisecond, Reset: time.Minute},
		CgroupParent: os.Getenv("TOFT_TEST_CGROUP_PARENT")}
	for _, m := range mutate {
		m(&cfg)
	}
	f := &fixture{t: t, dir: dir, log: filepath.Join(dir, "logs")}
	if err := os.MkdirAll(f.log, 0o700); err != nil {
		t.Fatal(err)
	}
	f.serve(cfg)
	return f
}

func (f *fixture) serve(cfg Config) {
	f.t.Helper()
	a, err := New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { a.Serve(ctx); close(served) }()
	f.a = a
	f.c = NewClient(cfg.Dir)
	f.t.Cleanup(func() {
		cancel()
		<-served
		a.Close(true)
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := f.c.List(context.Background()); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatal("the agent never answered")
}

func (f *fixture) spec(name, role string) Spec {
	return Spec{
		Name: name, AppID: "app-" + name, Kind: KindService, UID: -1,
		Dir: f.dir, Restart: RestartAlways,
		Env:     map[string]string{"AGENT_TEST_ROLE": role, "PATH": os.Getenv("PATH")},
		LogPath: filepath.Join(f.log, name+".log"),
		Labels:  map[string]string{"deployment_id": "dep-" + name},
	}
}

func (f *fixture) start(spec Spec) Status {
	f.t.Helper()
	st, err := f.c.Start(context.Background(), spec)
	if err != nil {
		f.t.Fatalf("start %s: %v", spec.Name, err)
	}
	return st
}

// until polls cond for up to d.
func until(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (f *fixture) status(name string) Status {
	l, err := f.c.List(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	for _, s := range l.Workloads {
		if s.Name == name {
			return s
		}
	}
	return Status{}
}

func (f *fixture) token(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.a.cfg.Dir, "id", name, "token"))
	return strings.TrimSpace(string(b))
}

func TestAStartedWorkloadIsListedStoppedAndDeleted(t *testing.T) {
	f := newFixture(t)
	st := f.start(f.spec("demo-1", "serve"))
	if st.State != StateOnline || st.PID <= 0 {
		t.Fatalf("after start: %+v", st)
	}
	if st.Labels["deployment_id"] != "dep-demo-1" {
		t.Errorf("labels did not come back: %v", st.Labels)
	}
	if err := f.c.Stop(context.Background(), "demo-1"); err != nil {
		t.Fatal(err)
	}
	if got := f.status("demo-1"); got.State != StateStopped || got.PID != 0 {
		t.Errorf("after stop: %+v", got)
	}
	if !alive(st.PID, 0) && syscall.Kill(st.PID, 0) == nil {
		t.Error("the stopped process is still running")
	}
	if err := f.c.Delete(context.Background(), "demo-1"); err != nil {
		t.Fatal(err)
	}
	if got := f.status("demo-1"); got.Name != "" {
		t.Errorf("deleted workload still listed: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(f.log, "demo-1.log")); !os.IsNotExist(err) {
		t.Errorf("a delete left the log behind: %v", err)
	}
	if err := f.c.Stop(context.Background(), "demo-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stop of an unknown name = %v, want ErrNotFound", err)
	}
}

// ADR §130: identity dies with its process. The token a process was given
// verifies while it lives and never again; the restarted process's own does.
func TestIdentityDiesWithItsProcess(t *testing.T) {
	f := newFixture(t)
	st := f.start(f.spec("demo-1", "serve"))
	first := f.token("demo-1")
	if first == "" {
		t.Fatal("no identity file was written")
	}
	v, err := f.c.Verify(context.Background(), first)
	if err != nil || v.AppID != "app-demo-1" || v.Workload != "demo-1" {
		t.Fatalf("live token: %+v, %v", v, err)
	}

	syscall.Kill(st.PID, syscall.SIGKILL)
	until(t, 5*time.Second, "the restart", func() bool {
		s := f.status("demo-1")
		return s.Restarts == 1 && s.State == StateOnline && s.PID != st.PID
	})
	if _, err := f.c.Verify(context.Background(), first); !errors.Is(err, ErrNotFound) {
		t.Errorf("a dead process's token verified: %v", err)
	}
	second := f.token("demo-1")
	if second == "" || second == first {
		t.Fatalf("the restarted process was not issued a new identity (%q)", second)
	}
	if v, err := f.c.Verify(context.Background(), second); err != nil || v.Workload != "demo-1" {
		t.Errorf("the restarted process's token: %+v, %v", v, err)
	}
	if _, err := f.c.Verify(context.Background(), "made-up"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an invented token: %v", err)
	}
}

// An exit's code is the process's own; a signal death reports the signal and
// no code, never a 0.
func TestAnExitReportsItsCodeOrItsSignal(t *testing.T) {
	f := newFixture(t)
	spec := f.spec("code-1", "exit:3")
	spec.Restart, spec.Kind = RestartNever, KindRun
	f.start(spec)
	until(t, 5*time.Second, "the exit", func() bool { return f.status("code-1").State == StateExited })
	st := f.status("code-1")
	if st.ExitCode == nil || *st.ExitCode != 3 || st.ExitSignal != "" {
		t.Errorf("exit 3 reported as code %v signal %q", deref(st.ExitCode), st.ExitSignal)
	}

	spec = f.spec("segv-1", "segv")
	spec.Restart, spec.Kind = RestartNever, KindRun
	f.start(spec)
	until(t, 5*time.Second, "the signal death", func() bool { return f.status("segv-1").State == StateExited })
	st = f.status("segv-1")
	if st.ExitCode != nil || st.ExitSignal != "SIGSEGV" {
		t.Errorf("SIGSEGV reported as code %v signal %q", deref(st.ExitCode), st.ExitSignal)
	}
}

// A run's output closes when it exits, its last line in the file first: the
// platform reads that file for the run's reason and removes it, and an agent
// that held it open until somebody deleted the run kept a descriptor per
// finished run — and on NFS kept the file, as .nfs*, in a directory an app's
// delete could then not remove.
func TestAFinishedRunsLogIsWrittenAndLetGo(t *testing.T) {
	f := newFixture(t)
	spec := f.spec("words-1", "last-words")
	spec.Restart, spec.Kind = RestartNever, KindRun
	f.start(spec)
	until(t, 5*time.Second, "the exit", func() bool { return f.status("words-1").State == StateExited })

	path := filepath.Join(f.log, "words-1.log")
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "the run's last line") {
		t.Errorf("the log lacks the run's last line when it reads exited:\n%s", b)
	}
	fds, _ := os.ReadDir("/proc/self/fd")
	for _, fd := range fds {
		if target, _ := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); target == path {
			t.Errorf("the agent still holds the finished run's log open (fd %s)", fd.Name())
		}
	}
}

// A binary that cannot be executed fails the start itself, and nothing is
// recorded as started.
func TestAnUnexecutableBinaryFailsTheStart(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.Exe = "/nonexistent/toft" })
	_, err := f.c.Start(context.Background(), f.spec("demo-1", "serve"))
	if err == nil || !strings.Contains(err.Error(), "starting the runner") {
		t.Fatalf("start = %v, want the exec's own failure", err)
	}
	if got := f.status("demo-1"); got.Name != "" {
		t.Errorf("a start that did not happen is listed: %+v", got)
	}
}

// Backoff: immediate, then Base doubling to Cap — and the workload is reported
// as crash-looping while it backs off.
func TestACrashLoopBacksOffAndSaysSo(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := f.c.Follow(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	spec := f.spec("loop-1", "exit:1")
	f.start(spec)

	var starts []time.Time
	for len(starts) < 6 {
		e, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == EventStart && e.Workload == "loop-1" {
			starts = append(starts, time.Now())
		}
	}
	// Gaps after each exit: 0, 100, 200, 400, 400 ms, each plus a process's
	// own life.
	want := []time.Duration{0, 100, 200, 400, 400}
	for i, w := range want {
		gap := starts[i+1].Sub(starts[i])
		if gap < w*time.Millisecond || gap > w*time.Millisecond+700*time.Millisecond {
			t.Errorf("restart %d came %v after the last start, want about %dms", i+1, gap, w)
		}
	}
	if st := f.status("loop-1"); !st.CrashLooping {
		t.Errorf("a workload backing off is not reported as crash-looping: %+v", st)
	}
}

func TestBackoffDelays(t *testing.T) {
	got := []time.Duration{}
	for n := 0; n < 8; n++ {
		got = append(got, DefaultBackoff.delay(n))
	}
	want := []time.Duration{0, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("delay(%d) = %v, want %v", i, got[i], want[i])
		}
	}
}

// Output: every line marked in one file — the runner's marks kept, an unmarked
// line given the platform's — and followers fed.
func TestOutputIsMarkedInOneFile(t *testing.T) {
	f := newFixture(t)
	f.start(f.spec("demo-1", "serve"))
	path := filepath.Join(f.log, "demo-1.log")
	until(t, 5*time.Second, "both lines", func() bool {
		b, _ := os.ReadFile(path)
		return strings.Contains(string(b), "started token=") && strings.Contains(string(b), "an unmarked line")
	})
	b, _ := os.ReadFile(path)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		at, source, stream, text := runner.ParseLogLine(line)
		if at.IsZero() {
			t.Errorf("an unmarked line in the log: %q", line)
		}
		if text == "an unmarked line" && (source != runner.LogSourcePlatform || stream != "stderr") {
			t.Errorf("the runner's unmarked stderr read back as %s/%s", source, stream)
		}
		if strings.HasPrefix(text, "started") && source != runner.LogSourceApp {
			t.Errorf("a line the runner marked as the app's became %s's", source)
		}
	}
}

// Rotation at RotateBytes keeps one earlier generation, and a follower sees
// every line, the ones written across the rotation included.
func TestRotationKeepsOneGenerationAndFollowSeesEveryLine(t *testing.T) {
	old := RotateBytes
	RotateBytes = 64 << 10
	defer func() { RotateBytes = old }()

	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := f.c.Follow(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	const n = 3000 // ~400 KiB: six rotations
	f.start(f.spec("flood-1", "flood:3000"))
	seen := 0
	for seen < n {
		e, err := stream.Next()
		if err != nil {
			t.Fatalf("after %d lines: %v", seen, err)
		}
		if e.Type == EventLine && strings.Contains(e.Line, "line ") {
			_, _, _, text := runner.ParseLogLine(e.Line)
			if want := fmt.Sprintf("line %05d", seen); !strings.HasPrefix(text, want) {
				t.Fatalf("follower got %q, want %q next", text[:10], want)
			}
			seen++
		}
	}
	path := filepath.Join(f.log, "flood-1.log")
	for _, p := range []string{path, path + ".1"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if info.Size() > RotateBytes+16<<10 {
			t.Errorf("%s is %d bytes, past the rotation size", p, info.Size())
		}
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Error("more than one earlier generation was kept")
	}
}

// A workload over its limit is stopped, reported as OOMKilled, and restarted.
func TestTheWatchdogHoldsAMemoryLimit(t *testing.T) {
	f := newFixture(t)
	spec := f.spec("hog-1", "alloc:64")
	spec.MemoryLimitMB = 16
	f.start(spec)
	until(t, 10*time.Second, "the memory kill", func() bool {
		st := f.status("hog-1")
		return st.OOMKilled && st.Restarts >= 1
	})
}

// A stop reaches a process that left the runner's group. The group's signals
// do not, so at the cgroup tier the cgroup is what kills it, and below it the
// tree the agent read before it signalled — this fake runner, unlike the real
// one, leaves its daemon behind when it exits.
func TestAStopKillsADaemonThatLeftTheGroup(t *testing.T) {
	f := newFixture(t)
	spec := f.spec("daemon-1", "escape")
	pidFile := filepath.Join(f.dir, "escaped.pid")
	spec.Env["ESCAPE_PID_FILE"] = pidFile
	f.start(spec)
	var pid int
	until(t, 5*time.Second, "the daemon's pid", func() bool {
		b, _ := os.ReadFile(pidFile)
		_, err := fmt.Sscan(string(b), &pid)
		return err == nil && pid > 0
	})
	if err := f.c.Stop(context.Background(), "daemon-1"); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, "the daemon to die", func() bool {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		// Gone, or a zombie waiting on whoever adopted it.
		return err != nil || strings.Contains(string(b), ") Z ")
	})
}

// An agent that dies leaves its workloads running, and the next one adopts
// them: same process, same identity — and an exit it then sees, not being the
// parent, carries no code.
func TestASuccessorAgentAdoptsTheEstate(t *testing.T) {
	exe, _ := os.Executable()
	root := shortTempDir(t)
	dir := filepath.Join(root, "agent")
	logs := filepath.Join(root, "logs")
	os.MkdirAll(logs, 0o700)

	// The first agent is a process of its own, so it can die as one does.
	c, err := Ensure(context.Background(), dir, exe, append(os.Environ(), "AGENT_TEST_ROLE=agent"), testAgentArgs()...)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: root, log: logs, c: c}
	st := f.start(f.spec("demo-1", "serve"))
	until(t, 5*time.Second, "the identity file", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "id", "demo-1", "token"))
		return len(b) > 0
	})
	tok, _ := os.ReadFile(filepath.Join(dir, "id", "demo-1", "token"))

	raw, err := os.ReadFile(filepath.Join(dir, "agent.pid"))
	if err != nil {
		t.Fatal(err)
	}
	var agentPID int
	fmt.Sscan(string(raw), &agentPID)
	syscall.Kill(agentPID, syscall.SIGKILL)
	until(t, 5*time.Second, "the first agent to die", func() bool { return syscall.Kill(agentPID, 0) != nil })
	if err := syscall.Kill(st.PID, 0); err != nil {
		t.Fatalf("the workload died with the agent: %v", err)
	}

	f.serve(Config{Dir: dir, Exe: exe, Args: []string{}, StopGrace: 2 * time.Second, CgroupParent: os.Getenv("TOFT_TEST_CGROUP_PARENT"),
		Backoff: Backoff{Base: 100 * time.Millisecond, Cap: 400 * time.Millisecond, Reset: time.Minute}})
	got := f.status("demo-1")
	if got.State != StateOnline || got.PID != st.PID {
		t.Fatalf("not adopted: %+v (was pid %d)", got, st.PID)
	}
	if v, err := f.c.Verify(context.Background(), strings.TrimSpace(string(tok))); err != nil || v.Workload != "demo-1" {
		t.Errorf("the adopted process's identity: %+v, %v", v, err)
	}

	syscall.Kill(st.PID, syscall.SIGKILL)
	until(t, 5*time.Second, "the adopted process's restart", func() bool {
		s := f.status("demo-1")
		return s.Restarts == st.Restarts+1 && s.State == StateOnline
	})
	if s := f.status("demo-1"); s.ExitCode != nil {
		t.Errorf("an exit the agent was not parent to reported code %d; nil is the honest answer", *s.ExitCode)
	}
}

// A caller speaking another protocol is refused, by name.
func TestAnotherProtocolIsRefused(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest(http.MethodGet, "http://agent/workloads", nil)
	req.Header.Set(ProtocolHeader, "999")
	resp, err := f.c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pe *ProtocolError
	if err := decode(resp, nil); !errors.As(err, &pe) || pe.Agent != ProtocolVersion {
		t.Errorf("another protocol: %v", err)
	}
}

// The socket is root's alone: 0600, in a directory app accounts may pass
// through to their identity files but not list.
func TestTheSocketIsTheAgentsAlone(t *testing.T) {
	f := newFixture(t)
	info, err := os.Stat(SocketPath(f.a.cfg.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v, want 0600", info.Mode().Perm())
	}
	// Bound in a private directory and moved into place, never chmodded where
	// an app account could connect first; the directory goes with the bind.
	if left, _ := filepath.Glob(filepath.Join(f.a.cfg.Dir, ".bind*")); len(left) > 0 {
		t.Errorf("the bind directory was left behind: %v", left)
	}
	dir, _ := os.Stat(f.a.cfg.Dir)
	if dir.Mode().Perm() != 0o711 {
		t.Errorf("agent directory mode %v, want 0711", dir.Mode().Perm())
	}
}

// Two starts of one name at once leave one process under it: the second
// replaces the first, never runs beside it unheld.
func TestConcurrentStartsOfOneNameLeaveOneProcess(t *testing.T) {
	f := newFixture(t)
	pids := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			st, err := f.c.Start(context.Background(), f.spec("twin-1", "serve"))
			if err != nil {
				t.Error(err)
			}
			pids <- st.PID
		}()
	}
	a, b := <-pids, <-pids
	held := f.status("twin-1").PID
	if held != a && held != b {
		t.Fatalf("the name holds pid %d, neither start's (%d, %d)", held, a, b)
	}
	other := a
	if held == a {
		other = b
	}
	until(t, 5*time.Second, "the replaced start to be gone", func() bool {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", other))
		return err != nil || strings.Contains(string(raw), ") Z ")
	})
}

// A child that ended while nobody waited — the re-exec window — is collected
// with its real status, not taken for a process that is merely gone and left a
// zombie for the agent's life.
func TestAChildThatEndedUnwatchedIsReapedWithItsCode(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 0.3; exit 5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	ticks, _, err := procStart(pid)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reapEnded(pid, ticks); ok {
		t.Fatal("reaped a child still running")
	}
	until(t, 5*time.Second, "the child to end", func() bool {
		_, state, _ := procStart(pid)
		return state == 'Z'
	})
	exit, ok := reapEnded(pid, ticks)
	if !ok || exit.code == nil || *exit.code != 5 {
		t.Fatalf("reapEnded = %+v, %v; want code 5", exit, ok)
	}
	if _, _, err := procStart(pid); err == nil {
		t.Error("the zombie is still there")
	}
}

// Two agents never hold one directory.
func TestASecondAgentForTheSameDirectoryRefuses(t *testing.T) {
	f := newFixture(t)
	if _, err := New(f.a.cfg); err == nil || !strings.Contains(err.Error(), "another agent") {
		t.Errorf("second agent: %v", err)
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// shortTempDir is a temporary directory with a short path: a Unix socket's is
// capped at 108 bytes, and t.TempDir under a long TMPDIR and a long test name
// passes it.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// ADR §134: an agent replaced in place keeps its pid, so its workloads
// are still its children, and an exit that happens after the replacement is
// reported with its real code — the thing an agent that crashed cannot do.
func TestAReexecutedAgentKeepsItsWorkloadsAsChildren(t *testing.T) {
	exe, _ := os.Executable()
	root := shortTempDir(t)
	dir := filepath.Join(root, "agent")
	logs := filepath.Join(root, "logs")
	os.MkdirAll(logs, 0o700)
	c, err := Ensure(context.Background(), dir, exe, append(os.Environ(), "AGENT_TEST_ROLE=agent"), testAgentArgs()...)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: root, log: logs, c: c}
	raw, _ := os.ReadFile(filepath.Join(dir, "agent.pid"))
	var agentPID int
	fmt.Sscan(string(raw), &agentPID)
	t.Cleanup(func() { syscall.Kill(agentPID, syscall.SIGKILL) })

	spec := f.spec("job-1", "sleep-exit:4")
	spec.Kind, spec.Restart = KindRun, RestartNever
	st := f.start(spec)

	// The old image answers for 50 ms after the 202, so "the agent answers"
	// alone can be the old one, and the next call lands in the gap between its
	// close and the new image's bind. The new image binds a new socket — a new
	// inode at the same path — which the old one never does.
	inode := func() uint64 {
		var fi syscall.Stat_t
		if syscall.Stat(SocketPath(dir), &fi) != nil {
			return 0
		}
		return fi.Ino
	}
	before := inode()
	if err := c.Reexec(context.Background()); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, "the replaced agent to answer", func() bool {
		raw, _ := os.ReadFile(filepath.Join(dir, "agent.pid"))
		var now int
		fmt.Sscan(string(raw), &now)
		if ino := inode(); ino == 0 || ino == before {
			return false
		}
		_, err := c.List(context.Background())
		return err == nil && now == agentPID && len(raw) > 0
	})
	if got := f.status("job-1"); got.PID != st.PID {
		t.Fatalf("after the re-exec: %+v", got)
	}
	until(t, 10*time.Second, "the run to end", func() bool { return f.status("job-1").State == StateExited })
	if got := f.status("job-1"); got.ExitCode == nil || *got.ExitCode != 4 {
		t.Errorf("the run's end after a re-exec reported code %v; a re-executed agent is still the parent", deref(got.ExitCode))
	}
	log, _ := os.ReadFile(LogPath(dir))
	if !strings.Contains(string(log), "re-executed in place") {
		t.Errorf("the agent's log does not say it re-executed:\n%s", log)
	}
}

// An agent started again inside the leaf a previous one made is placed beside
// it, never inside it: a leaf per start would nest without end, and new
// workloads would land beside the adopted ones' rather than with them.
func TestASecondAgentDoesNotNestTheCgroupLeaf(t *testing.T) {
	for self, want := range map[string]string{
		"/sys/fs/cgroup/system.slice/toft.service":               "/sys/fs/cgroup/system.slice/toft.service",
		"/sys/fs/cgroup/system.slice/toft.service/toft-platform": "/sys/fs/cgroup/system.slice/toft.service",
		"/sys/fs/cgroup/toft-platform":                           "/sys/fs/cgroup",
	} {
		if got := parentFor(self); got != want {
			t.Errorf("parentFor(%q) = %q, want %q", self, got, want)
		}
	}
}

// testAgentArgs are an agent process's flags in a test: the test's own cgroup
// parent, when one is named — as root, an agent given none rearranges the
// cgroup it was started in, which in a test is the whole login session's.
func testAgentArgs() []string {
	if p := os.Getenv("TOFT_TEST_CGROUP_PARENT"); p != "" {
		return []string{"--cgroup-parent", p}
	}
	return nil
}

// A cgroup tree the agent may make and not enter is refused at start, with the
// reason, rather than taken and failing every workload's start at clone — the
// case of a user's delegated cgroup reached from a login session not under it.
// TOFT_TEST_UNENTERABLE_CGROUP names such a parent; skipped without one.
func TestAnUnenterableCgroupParentIsRefusedNotTaken(t *testing.T) {
	parent := os.Getenv("TOFT_TEST_UNENTERABLE_CGROUP")
	if parent == "" {
		t.Skip("TOFT_TEST_UNENTERABLE_CGROUP names a cgroup this process may create under and not enter")
	}
	f := newFixture(t, func(c *Config) { c.CgroupParent = parent })
	l, _ := f.c.List(context.Background())
	if l.Tier != TierProcess || !strings.Contains(l.TierReason, "may not be started") {
		t.Errorf("tier %q (%s), want the process tier and the reason", l.Tier, l.TierReason)
	}
	if st := f.start(f.spec("demo-1", "serve")); st.State != StateOnline {
		t.Errorf("a workload did not start: %+v", st)
	}
}

// The namespace tier is decided from where the platform binds, never by
// dialling it: the agent starts before the platform's listeners do, so a dial
// at that moment refused the tier on every fresh start.
func TestTheNamespaceTiersReachabilityIsTheBindAddress(t *testing.T) {
	gw := netip.MustParseAddr("10.88.0.1")
	for host, ok := range map[string]bool{"": true, "0.0.0.0": true, "::": true, "10.88.0.1": true, "127.0.0.1": false, "10.0.1.5": false} {
		n := &netConfig{gateway: gw, platformHost: host}
		if got := n.reachable() == nil; got != ok {
			t.Errorf("platform on %q: reachable = %v, want %v", host, got, ok)
		}
	}
}

// A namespace-tier workload in the platform pod never sees the pod's
// ServiceAccount token, whatever the platform's own mask list names: the
// directory Kubernetes mounts it under is always covered, beside the agent's.
func TestTheServiceAccountTokenDirectoryIsAlwaysMasked(t *testing.T) {
	for _, requested := range [][]string{nil, {"/srv/apps"}} {
		mask := namespaceMask("/var/lib/toft-agent", requested)
		has := map[string]bool{}
		for _, m := range mask {
			has[m] = true
		}
		if !has["/var/run/secrets"] || !has["/var/lib/toft-agent"] {
			t.Errorf("mask %v lacks the token directory or the agent's own", mask)
		}
		for _, r := range requested {
			if !has[r] {
				t.Errorf("mask %v dropped the requested %s", mask, r)
			}
		}
	}
}
