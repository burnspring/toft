package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/cli/internal/ui"
)

// fakeInstall points a command at a test server instead of a real install.
//
// The credential rule's environment path is the CI path — an install named in
// the environment brings its own token and neither half falls back to the file
// — so it is also the one a test can use without writing a credentials file.
// HOME is redirected anyway, so a developer's own credentials cannot leak in.
func fakeInstall(t *testing.T, handler http.HandlerFunc) (*env, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/config")
	t.Setenv("TOFT_URL", srv.URL)
	t.Setenv("TOFT_TOKEN", "toftu_test")

	var buf bytes.Buffer
	out := ui.New(false, false, false)
	out.Stdout = &buf
	out.Stderr = &buf

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &env{out: out, ctx: ctx, cancel: cancel, dir: t.TempDir(), interrupted: make(chan struct{})}, &buf
}

// appRow is the by-slug answer, which is how --wait reads a status.
func appRow(status string) string {
	return fmt.Sprintf(`{"app":{"id":"app-1","slug":"demo","status":%q}}`, status)
}

// **A 202 is neither a failure nor a completion.** Both shapes are successes,
// and the client must not turn either into the other.
func TestRestartRendersBothShapes(t *testing.T) {
	const schedulingSentence = "The app is starting. This can take a minute in isolated mode, while the pod is scheduled and its image pulled."

	cases := []struct {
		name   string
		code   int
		body   string
		expect string
	}{
		{"completed inside the wake hold", http.StatusOK, `{"status":"restarted"}`, "restarted"},
		{"still starting past it", http.StatusAccepted,
			`{"status":"restarting","detail":"` + schedulingSentence + `"}`, "restarting"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
					fmt.Fprint(w, appRow("running"))
				case strings.HasSuffix(r.URL.Path, "/restart"):
					w.WriteHeader(tc.code)
					fmt.Fprint(w, tc.body)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			})

			err := cmdRestart(e, []string{"--app", "demo"})
			if err != nil {
				t.Fatalf("a %d is a success, got %v", tc.code, err)
			}
			if got := report(e, err); got != exitOK {
				t.Fatalf("exit %d, want 0", got)
			}
			if !strings.Contains(buf.String(), tc.expect) {
				t.Errorf("output %q does not contain the server's word %q", buf.String(), tc.expect)
			}
		})
	}

	// And the 202's sentence is the server's own, not a substitute written here.
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, `{"status":"restarting","detail":%q}`, schedulingSentence)
			return
		}
		fmt.Fprint(w, appRow("running"))
	})
	if err := cmdRestart(e, []string{"--app", "demo"}); err != nil {
		t.Fatalf("cmdRestart: %v", err)
	}
	if !strings.Contains(buf.String(), schedulingSentence) {
		t.Errorf("the server's sentence was replaced: %q", buf.String())
	}
	// And it names the flag that would have waited, so nobody has to guess.
	if !strings.Contains(buf.String(), "--wait") {
		t.Errorf("a 202 should name --wait: %q", buf.String())
	}
}

// --wait is what makes the exit code mean something. Without it a 0 says only
// that the platform accepted the restart, which is all a 202 asserts.
func TestRestartWaitPollsToTerminal(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"status":"restarting"}`)
			return
		}
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		if n < 3 {
			fmt.Fprint(w, appRow("building"))
			return
		}
		fmt.Fprint(w, appRow("running"))
	})

	if err := cmdRestart(e, []string{"--app", "demo", "--wait", "--wait-timeout", "5s"}); err != nil {
		t.Fatalf("--wait on an app that came up: %v", err)
	}
	// The line's word is --json's outcome, not the state it settled in.
	if !strings.Contains(buf.String(), "restarted") {
		t.Errorf("output %q does not say the app was restarted", buf.String())
	}
}

// **The row's status is not the restart's outcome while the start runs.** A
// running app still reads `running` while its restart is in flight, and --wait
// settled on that at once, with the old process serving the old environment.
func TestRestartWaitWaitsOutAStartInFlight(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"status":"restarting"}`)
			return
		}
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		if n < 3 {
			fmt.Fprint(w, `{"app":{"id":"app-1","slug":"demo","status":"running"},"starting":true}`)
			return
		}
		fmt.Fprint(w, appRow("crashed"))
	})

	err := cmdRestart(e, []string{"--app", "demo", "--wait", "--wait-timeout", "5s"})
	if err == nil {
		t.Fatal("--wait settled on the status the row had before the start")
	}
	if !strings.Contains(err.Error(), "crashed") {
		t.Errorf("the failure is not the start's outcome: %v", err)
	}
}

