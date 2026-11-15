package api

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"toft/internal/appout"
	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runner"
	"toft/internal/runtime"
)

// checkWebSocketOrigin is the upgrade-time equivalent of originCheck, which does
// not cover this path: the WebSocket handshake is a GET the browser will issue
// cross-origin without preflight, and the deployed-apps gateway is same-site, so
// an app's page could otherwise open a log stream for any app as its viewer.
//
// Unlike originCheck, a missing Origin is rejected rather than allowed. Browsers
// always send Origin on a WebSocket handshake, so its absence means the caller is
// not the browser this endpoint is for.
func (s *Server) checkWebSocketOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	return s.allowedOrigins(r)[normalizeOrigin(origin)]
}

// logsAction gates every log surface. Deliberately stricter than ActionRead:
// build logs are the raw stdout of npm/uv run against the build environment, and
// runtime logs are whatever the app printed, so a viewer on a public app must not
// see them. Owners and editors only.
const logsAction = auth.ActionEditContent

// Bounds on the `lines` parameter. It reaches the backend's tail — the agent's
// output file read from its end, the pod log's tailLines — which is not
// injectable, but an unbounded value is a cheap way to make the host allocate.
const (
	defaultLogLines = 100
	maxLogLines     = 5000
)

// authorizeAppLogs resolves the app named in the request and authorizes the
// caller for its logs. It writes the response itself when it returns false, and
// distinguishes 404 from 403 the way the rest of the API does: a caller who
// cannot even read the app is told it does not exist.
func (s *Server) authorizeAppLogs(w http.ResponseWriter, r *http.Request) (*database.App, bool) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return nil, false
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	if d := auth.Authorize(p, logsAction, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, auth.CanRead(p, obj, s.db))
		return nil, false
	}
	return app, true
}

// authorizeDeploymentLogs additionally pins the deployment to the app in the
// URL. Without that check the deployment ID is an IDOR: authorization is
// evaluated against one app while the logs come from another.
func (s *Server) authorizeDeploymentLogs(w http.ResponseWriter, r *http.Request) (*database.Deployment, bool) {
	_, deploy, ok := s.authorizeDeployment(w, r)
	return deploy, ok
}

// authorizeDeployment is the same check, returning the app as well — for the
// callers that have to say which build is current, a fact that only exists
// relative to the app.
func (s *Server) authorizeDeployment(w http.ResponseWriter, r *http.Request) (*database.App, *database.Deployment, bool) {
	app, ok := s.authorizeAppLogs(w, r)
	if !ok {
		return nil, nil, false
	}

	deployID := chi.URLParam(r, "deployID")
	deploy, err := s.db.GetDeployment(deployID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, nil, false
	}
	if deploy == nil || deploy.AppID != app.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "deployment not found", "hint": "toft deployments lists this app's deployments with their ids"})
		return nil, nil, false
	}
	return app, deploy, true
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	app, ok := s.authorizeAppLogs(w, r)
	if !ok {
		return
	}

	lines := defaultLogLines
	if l := r.URL.Query().Get("lines"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			lines = n
		}
	}
	if lines < 1 {
		lines = 1
	}
	if lines > maxLogLines {
		lines = maxLogLines
	}
	// A download is the whole of what the backend holds, not the pane's tail.
	if wantsDownload(r) {
		lines = maxDownloadLines
	}
	// One voice, chosen before the tail is cut rather than after: `--lines 6
	// --source platform` cut six lines off the merged log first, and when the
	// newest six were all the app's the platform's answer was empty.
	source := r.URL.Query().Get("source")
	if source != "" && source != runtime.LogSourceApp && source != runtime.LogSourcePlatform {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `source must be "app" or "platform"`, "code": "invalid_source", "hint": "pass source as app or platform, or leave it out"})
		return
	}

	// What the platform said about this app, whether or not there is a workload
	// to ask. This is the read that makes the pointer in an app_events detail —
	// "Check the app's logs for what it printed on its way out" — land on
	// something after the workload is gone.
	stored := s.withFailedStartOutput(app, s.storedPlatformLog(app, lines))

	// Facts the platform holds are answered without asking a backend. Not an
	// optimisation: a stopped app has no workload for either backend to have an
	// opinion about, and the reason the tab needs is on the app row.
	//
	// The reason no longer empties the answer. A stopped app still has the
	// platform's account of the start that preceded the stop, and that account
	// is the whole point — so the word travels *with* the lines rather than
	// instead of them, and the client renders it only when there is nothing to
	// show. `locked` is the exception and logAbsenceReason keeps it.
	if reason, err := s.logAbsenceReason(app); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	} else if reason == runtime.LogAbsentLocked {
		// The one absence that is also a refusal. An administrator took this
		// app away; a lock that left the platform's account of it readable
		// would be a lock that took the app away and left its file open, and
		// the copy beside this word promises that nothing it printed is kept.
		s.answerLogs(w, r, app, absentLogs(reason))
		return
	} else if reason != "" {
		s.answerLogs(w, r, app, withReason(tailLogs(onlySource(mergeLogs(app, stored, nil), source), lines), reason))
		return
	}

	rt, err := s.runtimeFor(app)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "hint": `switch the app to shared with PATCH /api/v1/apps/{id} and "execution_mode": "shared", or ask the operator to give this install isolated execution`})
		return
	}
	logLines, err := rt.Logs(app.ProcessName(), lines)
	if err != nil {
		if reason := runtime.AbsenceReason(err); reason != "" {
			s.answerLogs(w, r, app, withReason(tailLogs(onlySource(mergeLogs(app, stored, nil), source), lines), reason))
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get logs"})
		return
	}

	s.answerLogs(w, r, app, tailLogs(onlySource(mergeLogs(app, stored, maskAppLines(logLines)), source), lines))
}

