package api

import (
	"html/template"
	"net/http"
	"strings"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
)

// RequestDeadlineHeader is when the request an app is serving will be cut off,
// RFC 3339, set by the gateway on every forwarded request. The SDK reads it to
// refuse a wait() that would outlive the request (would_outlive_request) and
// to point at respond(run) instead.
const RequestDeadlineHeader = "X-Toft-Request-Deadline"

// setRequestDeadline stamps a forwarded request with its cut-off — except a
// WebSocket upgrade, whose connection outlives any deadline the gateway could
// name: the SDKs document that an upgrade carries none, and a Streamlit or
// marimo session's headers *are* its handshake's, so a deadline there would be
// minutes stale for the rest of the session (drills on trytoft, 2026-10-23).
func setRequestDeadline(req *http.Request, now time.Time, d time.Duration) {
	if isUpgrade(req.Header) {
		req.Header.Del(RequestDeadlineHeader)
		return
	}
	req.Header.Set(RequestDeadlineHeader, now.UTC().Add(d).Format(time.RFC3339))
}

// isUpgrade is a request asking to switch protocols: Connection names upgrade.
func isUpgrade(h http.Header) bool {
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// runStatusRequest reports whether path is a run's status page under this
// app's base path, and the run it names.
func runStatusRequest(path, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(path, prefix+runStatusSegment)
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// runStatus is what the status page shows, and what it answers a script with.
type runStatus struct {
	ID              string     `json:"id"`
	Job             string     `json:"job"`
	Status          string     `json:"status"`
	Progress        int        `json:"progress"`
	ProgressMessage string     `json:"progress_message,omitempty"`
	QueuePosition   int        `json:"queue_position,omitempty"`
	FailureReason   string     `json:"failure_reason,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	Done            bool       `json:"done"`
}

// serveRunStatus answers /apps/{slug}/__toft/runs/{id}: the live status page
// respond(run) sends a browser to, styled by the platform with no app code.
//
// Who may see it: anyone who can reach the app, for a run the app started as
// itself; for a run started in a person's name, that person and the app's
// owners and editors; for any other run — a schedule's, a deploy key's — the
// owners and editors alone. 404 otherwise, the same as a run that does not
// exist — a run another viewer started is theirs.
func (s *Server) serveRunStatus(w http.ResponseWriter, r *http.Request, app *database.App, p *auth.Principal, runID string) {
	// Every answer on this path is the platform's — the page, its JSON and
	// its 404 — and says so, as the cards do (BB-014, second 2026-11-03 bash).
	w.Header().Set(interstitialHeader, "run_page")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	run, err := s.db.GetAppJobRun(app.ID, runID, "")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Open only for a run the app started as itself — the page respond(run)
	// hands its own visitor. A schedule's run and a deploy key's carry no
	// actor either, and were served to every viewer and to a public app's
	// anonymous visitors, failure reason included (BB-110): those are the
	// app's editors', as job history is.
	if run != nil && !(run.ActorID == "" && run.StartedBy == database.JobStartedByApp) {
		mine := p != nil && run.ActorID != "" && p.ID == run.ActorID
		if !mine && !auth.Authorize(p, auth.ActionEditContent, objectFromApp(app), s.db).Allow {
			run = nil
		}
	}
	if run == nil {
		http.Error(w, "no such run", http.StatusNotFound)
		return
	}

	st := runStatus{
		ID: run.ID, Job: run.JobName, Status: run.ShownStatus(), Progress: run.Progress,
		ProgressMessage: run.ProgressMessage, FailureReason: run.FailureReason,
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, Done: database.JobRunTerminal(run.Status),
	}
	if run.Status == database.JobRunQueued {
		st.QueuePosition, _ = s.db.QueuePosition(run)
	}
	w.Header().Set("Cache-Control", "no-store")
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		writeJSON(w, http.StatusOK, st)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Its own policy, not the cards': its polling script carries template
	// data, so no hash taken once stands for every render. The helper adds
	// the nosniff and referrer policy the cards carry (gateway_pages.go).
	gatewayPageHeaders(w,
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	// The app's home is under the prefix the request arrived on: /apps/{slug}
	// on the shared origin, the root on the app's own hostname (security
	// review 2026-11-15, HD-8).
	home := "/apps/" + app.Slug + "/"
	if prefix, ok := r.Context().Value(urlPrefixKey{}).(string); ok {
		home = prefix + "/"
	}
	_ = runStatusTemplate.Execute(w, struct {
		Run     runStatus
		AppName string
		AppHome string
	}{st, app.Name, home})
}

// runStatusTemplate is the page. Self-contained — no stylesheet, no script from
// anywhere else — because it is served on the app's origin, where the platform
// owns nothing but this one path. It polls its own URL for JSON until the run
// ends.
var runStatusTemplate = template.Must(template.New("run").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Run.Job}} · {{.AppName}}</title>
<style>
:root{--fg:#1f2328;--muted:#59636e;--line:#d1d9e0;--bg:#f6f8fa;--ok:#1a7f37;--bad:#cf222e;--bar:#0969da}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif}
main{max-width:34rem;margin:12vh auto;padding:0 16px}
.card{background:#fff;border:1px solid var(--line);border-radius:8px;padding:24px}
h1{font-size:18px;margin:0 0 4px}.app{color:var(--muted);font-size:13px;margin:0 0 20px}
.state{font-weight:600;margin:0 0 8px}.state.ok{color:var(--ok)}.state.bad{color:var(--bad)}
.track{height:8px;background:var(--bg);border:1px solid var(--line);border-radius:4px;overflow:hidden}
.fill{height:100%;background:var(--bar);width:0;transition:width .4s}
.msg{color:var(--muted);font-size:13px;margin:8px 0 0;min-height:1.5em;overflow-wrap:anywhere}
a{color:var(--bar)}.back{display:inline-block;margin-top:20px;font-size:14px}
</style></head><body><main><div class="card">
<h1>{{.Run.Job}}</h1><p class="app">{{.AppName}}</p>
<p class="state" id="state">{{.Run.Status}}</p>
<div class="track"><div class="fill" id="fill"></div></div>
<p class="msg" id="msg"></p>
<a class="back" id="back" href="{{.AppHome}}" hidden>Back to {{.AppName}}</a>
</div></main>
<script>
(function(){
  var words={queued:"Waiting to start",running:"Running",succeeded:"Done",failed:"Failed",
    timed_out:"Ran out of time",out_of_memory:"Ran out of memory",cancelled:"Cancelled",skipped:"Skipped"};
  function show(r){
    var s=document.getElementById("state"),f=document.getElementById("fill"),m=document.getElementById("msg");
    var word=words[r.status]||r.status;
    if(r.status==="queued"&&r.queue_position){word+=" · "+r.queue_position+" in line"}
    if(r.status==="running"&&r.progress){word+=" · "+r.progress+"%"}
    s.textContent=word;
    s.className="state"+(r.status==="succeeded"?" ok":(r.done&&r.status!=="cancelled"?" bad":""));
    f.style.width=(r.status==="succeeded"?100:r.progress)+"%";
    m.textContent=r.done?(r.failure_reason||""):(r.progress_message||"");
    if(r.done){document.getElementById("back").hidden=false}
    return r.done;
  }
  var first={{.Run}};
  if(show(first))return;
  function poll(){
    fetch(location.pathname,{headers:{Accept:"application/json"},credentials:"same-origin"})
      .then(function(res){return res.ok?res.json():null})
      .then(function(r){if(!r||!show(r)){setTimeout(poll,2000)}})
      .catch(function(){setTimeout(poll,5000)});
  }
  setTimeout(poll,1500);
})();
</script></body></html>
`))