// A wait that never settles ends at the timeout with a failure, and never hangs.
func TestRestartWaitTimesOutRatherThanHanging(t *testing.T) {
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"status":"restarting"}`)
			return
		}
		fmt.Fprint(w, appRow("building"))
	})

	err := cmdRestart(e, []string{"--app", "demo", "--wait", "--wait-timeout", "150ms"})
	if err == nil {
		t.Fatal("an app that never came up exited 0")
	}
	if got := report(e, err); got != exitFailed {
		t.Errorf("exit %d, want %d", got, exitFailed)
	}
	// It names the next thing to run rather than leaving a dead end.
	if !strings.Contains(err.Error(), "toft events") {
		t.Errorf("the failure does not name `toft events`: %v", err)
	}
}

// **A crashed start is exit 1**, which is the whole point of --wait.
func TestRestartWaitFailsOnCrashed(t *testing.T) {
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"status":"restarting"}`)
			return
		}
		fmt.Fprint(w, appRow("crashed"))
	})
	err := cmdRestart(e, []string{"--app", "demo", "--wait", "--wait-timeout", "5s"})
	if err == nil {
		t.Fatal("a crashed app exited 0")
	}
	if got := report(e, err); got != exitFailed {
		t.Errorf("exit %d, want %d", got, exitFailed)
	}
}