// maskAppLines applies the copy's mask (appout.Mask) to what either backend
// returned an app printed: one rule for the tab and the collector, on the job
// log's precedent, because a token in a browser pane is still a token on a
// screen. The files keep the raw line; only the platform reads them.
func maskAppLines(lines []runtime.LogLine) []runtime.LogLine {
	for i := range lines {
		if lines[i].Source == runtime.LogSourceApp {
			lines[i].Line = appout.Mask(lines[i].Line)
		}
	}
	return lines
}

// onlySource keeps one voice of a merged log, or all of it for "".
func onlySource(resp logsResponse, source string) logsResponse {
	if source == "" {
		return resp
	}
	kept := make([]runtime.LogLine, 0, len(resp.Lines))
	for _, l := range resp.Lines {
		if l.Source == source {
			kept = append(kept, l)
		}
	}
	resp.Lines = kept
	resp.Logs = runtime.JoinLines(kept)
	return resp
}

// tailLogs makes `lines` the size of the answer, not of each source in it.
//
// Up to three sources are asked for `lines` each — the stored platform log,
// and the backend's tail, which for a pod that restarted is two container
// generations read separately — because none of them knows how many of the
// newest lines across all of them are its own. (The agent's file and its
// rotated generation are already one tail of N.) So the merge holds up to
// 3N+1, and the newest N are kept here, after the merge has put them in order.
//
// A separator left at the top of the cut divides nothing from nothing and is
// dropped: the boundary line only means something with lines on both sides.
func tailLogs(resp logsResponse, n int) logsResponse {
	if n <= 0 || len(resp.Lines) <= n {
		return resp
	}
	kept := resp.Lines[len(resp.Lines)-n:]
	if len(kept) > 0 && isSeparator(kept[0]) {
		kept = kept[1:]
	}
	resp.Lines = append([]runtime.LogLine{}, kept...)
	resp.Logs = runtime.JoinLines(resp.Lines)
	return resp
}

// isSeparator is the boundary either merge draws between the platform's
// account and the workload's own output.
func isSeparator(l runtime.LogLine) bool {
	return l.Source == runtime.LogSourcePlatform && (l.Marker != "" || l.Line == sharedLogSeparator)
}

// sharedLogSeparator is the shared-mode boundary over an unstamped tail.
const sharedLogSeparator = "── everything below is the app's own output; it was written without timestamps ──"

// answerLogs writes the run log as the JSON read, or as a file when the caller
// asked for one.
func (s *Server) answerLogs(w http.ResponseWriter, r *http.Request, app *database.App, resp logsResponse) {
	if wantsDownload(r) {
		writeLogFile(w, r, app.Slug+"-logs.txt", runLogText(resp.Lines))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// storedPlatformLog is this app's narration, or nothing at all.
//
// A failure to read it is not a failure to answer: the backend's tail is what
// this route has always returned, and losing the platform's half of a log is
// not a reason to lose the app's half too. It is one indexed query on a
// request that already reads the agent's file or calls the API server, and `toft logs
// -f` polls this route once a second, so it is deliberately the cheapest thing
// here.
func (s *Server) storedPlatformLog(app *database.App, budget int) []runtime.LogLine {
	if s.platformLog == nil || s.db == nil {
		return nil
	}
	rows, err := s.db.PlatformLogTail(app.ID, budget)
	if err != nil {
		log.Printf("platform log: reading %s: %v", app.Slug, err)
		return nil
	}
	out := make([]runtime.LogLine, 0, len(rows))
	for _, r := range rows {
		at := r.At
		out = append(out, runtime.LogLine{
			At:     &at,
			Source: runtime.LogSourcePlatform,
			Stream: r.Stream,
			Line:   r.Line,
		})
	}
	return out
}

// failedStartOutputMarker opens the builder's tail of a workload's own output
// in a readiness failure (builder.watchWorkload): each following line indented
// four spaces.
const failedStartOutputMarker = "\n    Last lines of its log:\n"

// withFailedStartOutput adds what the app printed on a start that failed to
// the stored account, as the app's lines (BB-003).
//
// **A failed start deletes its workload, and the output goes with it** — the
// agent removes the workload's file, a pod's log dies with the pod — so after
// `start_failed · not_ready` the backend has nothing to read, and `toft logs`
// showed only "==> Starting" and "==> Started" while the traceback sat in the
// deploy log. The builder read the tail before the delete, in both seams, and
// recorded it in the deployment's failure; this puts it back where a person
// reads a crash. Only while that deployment is the app's newest: a later
// deploy is the newer story. Stamped at the deployment's end, so it sorts
// after the start's narration; masked like any app line.
func (s *Server) withFailedStartOutput(app *database.App, stored []runtime.LogLine) []runtime.LogLine {
	if s.db == nil {
		return stored
	}
	d, err := s.db.LatestDeploymentEnd(app.ID)
	if err != nil || d == nil || d.Status != database.DeployStatusFailed {
		return stored
	}
	// Each line as the workload wrote it — its own time and stream — where the
	// deployment kept them (BB-038, fourth 2026-11-03 black-box list); a
	// failure recorded before the column keeps the old reading, below.
	var kept []builder.FailedStartLine
	if d.FailureOutput != "" && json.Unmarshal([]byte(d.FailureOutput), &kept) == nil && len(kept) > 0 {
		out := append([]runtime.LogLine(nil), stored...)
		for _, l := range kept {
			stream := l.Stream
			if stream == "" {
				stream = runtime.LogStreamStderr
			}
			out = append(out, runtime.LogLine{At: l.At, Source: runtime.LogSourceApp,
				Stream: stream, Line: appout.Mask(l.Line)})
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].At == nil || out[j].At == nil {
				return false
			}
			return out[i].At.Before(*out[j].At)
		})
		return out
	}
	tail := failedStartOutput(d.FailureMessage)
	if len(tail) == 0 {
		return stored
	}
	at := time.Now().UTC()
	switch {
	case d.FinishedAt != nil:
		at = *d.FinishedAt
	case d.StartedAt != nil:
		at = *d.StartedAt
	}
	out := make([]runtime.LogLine, 0, len(stored)+len(tail))
	i := 0
	for ; i < len(stored) && (stored[i].At == nil || !stored[i].At.After(at)); i++ {
		out = append(out, stored[i])
	}
	for _, line := range tail {
		stamp := at
		out = append(out, runtime.LogLine{At: &stamp, Source: runtime.LogSourceApp,
			Stream: runtime.LogStreamStderr, Line: appout.Mask(line)})
	}
	return append(out, stored[i:]...)
}

// failedStartOutput is the workload output a readiness failure's message
// carries, unindented; nil when it carries none.
func failedStartOutput(msg string) []string {
	i := strings.Index(msg, failedStartOutputMarker)
	if i < 0 {
		return nil
	}
	var out []string
	for _, l := range strings.Split(msg[i+len(failedStartOutputMarker):], "\n") {
		if !strings.HasPrefix(l, "    ") {
			break
		}
		out = append(out, strings.TrimPrefix(l, "    "))
	}
	return out
}

// mergeLogs puts the two halves of a log together, by time wherever both
// halves have one.
//
// **Shared mode has a clock.** The runner stamps every line it writes to the
// agent's file (its marker, read back by runner.ParseLogLine), so when every
// backend line carries an `At` the platform's stored account and the app's
// output are one stream sorted by it — stably, so lines of one instant keep
// the order they were read in. A tail with *any* unstamped line — an unmarked
// line, which ParseLogLine returns with no time — is not sorted: LogLine.At is
// a pointer because a missing time is an absence, and a sort over holes would
// invent an order. That tail keeps the old shape, the platform's account above
// the app's output behind a separator saying what the boundary is.
//
// **Isolated mode has one on both halves**, so the two are merged by time and
// deduped: the pod's teed copy and the shipped copy of one platform line come
// from the same record() call over the same variables, so `(at, stream, line)`
// matches exactly with the stamp truncated to the millisecond the marker
// carries. Neither half is re-sorted — kube.Logs composes its restart
// separator without a stamp and it must stay where the backend put it — so
// stored rows are only ever *prepended*, above the oldest line the backend
// could still reach.
func mergeLogs(app *database.App, stored, backend []runtime.LogLine) logsResponse {
	merged := stored
	if app.RunningModeOrShared() == runtime.ModeIsolated {
		merged = prependOlder(stored, backend)
	} else if len(stored) > 0 && len(backend) > 0 {
		if allStamped(stored) && allStamped(backend) {
			// Deduped as isolated mode's are, and for its reason: the runner
			// writes each platform line to the agent's file and ships the same
			// record to the store, so both halves hold it with one stamp.
			merged = append(dropHeld(stored, backend), backend...)
			sort.SliceStable(merged, func(i, j int) bool { return merged[i].At.Before(*merged[j].At) })
		} else {
			merged = append(append(append([]runtime.LogLine{}, stored...),
				runtime.LogLine{
					Source: runtime.LogSourcePlatform,
					Stream: runtime.LogStreamStdout,
					Line:   sharedLogSeparator,
				}), backend...)
		}
	} else if len(stored) == 0 {
		merged = backend
	}

	if merged == nil {
		merged = []runtime.LogLine{}
	}
	return logsResponse{Logs: runtime.JoinLines(merged), Lines: merged}
}