// **`sleeping` is a success, and it is the one entry that is not obvious.**
//
// A restart writes exactly two outcomes, `running` or `crashed`, and the idle
// reaper only ever sleeps an app that is already `running`. So an app found
// sleeping inside the wait window started and was then slept for want of
// traffic. Failing the pipeline there would fail it for a feature working
// exactly as designed.
func TestRestartWaitTreatsSleepingAsUp(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"status":"restarting"}`)
			return
		}
		fmt.Fprint(w, appRow("sleeping"))
	})
	if err := cmdRestart(e, []string{"--app", "demo", "--wait", "--wait-timeout", "5s"}); err != nil {
		t.Fatalf("an app slept for being idle is an app that started: %v", err)
	}
	if !strings.Contains(buf.String(), "idle") {
		t.Errorf("output %q does not explain why it is asleep", buf.String())
	}
}

func TestStartSettled(t *testing.T) {
	cases := []struct {
		status            string
		terminal, healthy bool
	}{
		{"running", true, true},
		{"sleeping", true, true},
		{"crashed", true, false},
		{"failed", true, false},
		{"building", false, false},
		{"stopped", false, false},
		// A word from an install newer than this binary is not something to
		// rule on: keep waiting, and let the timeout be the answer.
		{"quiescing", false, false},
	}
	for _, tc := range cases {
		terminal, healthy := startSettled(tc.status)
		if terminal != tc.terminal || healthy != tc.healthy {
			t.Errorf("startSettled(%q) = (%v, %v), want (%v, %v)",
				tc.status, terminal, healthy, tc.terminal, tc.healthy)
		}
	}
}

// `toft stop` is one call and one word back.
func TestStopSaysWhatTheServerSaid(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stop") {
			if r.Method != http.MethodPost {
				t.Errorf("stop used %s", r.Method)
			}
			fmt.Fprint(w, `{"status":"stopped"}`)
			return
		}
		fmt.Fprint(w, appRow("running"))
	})
	if err := cmdStop(e, []string{"--app", "demo"}); err != nil {
		t.Fatalf("cmdStop: %v", err)
	}
	if !strings.Contains(buf.String(), "stopped") {
		t.Errorf("output %q does not say stopped", buf.String())
	}
}

// **A locked app's refusal carries the administrator's words, whole.**
func TestRestartOnALockedAppPrintsTheAdministratorsWords(t *testing.T) {
	const refusal = "Locked by an administrator: unreviewed dependency. Ask platform-team@corp."
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `{"error":%q,"code":"app_locked"}`, refusal)
			return
		}
		fmt.Fprint(w, appRow("stopped"))
	})
	err := cmdRestart(e, []string{"--app", "demo"})
	if err == nil {
		t.Fatal("a locked app was restarted")
	}
	if !strings.Contains(err.Error(), refusal) {
		t.Errorf("the refusal was reworded: %v", err)
	}
	// A locked app is 2: nothing was attempted (docs/invariants.md § The CLI).
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d", got, exitUsage)
	}
}

// --json puts the answer on stdout, which is what makes these composable.
func TestLifecycleJSONIsTheAnswer(t *testing.T) {
	for _, cmd := range []struct {
		name string
		run  func(*env, []string) error
		path string
		body string
	}{
		{"restart", cmdRestart, "/restart", `{"status":"restarted"}`},
		{"start", cmdStart, "/restart", `{"status":"restarted"}`},
		{"stop", cmdStop, "/stop", `{"status":"stopped"}`},
	} {
		t.Run(cmd.name, func(t *testing.T) {
			e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, cmd.path) {
					fmt.Fprint(w, cmd.body)
					return
				}
				fmt.Fprint(w, appRow("running"))
			})
			if err := cmd.run(e, []string{"--app", "demo", "--json"}); err != nil {
				t.Fatalf("%s --json: %v", cmd.name, err)
			}
			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("%s --json did not put JSON on stdout: %q", cmd.name, buf.String())
			}
			if got["app"] != "demo" {
				t.Errorf("%s --json does not name the app: %v", cmd.name, got)
			}
		})
	}
}

// The wait's poll interval is the shared loop's, so a slow test here would mean
// the loop grew a default nobody wanted.
func TestRestartWaitDoesNotSleepAWholeIntervalBeforeItsFirstRead(t *testing.T) {
	start := time.Now()
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			fmt.Fprint(w, `{"status":"restarted"}`)
			return
		}
		fmt.Fprint(w, appRow("running"))
	})
	if err := cmdRestart(e, []string{"--app", "demo", "--wait", "--wait-timeout", "10s"}); err != nil {
		t.Fatalf("cmdRestart --wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("an already-running app took %s to confirm", elapsed)
	}
}

// Stopping a stopped app says so, rather than the word a real stop prints.
func TestStopOnAStoppedAppSaysAlready(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stop") {
			fmt.Fprint(w, `{"status":"stopped"}`)
			return
		}
		fmt.Fprint(w, appRow("stopped"))
	})
	if err := cmdStop(e, []string{"--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "already stopped") {
		t.Errorf("printed %q", buf)
	}
}

// A `..` segment is refused before a request: exit 2, invalid_path.
func TestFetchRefusesDotSegments(t *testing.T) {
	for _, path := range []string{"/../other/api/health", "/a/./b", "/%2E%2E/x"} {
		if err := refuseDotSegments(path); err == nil {
			t.Errorf("%s was accepted", path)
		}
	}
	for _, path := range []string{"/", "/a..b/c", "/x?y=../z"} {
		if err := refuseDotSegments(path); err != nil {
			t.Errorf("%s was refused: %v", path, err)
		}
	}
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
			fmt.Fprint(w, appRow("running"))
			return
		}
		t.Errorf("a request was sent: %s", r.URL.Path)
	})
	err := cmdFetch(e, []string{"/../toft-testdrive/api/health", "--app", "demo"})
	var usage *usageError
	if !errors.As(err, &usage) || usage.code != "invalid_path" {
		t.Errorf("err = %v", err)
	}
}

// `toft start` of a stopped app says "started": the route's `action` names
// which of the two it did, and its status stays "restarted" for older clients
// (docs/bugs 2026-10-28 D20). A document's restart carries the install's note.
func TestStartSaysStartedAndANoteIsPrinted(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			fmt.Fprint(w, `{"status":"restarted","action":"start","note":"served the same pages again"}`)
			return
		}
		fmt.Fprint(w, appRow("stopped"))
	})
	if err := cmdStart(e, []string{"--app", "demo"}); err != nil {
		t.Fatalf("cmdStart: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "started") || strings.Contains(got, "restarted") ||
		!strings.Contains(got, "served the same pages again") {
		t.Errorf("output %q, want started and the note", got)
	}
}

// Under --json a static app's restart says what the line says: `outcome` is
// "unchanged", and so is `status` (drills 2026-10-29: an agent reading --json
// still saw "restarted"; BB-062: status still said it).
func TestRestartJSONCarriesTheOutcomeTheLinePrints(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			fmt.Fprint(w, `{"status":"restarted","action":"restart"}`)
			return
		}
		fmt.Fprint(w, `{"app":{"id":"app-1","slug":"demo","status":"running","framework":"static"}}`)
	})
	if err := cmdRestart(e, []string{"--app", "demo", "--json"}); err != nil {
		t.Fatalf("cmdRestart: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("not one JSON document: %q", buf.String())
	}
	// status is the outcome's word, never a contradicting "restarted" (BB-062).
	if doc["outcome"] != "unchanged" || doc["status"] != "unchanged" {
		t.Errorf("doc = %v, want outcome and status unchanged", doc)
	}
}

// A stopped static app brought back is served again, so it "started" — and an
// older install that sends no note must not have this client add the one that
// says nothing changed beside it (drills 2026-10-29 #3).
func TestStartOfAStoppedStaticAppSaysNothingAboutNothingChanging(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			fmt.Fprint(w, `{"status":"restarted","action":"start"}`)
			return
		}
		fmt.Fprint(w, `{"app":{"id":"app-1","slug":"demo","status":"stopped","framework":"static"}}`)
	})
	if err := cmdStart(e, []string{"--app", "demo"}); err != nil {
		t.Fatalf("cmdStart: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "started") || strings.Contains(got, "changed nothing") {
		t.Errorf("output %q, want started and no note that nothing changed", got)
	}
}