// allStamped reports whether every line carries a time.
func allStamped(lines []runtime.LogLine) bool {
	for _, l := range lines {
		if l.At == nil {
			return false
		}
	}
	return true
}

// dropHeld is stored without the platform lines the backend also holds.
func dropHeld(stored, backend []runtime.LogLine) []runtime.LogLine {
	held := map[string]bool{}
	for _, l := range backend {
		if l.At != nil && l.Source == runtime.LogSourcePlatform {
			held[lineKey(*l.At, l.Stream, l.Line)] = true
		}
	}
	out := make([]runtime.LogLine, 0, len(stored))
	for _, l := range stored {
		if l.At == nil || !held[lineKey(*l.At, l.Stream, l.Line)] {
			out = append(out, l)
		}
	}
	return out
}

// recentLines is the socket's memory of the platform lines it has sent, so the
// one a runner both writes to the agent's file and ships to the store reaches
// the pane once, from whichever route delivered it first. Bounded by count:
// the two copies arrive within a second of each other.
type recentLines struct {
	seen  map[string]bool
	order []string
}

const recentLinesKept = 512

// fresh reports whether a platform line has not been sent yet, and remembers
// it. A line with no time cannot be matched and is always fresh.
func (r *recentLines) fresh(l runtime.LogLine) bool {
	if l.At == nil || l.Source != runtime.LogSourcePlatform {
		return true
	}
	k := lineKey(*l.At, l.Stream, l.Line)
	if r.seen[k] {
		return false
	}
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	r.seen[k] = true
	r.order = append(r.order, k)
	if len(r.order) > recentLinesKept {
		delete(r.seen, r.order[0])
		r.order = r.order[1:]
	}
	return true
}

// prependOlder puts what the backend can no longer reach above what it can,
// and drops the stored copy of anything it still holds.
//
// Position is by *generation* and never by timestamp, which is the same
// arrangement kube.Logs already makes between two container generations: the
// older block, a separator, the newer block. Sorting the two together by `at`
// would look tidier and be wrong — the backend composes its own restart
// separator without a stamp, so a sort would move it away from the restart it
// describes.
func prependOlder(stored, backend []runtime.LogLine) []runtime.LogLine {
	if len(stored) == 0 {
		return backend
	}
	if len(backend) == 0 {
		return stored
	}

	// The lines the backend is already showing. Only platform ones can collide:
	// an app line never reaches the store at all.
	held := map[string]bool{}
	for _, l := range backend {
		if l.At == nil || l.Source != runtime.LogSourcePlatform {
			continue
		}
		held[lineKey(*l.At, l.Stream, l.Line)] = true
	}

	older := make([]runtime.LogLine, 0, len(stored))
	for _, l := range stored {
		if l.At != nil && held[lineKey(*l.At, l.Stream, l.Line)] {
			continue
		}
		older = append(older, l)
	}
	if len(older) == 0 {
		return backend
	}

	// The boundary is stamped with the time of the line it sits in front of, so
	// a reader sorting by `at` keeps it in place. It used to carry no time at
	// all, which put it at the epoch — top of the log, in front of everything
	// it was supposed to divide.
	//
	// Falling back to the last stored line's time rather than to `now`: a
	// marker later than the lines beneath it would sort past them the moment a
	// caller does the obvious thing, and both fallbacks are only reachable when
	// a backend hands over lines with no time of their own.
	at := firstStamp(backend)
	if at == nil {
		at = lastStamp(older)
	}
	out := make([]runtime.LogLine, 0, len(older)+1+len(backend))
	out = append(out, older...)
	out = append(out, runtime.LogLine{
		At:     at,
		Source: runtime.LogSourcePlatform,
		Stream: runtime.LogStreamStdout,
		Marker: runtime.LogMarkerWorkloadStart,
		Line:   "── everything above is from the platform's record; the workload's own log begins here ──",
	})
	return append(out, backend...)
}

// lineKey is the identity of one platform line across the two copies of it.
// Millisecond precision because that is all the runner's marker carries.
func lineKey(at time.Time, stream, line string) string {
	return at.Format(runner.LogMarkerTimeLayout) + "\x00" + stream + "\x00" + line
}

// withReason attaches the absence word to an answer that may still have lines.
func withReason(resp logsResponse, reason string) logsResponse {
	resp.Reason = reason
	return resp
}

// absentLogs is the empty answer, carrying why. The slice is empty rather than
// nil for the reason stated on the field.
func absentLogs(reason string) logsResponse {
	return logsResponse{Lines: []runtime.LogLine{}, Reason: reason}
}

// logsResponse is the tail's shape. A client that reads only Logs sees exactly
// what it saw before.
type logsResponse struct {
	// Logs is the flat blob, kept because this route has readers beyond the
	// browser and this repository cannot enumerate them: `toft logs` reads it,
	// and a personal key and a deploy key both reach this route, so whatever
	// an install has automated against it is invisible from here. Duplication
	// over a broken contract.
	//
	// Derived from Lines rather than produced beside them — one parse, one
	// join. Producing the two independently is how they drift, and the drift
	// would be invisible: the CLI and the browser would each be right about a
	// different log.
	Logs string `json:"logs"`
	// Lines is the same output labeled, and is **always present** on a platform
	// that has it — `[]` when there is nothing to show. Absence of the key is
	// what tells a client it is talking to a platform too old to label lines,
	// and `toft` reads exactly that to decide whether --source can be honoured.
	// Marking it omitempty made an empty log indistinguishable from an old
	// platform, which is a wire bug rather than a display one.
	Lines []runtime.LogLine `json:"lines"`
	// Reason is one of runtime's LogAbsent* words. A closed set: the client
	// renders copy from it, so an unknown word renders as nothing.
	//
	// **It may now accompany output, and used not to.** It answers "why is
	// there no *workload*", which is a different question from "why is there
	// nothing to show" — a stopped app has no workload and still has the
	// platform's account of the start that preceded the stop, which is the
	// whole reason that account is kept. The client renders the word only when
	// there are no lines, so the pane that used to explain itself still does
	// and the pane that has something to show now shows it.
	//
	// `locked` is the one word that still arrives alone: logAbsenceReason
	// keeps its short-circuit, because a lock is an administrator taking the
	// app away rather than a description of a process, and the copy beside it
	// promises that nothing it printed is kept.
	Reason string `json:"reason,omitempty"`
}

// logAbsenceReason is why this app has no log to show, from the app row alone,
// or "" if the backend is the one to ask.
//
// The order is the order the answers stop being true in. A locked app is also
// stopped and an app that has never deployed is also not running, and in both
// cases the outer fact is the one worth saying: "an administrator took this
// app away" is a different sentence from "this app is stopped", and only one of
// them tells the owner what to do next.
// Since the platform log exists, only `locked` still means "and nothing to
// show". The other three travel beside whatever the platform said.
func (s *Server) logAbsenceReason(app *database.App) (string, error) {
	switch {
	case app.Locked():
		return runtime.LogAbsentLocked, nil
	case app.Status == database.AppStatusSleeping:
		return runtime.LogAbsentSleeping, nil
	case app.Status == database.AppStatusStopped:
		return runtime.LogAbsentStopped, nil
	}

	// "Never deployed" is asked of the deployment history, not of
	// `current_release`. That column is empty for apps written before releases
	// were named, and ProcessName deliberately falls back to the slug for
	// exactly those — so reading it as "never deployed" would blank the log of
	// the oldest working apps on the install.
	serving, err := s.db.ServingDeploymentID(app.ID)
	if err != nil {
		return "", err
	}
	if serving == "" {
		return runtime.LogAbsentNeverDeployed, nil
	}
	return "", nil
}

// The log socket's liveness clock. A ping every interval; a peer that has not
// answered within the wait is gone. The wait is longer than the interval so
// one lost pong does not drop a live pane.
const (
	logSocketPingInterval = 30 * time.Second
	logSocketReadWait     = 75 * time.Second
	logSocketWriteWait    = 10 * time.Second
)

func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	// Authorize before upgrading: once the connection is hijacked there is no
	// way left to write a status code.
	app, ok := s.authorizeAppLogs(w, r)
	if !ok {
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// The read side. gorilla/websocket learns that the peer has gone only on
	// a read or a write, and this socket used to do neither until the app
	// printed a line — so a closed tab on a quiet app held the goroutine, the
	// pod-log follow and the narration subscription until it did. One
	// goroutine reads and discards frames (there is nothing the browser sends
	// that means anything here) and closes clientGone when the read fails; a
	// pong handler refreshes the read deadline the ping ticker below keeps
	// alive, so a peer that vanished without a close frame is found within
	// one interval.
	clientGone := make(chan struct{})
	conn.SetReadDeadline(time.Now().Add(logSocketReadWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(logSocketReadWait))
	})
	go func() {
		defer close(clientGone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	ping := time.NewTicker(logSocketPingInterval)
	defer ping.Stop()

	// The one absence that is also a refusal, answered alone and before
	// anything is subscribed to, as the tail answers it: a locked app's
	// platform narration is not readable either.
	absent, err := s.logAbsenceReason(app)
	if err != nil {
		return
	}
	if absent == runtime.LogAbsentLocked {
		conn.WriteJSON(absentLogs(absent))
		return
	}

	// The platform's live voice, and only in shared mode.
	//
	// An isolated workload's platform lines already ride the pod stream the
	// backend follows, marked by the runner's own tee — subscribing there too
	// would put every line on the wire twice, and the tail's dedupe cannot
	// reach a socket. Shared mode's stream is the agent's, which carries the
	// runner's marked lines as a pod's does but nothing the platform says about
	// the app outside the runner — the backend's own narration, the health
	// watcher's — so this is the only live route those have.
	//
	// Subscribed before the backend's stream is opened, for the reason the
	// build log's socket re-reads after subscribing: whatever is said while
	// the stream is being opened would otherwise fall between the two.
	//
	// A nil channel blocks forever in a select, which is exactly what an
	// install with no sink should contribute to the loop below.
	var narration chan database.PlatformLogLine
	if s.platformLog != nil && app.RunningModeOrShared() == runtime.ModeShared {
		narration = s.platformLog.Subscribe(app.ID)
		defer s.platformLog.Unsubscribe(app.ID, narration)
	}

	// Facts the platform holds are answered without asking a backend, exactly
	// as the tail answers them — and this socket did not, which is how the two
	// surfaces came to disagree about one app.
	//
	// A *stopped* shared workload still answers a follow: the agent keeps the
	// entry and its output file. Under PM2, `pm2 logs --raw` there dumped the
	// whole file into a pane whose tail had just said there was no workload.
	// Nobody noticed while the tail was empty and the pane's only content came
	// from here. The agent's follow sends only new lines, but the rule stands:
	// the platform's facts are answered first, and the backend is not asked.
	var stream runtime.LogReader
	if absent == "" {
		rt, rerr := s.runtimeFor(app)
		if rerr != nil {
			writeStreamError(conn, rerr)
			return
		}
		stream, rerr = rt.LogStream(app.ProcessName())
		if rerr != nil {
			// An absent workload is not an error to report into the log pane.
			// The tail alongside it already carries the reason as a code the
			// client renders copy from, and a sentence written here would be a
			// second, English copy of the same fact.
			if runtime.AbsenceReason(rerr) != "" {
				stream = nil
			} else {
				writeStreamError(conn, rerr)
				return
			}
		}
	}
	if stream == nil && narration == nil {
		// Nothing to follow and nothing to say. Closing is what the client's
		// reconnect affordance is for, and it is what this did before.
		return
	}
	if stream != nil {
		defer stream.Close()
	}

	// The backend's stream, pumped onto a channel so it can be selected
	// against the platform's. Next() blocks, and there is no way to ask it not
	// to — the deferred Close above is what ends this goroutine, and `done` is
	// what releases it if it is mid-send when the socket goes away.
	// A nil channel blocks forever in a select, which is the right contribution
	// from a workload that is not there: the socket stays open on the
	// narration alone, so a start that happens while the pane is open arrives
	// live.
	var backend chan runtime.LogLine
	done := make(chan struct{})
	defer close(done)
	if stream != nil {
		backend = make(chan runtime.LogLine, 64)
		go func() {
			defer close(backend)
			for {
				line, err := stream.Next()
				if err != nil {
					return
				}
				if line.Source == runtime.LogSourceApp {
					line.Line = appout.Mask(line.Line)
				}
				select {
				case backend <- line:
				case <-done:
					return
				}
			}
		}()
	}

	// One JSON object per message rather than a raw line. Nothing but the
	// browser reads this socket — the CLI polls the tail — so unlike the tail
	// there is no compatibility half to preserve.
	//
	// One writer, because a websocket connection is not safe for concurrent
	// writes and two sources would otherwise interleave mid-frame.
	var sent recentLines
	for {
		select {
		case <-clientGone:
			// The tab closed, or the read deadline passed with no pong. The
			// deferred closes release the follow and the subscription.
			return
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(logSocketWriteWait)); err != nil {
				return
			}
		case line, ok := <-backend:
			if !ok {
				// The workload's log ended. Nothing is left to follow, and the
				// narration channel alone is not a reason to hold a socket the
				// client will reconnect anyway.
				return
			}
			if !sent.fresh(line) {
				continue
			}
			if err := conn.WriteJSON(line); err != nil {
				return
			}
		case l, ok := <-narration:
			if !ok {
				// Unsubscribed from under us. Keep following the workload:
				// losing the platform's live half is not a reason to close a
				// socket that is still carrying the app's.
				narration = nil
				continue
			}
			at := l.At
			line := runtime.LogLine{At: &at, Source: runtime.LogSourcePlatform, Stream: l.Stream, Line: l.Line}
			if !sent.fresh(line) {
				continue
			}
			if err := conn.WriteJSON(line); err != nil {
				return
			}
		}
	}
}

// writeStreamError reports a failure to follow the log as a platform line.
//
// It used to be a raw "Error: …" text frame, which the browser appended to the
// pane as though the app had printed it. Now that every frame is a labeled
// line, a raw one would not parse and would be dropped — so the failure would
// be silent, which is worse than either. Said in the platform's voice, it is
// visible and attributed to the right speaker.
func writeStreamError(conn *websocket.Conn, err error) {
	conn.WriteJSON(runtime.LogLine{
		Source: runtime.LogSourcePlatform,
		Stream: runtime.LogStreamStderr,
		Line:   "could not follow this app's log: " + err.Error(),
	})
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	app, ok := s.authorizeAppLogs(w, r)
	if !ok {
		return
	}

	deployments, err := s.db.ListDeployments(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.deploymentViews(app, deployments))
}

// deploymentView is a deployment plus the two facts a client cannot work out
// for itself: which build is actually being served, and whether this one may be
// deleted.
//
// "Newest successful build" is the obvious client-side guess and it is wrong
// after a promote, where the newest success may be an old build made current
// again. Rather than ship that guess to every client and have the API
// contradict it with a 409, the server says which is which.
type deploymentView struct {
	database.Deployment
	// BuildLog claims the embedded row's JSON name and is never written, so the
	// key never appears. The list returned the whole text of every retained
	// build — to a page that refetched it every three seconds while a deploy was
	// in flight — for one reader that has a dedicated route right beside it. One
	// route serves a build's log and it is `/deployments/{id}/logs`, which is
	// what `toft` already reads (ADR §87).
	//
	// **`json:"-"` does not do this.** A field tagged `-` has no JSON name, so
	// it does not conflict with the embedded field and does not shadow it — the
	// embedded `build_log` marshals exactly as before. Claiming the name at
	// depth 0 is what shadows it, because the shallower field wins; `omitempty`
	// on a value nothing ever sets is what drops the key rather than emitting an
	// empty string. `TestDeploymentListCarriesNoBuildLog` is the guard.
	BuildLog string `json:"build_log,omitempty"`
	// Current marks the row that records the running deploy.
	Current bool `json:"current"`
	// Serving marks the row that owns the release actually executing. It is
	// the same row as Current except for legacy rollback rows written before
	// promotes were folded back onto their source row.
	Serving bool `json:"serving"`
	// ProtectedReason is why this build cannot be deleted, or "" if it can.
	ProtectedReason string `json:"protected_reason"`
}

func (s *Server) deploymentViews(app *database.App, deployments []database.Deployment) []deploymentView {
	state := s.deletionStateFor(app)
	views := make([]deploymentView, 0, len(deployments))
	for i := range deployments {
		d := &deployments[i]
		protected := state.protects(app, d)
		if !s.listableRelease(app, d, protected) {
			continue
		}
		views = append(views, deploymentView{
			Deployment:      *d,
			Current:         builder.ReleaseNameIsDeployment(state.liveRowName, d.ID),
			Serving:         state.liveReleaseID != "" && state.liveReleaseID == d.ID,
			ProtectedReason: protected,
		})
	}
	return views
}

// listableRelease drops a successful build whose release no longer exists.
//
// Cleanup deletes the rows it prunes, so this normally has nothing to do. It
// exists for the rows that got orphaned another way — pruned by a version of
// this platform that kept them, or storage lost out of band — because a build
// that cannot be made current, downloaded, or rolled back to is not a version
// of anything, and listing it offers three actions that all end in 410.
//
// A failed or in-flight build is always listed: it never owned a release, and
// its row is the only record of why it failed. That includes the row the
// auto-deploy poller writes for a push it could not build.
//
// A store that is merely unreachable answers "not gone" (ReleaseAvailable), so
// an outage hides nobody's history.
//
// A protected row is never dropped, and the reason is not caution — it is that
// "owns no release" and "its release was deleted" look identical from the
// filesystem. A promote row owns nothing: while one exists it serves the
// directory its *source* published (see builder.DeleteRelease), so the row
// recording the running deploy can be exactly the row with no directory of its
// own. Protection is what tells the two apart, and it is already computed here.
func (s *Server) listableRelease(app *database.App, d *database.Deployment, protectedReason string) bool {
	if d.Status != database.DeployStatusSuccess || protectedReason != "" {
		return true
	}
	return builder.ReleaseAvailable(s.cfg.AppsDir, s.artifacts, app.ID, d.ID, d.ArtifactKey)
}

func (s *Server) handleDeploymentLogs(w http.ResponseWriter, r *http.Request) {
	app, deploy, ok := s.authorizeDeployment(w, r)
	if !ok {
		return
	}

	// A running build has nothing on its row until it ends; the builder's live
	// copy is the whole log so far, and the one to answer with.
	text := deploy.BuildLog
	live := false
	if s.builder != nil {
		if l, ok := s.builder.LiveLog(deploy.ID); ok {
			text, live = l, true
		}
	}
	// When each line was written (ADR §155), beside the text and never in it:
	// absent for a build that recorded none.
	var times string
	if live {
		times, _ = s.builder.LiveLogTimes(deploy.ID)
	} else if t, err := s.db.GetDeploymentLogTimes(deploy.ID); err == nil {
		times = t
	}
	lt := builder.DecodeLineTimes(times)
	if wantsDownload(r) {
		// The file is stamped as the run log's is: a file read tomorrow has no
		// pane beside it to supply the time.
		writeLogFile(w, r, buildLogFilename(app, deploy), buildLogText(text, lt))
		return
	}
	body := map[string]any{"build_log": text}
	if lt != nil {
		body["line_times"] = lt
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleDeployLogStream(w http.ResponseWriter, r *http.Request) {
	deploy, ok := s.authorizeDeploymentLogs(w, r)
	if !ok {
		return
	}
	deployID := deploy.ID

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	backlog, at, ch := s.builder.SubscribeLogsTimed(deployID)
	defer s.builder.UnsubscribeLogs(deployID, ch)

	// `?times=1` (ADR §155): every line of every message is "<unix ms>\t<line>",
	// or "-\t<line>" for one with no recorded time. Opt-in, so a reader that
	// takes the plain stream is handed exactly what it always was. "DONE" is
	// framing and stays bare either way.
	timed := r.URL.Query().Get("times") == "1"

	// Everything before the first live line, from wherever it is: the builder's
	// live copy while the build runs, which the subscribe took atomically with
	// the channel, and the row once it has ended. The row is re-read after
	// subscribing, not before: a build that ended in between is on it by now.
	// Authorization already used the earlier copy.
	if backlog != nil {
		if len(backlog) > 0 {
			msg := strings.Join(backlog, "\n") + "\n"
			if timed {
				msg = stampedMessage(builder.StampLines(backlog, at))
			}
			conn.WriteMessage(websocket.TextMessage, []byte(msg))
		}
	} else if current, _ := s.db.GetDeployment(deployID); current != nil {
		if current.BuildLog != "" {
			msg := current.BuildLog
			if timed {
				enc, _ := s.db.GetDeploymentLogTimes(deployID)
				msg = stampedMessage(storedStamps(current.BuildLog, builder.DecodeLineTimes(enc)))
			}
			conn.WriteMessage(websocket.TextMessage, []byte(msg))
		}
		// A deploy that is over has nothing more to send: its terminator went
		// out before this socket opened, and waiting on the channel held the
		// handler for the life of the process (F-9).
		if current.Status == database.DeployStatusSuccess || current.Status == database.DeployStatusFailed {
			conn.WriteMessage(websocket.TextMessage, []byte("DONE"))
			return
		}
	}

	// The client hanging up is noticed: nothing read the socket, so a reader
	// that went away left this handler waiting on the channel for ever (F-9).
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()
	for {
		var line string
		var ok bool
		select {
		case line, ok = <-ch:
			if !ok {
				return
			}
		case <-gone:
			return
		case <-r.Context().Done():
			return
		}
		msg := line
		if timed && line != "DONE" {
			// Stamped on receipt: the channel is in-process and the gap is
			// microseconds, and the row's copy is the builder's own clock.
			msg = stampedMessage(builder.StampLines([]string{line}, []time.Time{time.Now()}))
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			return
		}
		if line == "DONE" {
			return
		}
	}
}

// firstStamp is the time of the earliest line carrying one, and lastStamp the
// latest. Both answer nil for a run of lines that carry none — unmarked lines
// are the case, and a zero time rendered as a date would be a claim rather than
// an absence.
func firstStamp(lines []runtime.LogLine) *time.Time {
	for i := range lines {
		if lines[i].At != nil {
			return lines[i].At
		}
	}
	return nil
}

func lastStamp(lines []runtime.LogLine) *time.Time {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].At != nil {
			return lines[i].At
		}
	}
	return nil
}

// stampedMessage is one socket message under `?times=1`: each line as
// "<unix ms>\t<line>", "-" where the time is unknown, newline-terminated.
func stampedMessage(lines []string, ms []int64) string {
	var sb strings.Builder
	for i, l := range lines {
		if i < len(ms) && ms[i] >= 0 {
			sb.WriteString(strconv.FormatInt(ms[i], 10))
		} else {
			sb.WriteByte('-')
		}
		sb.WriteByte('\t')
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// storedStamps pairs a stored log's lines with its stored times from the top;
// lines past the end of the times (or all of them, with none) are unknown.
func storedStamps(text string, lt *builder.LineTimes) ([]string, []int64) {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	ms := make([]int64, len(lines))
	for i := range ms {
		ms[i] = -1
		if lt != nil && i < len(lt.OffsetsMS) {
			ms[i] = lt.StartMS + lt.OffsetsMS[i]
		}
	}
	return lines, ms
}
