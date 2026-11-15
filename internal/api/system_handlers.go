package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	stdruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"toft/internal/agent"
	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/runtime"
	"toft/internal/toolchain"
	"toft/internal/version"
)

// serverStarted anchors the process uptime reading; package-level so it is
// captured at boot, not at first request.
var serverStarted = time.Now()

type serverInfo struct {
	Version string `json:"version"`
	// Revision is the commit the binary was built from, or "" when the build
	// carried no VCS stamp. It supplements Version; it never stands in for it.
	Revision  string `json:"revision"`
	GoVersion string `json:"go_version"`
	// Runtimes are the toolchains on *this* host — what shared-mode builds and
	// processes actually get. An empty version means the binary is absent,
	// which is a real configuration (isolated-only installs), not an error.
	Runtimes []runtimeVersion `json:"runtimes"`
	// Toolchains is the same three languages as a *set* rather than a row: what
	// this host has, which one is the default, where each came from, and what
	// the isolated runner images are declared to carry.
	//
	// Both columns, because a pin is judged against the mode the app runs in
	// and an administrator has to be able to see the two agree. Nothing else
	// makes a divergence between the platform image and a runner image visible
	// — the two are held in step today by a comment in each Dockerfile.
	Toolchains    []toolchainSet `json:"toolchains"`
	Hostname      string         `json:"hostname"`
	OS            string         `json:"os"`
	StartedAt     time.Time      `json:"started_at"`
	UptimeSeconds int64          `json:"uptime_seconds"`
}

type runtimeVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// toolchainSet is one language's whole answer.
type toolchainSet struct {
	Language string `json:"language"`
	Label    string `json:"label"`
	// Installed is what is on this host — the build host in both execution
	// modes, since the build always happens here.
	Installed []toolchain.Version `json:"installed"`
	// Runner is what the isolated image for this language is *declared* to
	// carry, or "" for an install with no such declaration — and always for Go,
	// which leaves its toolchain on the build host and ships a binary.
	Runner string `json:"runner,omitempty"`
	// Posture is Go's GOTOOLCHAIN, and only Go's. An administrator debugging a
	// Go build that cannot reach the module proxy needs the installed toolchain
	// and this in the same place.
	Posture string `json:"posture,omitempty"`
}

// collectToolchains renders the three languages as sets.
func collectToolchains() []toolchainSet {
	out := make([]toolchainSet, 0, len(toolchain.Languages))
	for _, lang := range toolchain.Languages {
		set := toolchainSet{
			Language:  lang,
			Label:     toolchain.Label(lang),
			Installed: toolchain.Available(lang),
		}
		if runner := toolchain.Configured(lang); len(runner) > 0 {
			// The whole declaration. An image may carry several interpreters
			// (D19), and rendering only the first would show an administrator
			// one version while the platform resolves deploys against three.
			versions := make([]string, 0, len(runner))
			for _, v := range runner {
				versions = append(versions, v.Version)
			}
			set.Runner = strings.Join(versions, ", ")
		}
		if lang == toolchain.Go {
			set.Posture = toolchain.GoToolchainPosture()
		}
		out = append(out, set)
	}
	return out
}

var (
	serverInfoOnce   sync.Once
	cachedServerInfo serverInfo
)

// getServerInfo describes this Toft process and its host. Everything but
// the uptime is fixed for the life of the process, and `node --version` is an
// exec, so the facts are collected once.
func getServerInfo() serverInfo {
	serverInfoOnce.Do(func() {
		info := serverInfo{
			Version:   version.Version,
			Revision:  version.Revision(),
			GoVersion: stdruntime.Version(),
			OS:        osDescription(),
			StartedAt: serverStarted.UTC(),
		}
		info.Hostname, _ = os.Hostname()
		info.Runtimes = collectRuntimeVersions()
		info.Toolchains = collectToolchains()
		cachedServerInfo = info
	})
	info := cachedServerInfo
	info.UptimeSeconds = int64(time.Since(serverStarted).Seconds())
	return info
}

// versionOutput runs a --version-style command with the clean base
// environment — the same rule as every other child exec. Returns the trimmed first output line, or "" when the binary is
// absent or unresponsive.
func versionOutput(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Env = runtime.BaseEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	// uv appends its platform triple in parentheses; nobody needs it here.
	if i := strings.Index(line, " ("); i > 0 {
		line = line[:i]
	}
	return line
}

// collectRuntimeVersions probes every toolchain the shared build and run path
// uses: node/npm builds, the host Go toolchain, uv-managed Python, and git for
// clones. Shared workloads are placed by the platform's own binary (`toft
// agent`), which has no version of its own to show. Keep the list in sync with what
// internal/builder and internal/runtime actually exec.
//
// The *languages* come from internal/toolchain rather than from an exec here.
// That package probes the same binaries the same way, and it is where the probe
// has to live: an install may have several versions of one language, and a
// deploy is refused on the answer — a fact a build refuses on cannot live in a
// package only a page can reach (ADR §64 D10). What is left here is the
// platform's own tooling, which is not a language and has no set.
//
// The set is toolchain.Languages and is never restated here. This used to be a
// hand-written []string{Node, Python, Go}, which agreed with that list only
// because nobody had added a language since it was written; a fourth one would
// have left this page — the one screen an operator opens to find out what the
// install can run — quietly a language short, with nothing failing to say so.
func collectRuntimeVersions() []runtimeVersion {
	out := make([]runtimeVersion, 0, len(toolchain.Languages)+4)
	for _, lang := range toolchain.Languages {
		// The default, because this row answers "what does an app with no pin
		// get here?". The whole set is the Toolchains block below.
		var v string
		for _, av := range toolchain.Available(lang) {
			if av.Default {
				v = av.Version
				break
			}
		}
		out = append(out, runtimeVersion{Name: toolchain.Label(lang), Version: v})
	}
	// Quarto is not a language and has no set: it is one program that either
	// exists on this host or does not, and the whole of the question is which.
	// It is here rather than in the Toolchains block for that reason — and it
	// is here at all because "the install has no Quarto" is the answer behind
	// every refusal of a .qmd, and an administrator should be able to read it
	// somewhere other than a failed deploy's log.
	_, quartoVersion := toolchain.Quarto()
	return append(out,
		runtimeVersion{Name: "Quarto", Version: quartoVersion},
		runtimeVersion{Name: "uv", Version: strings.TrimPrefix(versionOutput("uv", "--version"), "uv ")},
		runtimeVersion{Name: "Git", Version: strings.TrimPrefix(versionOutput("git", "--version"), "git version ")},
	)
}

// retakeAbsentRuntime re-runs the version probe behind one *non-language* row
// of the system checks page. Called only from there, and only for a row that
// currently reads absent.
//
// collectRuntimeVersions runs inside getServerInfo's sync.Once, so "Not
// installed" is a fact about start-up being read by an operator who may have
// installed the thing since. For a language the retake belongs to
// internal/toolchain, which owns the cache a deploy resolves against and must
// not have it moved underneath a build. These two are the platform's own
// tooling — nothing resolves against them and no deploy is refused on them — so
// the retake is local, and the answer is used for the row and thrown away.
func retakeAbsentRuntime(name string) string {
	switch name {
	case "uv":
		return strings.TrimPrefix(versionOutput("uv", "--version"), "uv ")
	case "Git":
		return strings.TrimPrefix(versionOutput("git", "--version"), "git version ")
	}
	return ""
}

// osDescription prefers the distro's self-description over a bare GOOS.
func osDescription() string {
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				if name := strings.Trim(v, `"`); name != "" {
					return name + " (" + stdruntime.GOARCH + ")"
				}
			}
		}
	}
	return stdruntime.GOOS + "/" + stdruntime.GOARCH
}

// handleSystemHealth reports app health. It used to call ListAllApps with no
// principal check at all, which handed an anonymous caller the id, name, slug,
// and status of every app — including private ones — plus host metrics. That was
// the enumeration primitive the rest of the API's 404-over-403 handling exists to
// prevent.
func (s *Server) handleSystemHealth(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// Admins see the estate; everyone else sees only the apps they could already
	// list, on the same visibility and grant rules as GET /apps.
	// A failed query is 500, not an empty and healthy-looking estate: an
	// uptime probe reading `total: 0` and no crashed apps at the moment the
	// database is unreachable would be told the least alarming thing at the
	// worst time. handleQueue answers the same way.
	var apps []database.App
	var err error
	if p.IsAdmin() {
		apps, err = s.db.ListAllApps()
	} else {
		apps, err = s.db.ListApps(p.ID, p.Type, p.Groups)
	}
	if err != nil {
		log.Printf("system health: listing apps: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// A non-administrator's list is the *listable* set, and the list rung sees
	// an app without being able to open it: its status, its readings and its
	// place in the counts go only to a caller who could read the app. The
	// grants for the whole list in one query, as GET /apps resolves them.
	var roles auth.RoleResolver = s.db
	if !p.IsAdmin() {
		ids := make([]string, 0, len(apps))
		for _, app := range apps {
			ids = append(ids, app.ID)
		}
		if found, err := s.db.EffectiveRolesForObjects(ids, p.Type, p.ID, p.Groups); err != nil {
			log.Printf("system health: resolving grants: %v", err)
		} else {
			roles = pageRoles(found)
		}
	}

	readable := make(map[string]bool, len(apps))
	for _, app := range apps {
		readable[app.ID] = p.IsAdmin() || auth.Authorize(p, auth.ActionRead, objectFromApp(&app), roles).Allow
	}

	// `failed` is counted apart from `crashed`, and used to be folded into it.
	// The two words are the platform's own and the estate list filters on each
	// separately, so a reader sent to "1 app crashed" arrived at an empty
	// Crashed filter with the failed app one tab over. A build that never
	// produced a release and a workload that died are also different jobs: one
	// is fixed in the build log, the other in the run log.
	var running, crashed, failed, building, stopped, sleeping, ready int
	for _, app := range apps {
		// The counts are statuses too, so they are taken over the apps the
		// caller could open: a list-rung app counted here would be its status
		// told by subtraction.
		if !readable[app.ID] {
			continue
		}
		switch app.Status {
		case database.AppStatusRunning:
			running++
		case database.AppStatusCrashed:
			crashed++
		case database.AppStatusBuilding:
			building++
		case database.AppStatusStopped:
			stopped++
		case database.AppStatusFailed:
			failed++
		// Counted apart from stopped on purpose: "idle, wakes on the next
		// visit" and "someone turned these off" are different facts about the
		// estate, and folding them together would hide both.
		case database.AppStatusSleeping:
			sleeping++
		// A batch app with a release (ADR §107): live and serving nothing,
		// which is neither running nor stopped.
		case database.AppStatusReady:
			ready++
		}
	}

	// Host CPU, memory, and disk are infrastructure detail, not app detail.
	//
	// A backend may also have no such thing to report: "the machine the apps
	// run on" is a real, shared, finite thing for the shared backend, and
	// meaningless for a workload with a machine to itself. That answer is
	// ErrNotSupported and it is not an error — it must reach the client as an
	// absent reading, never as a zeroed struct that would render as an idle,
	// empty host.
	var sysMetrics *runtime.SystemMetrics
	systemAvailable := false
	if p.IsAdmin() {
		m, err := s.rts.SystemMetrics()
		switch {
		case errors.Is(err, runtime.ErrNotSupported):
			// Leave it absent.
		case err != nil:
			log.Printf("system metrics: %v", err)
		default:
			sysMetrics, systemAvailable = m, m != nil
		}
	}

	runtimeMetrics, _ := s.rts.List()

	type appSummary struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Slug string `json:"slug"`
		// Absent for an app held at the `list` rung, with Metrics: the rung
		// discloses name, slug, owner and framework, and whether an app is up
		// is something it takes opening it to learn (BB-22).
		Status  string              `json:"status,omitempty"`
		Metrics *appMetricsResponse `json:"metrics,omitempty"`
	}

	var appList []appSummary
	for _, app := range apps {
		as := appSummary{
			ID:   app.ID,
			Name: app.Name,
			Slug: app.Slug,
		}
		if !readable[app.ID] {
			appList = append(appList, as)
			continue
		}
		as.Status = app.Status
		if m, ok := runtimeMetrics[app.ProcessName()]; ok {
			as.Metrics = &appMetricsResponse{
				CPU:      m.CPU,
				Memory:   m.Memory,
				Uptime:   m.Uptime,
				Restarts: m.Restarts,
				Status:   m.Status,
			}
		}
		appList = append(appList, as)
	}

	resp := map[string]interface{}{
		"system": sysMetrics,
		// Distinguishes "this install has no host to report on" from "the read
		// failed", which a null alone cannot.
		"system_available": systemAvailable,
		"summary": map[string]int{
			"total":    len(apps),
			"running":  running,
			"crashed":  crashed,
			"failed":   failed,
			"building": building,
			"stopped":  stopped,
			"sleeping": sleeping,
			"ready":    ready,
		},
		"apps": appList,
	}
	// Like the host metrics: hostname, OS, and versions are infrastructure
	// detail, so only admins get them.
	if p.IsAdmin() {
		resp["server"] = getServerInfo()
	}
	writeJSON(w, http.StatusOK, resp)
}

// queueRow is one unfinished build, with the position it will be taken in.
type queueRow struct {
	database.QueuedDeployment
	// Position is the row's place in the order the workers take builds in,
	// oldest first. Derived from that order rather than stored: the queue is an
	// in-memory channel, and a number written to a row would be wrong the
	// moment anything ahead of it finished.
	Position int `json:"position"`
}

// handleQueue — GET /system/queue. system_admin only, like /system/processes:
// it names every app with a build in flight, which is the estate.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}

	deps, err := s.db.ListActiveDeployments()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	rows := make([]queueRow, 0, len(deps))
	for i, d := range deps {
		rows = append(rows, queueRow{QueuedDeployment: d, Position: i + 1})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(rows),
		"items": rows,
		// How many of those rows can be building at once — the dial as it is
		// now, not the number this process started with, because an
		// administrator who raised it and still saw "1" would conclude the
		// setting did nothing. Without it the page cannot say whether a waiting
		// build is waiting on anything at all.
		"concurrency": s.envelope().MaxConcurrentBuilds,
	})
}

// jobRunStatuses is the filter vocabulary: every stored status, plus the two
// pseudo-statuses. An unknown value is refused by name rather than quietly
// returning everything, because a filter that silently does nothing is how an
// operator concludes there were no failures.
var jobRunStatuses = map[string]bool{
	database.JobRunInFlight:     true,
	database.JobRunUnsuccessful: true,
	database.JobRunQueued:       true,
	database.JobRunRunning:      true,
	database.JobRunSucceeded:    true,
	database.JobRunFailed:       true,
	database.JobRunCancelled:    true,
	database.JobRunTimedOut:     true,
	database.JobRunOutOfMemory:  true,
	database.JobRunSkipped:      true,
}

// jobRunStatusNames is the filter vocabulary in a stable order, for the hint
// that answers a status it does not know.
func jobRunStatusNames() []string {
	out := make([]string, 0, len(jobRunStatuses))
	for s := range jobRunStatuses {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// handleSystemJobRuns — GET /system/job-runs. system_admin only, like
// /system/processes and /system/queue: it names every app with a run in flight,
// which is the estate.
//
// This is the only cross-app view of the job system. Everything else about a
// run — the per-app reads, the Jobs tab — is scoped to one app and reached
// through it, which leaves an app-started run that fails invisible to an
// operator until somebody complains. Builds have Admin → Queue; this is the
// same page for the other queue.
//
// Deliberately *not* the build queue's shape. Builds share one in-memory
// channel, so a global position means something; runs are limited per app, so
// the position a pending run carries is its place in its own app's line and is
// labelled that way. Inventing an install-wide position here would be a number
// nothing enforces.
func (s *Server) handleSystemJobRuns(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}

	q := r.URL.Query()
	status := q.Get("status")
	if status != "" && !jobRunStatuses[status] {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "unknown status: " + status,
			"hint":  "send status as one of " + strings.Join(jobRunStatusNames(), ", ") + ", or leave it out",
		})
		return
	}
	filter := database.JobRunFilter{AppID: q.Get("app_id"), Status: status}
	filter.Limit, _ = strconv.Atoi(q.Get("limit"))
	if days, err := strconv.Atoi(q.Get("days")); err == nil && days > 0 {
		since := time.Now().UTC().AddDate(0, 0, -days)
		filter.Since = &since
	}

	runs, err := s.db.ListAllJobRuns(filter)
	if err != nil {
		log.Printf("system job runs: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Where each queued run stands in its app's line. Computed here rather
	// than stored, for the reason the per-app read computes it: it changes
	// every time anything ahead of it starts. Only queued rows carry one, so
	// this is bounded by the queue cap times the number of apps queueing.
	for i := range runs {
		if runs[i].Status == database.JobRunQueued {
			if pos, err := s.db.QueuePosition(&runs[i].JobRun); err == nil {
				runs[i].QueuePosition = pos
			}
		}
		runs[i].Status = runs[i].ShownStatus()
	}

	caps := s.capabilities()
	writeJSON(w, http.StatusOK, map[string]any{
		"runs": runs,
		// The capability state travels with the list for the reason it travels
		// with an app's Jobs tab: an install that never turned jobs on returns
		// an empty list, and empty-because-quiet and empty-because-off are not
		// the same page.
		"jobs_available":  caps.BackgroundJobsAvailable(),
		"unavailable_bec": caps.ExplainJobsUnavailable(runtime.ModeIsolated),
		// What limits a run here — per app, not install-wide: how many run at
		// once, and how many may wait.
		"concurrency_limit": settingsOrDefault(s.db).JobConcurrency,
		"max_queued":        settingsOrDefault(s.db).MaxQueuedRunsPerApp,
	})
}

// processRow is one live workload: what is running, for whom, and under which
// execution mode.
//
// It is deliberately app-shaped rather than process-shaped. Nothing outside
// internal/runtime may name a path or an address, so a row carries the app, its
// release name, and the backend's own readings — never a pid path, a port, or a
// command line.
type processRow struct {
	AppID     string `json:"app_id"`
	AppName   string `json:"app_name"`
	AppSlug   string `json:"app_slug"`
	Framework string `json:"framework,omitempty"`
	// Release is the process name the backend knows this workload by — the
	// current release id, which is what makes two deploys of one app distinct.
	Release string `json:"release"`
	// Mode is the mode the workload is *running* under, not the one the next
	// deploy will use.
	Mode       string `json:"mode"`
	OwnerID    string `json:"owner_id,omitempty"`
	OwnerLogin string `json:"owner_login,omitempty"`
	OwnerName  string `json:"owner_name,omitempty"`
	// OwnerGroup is the owning group's name, empty when a person owns the app.
	// A group is not a person with an unusual name, and this column is where
	// that would otherwise be indistinguishable.
	OwnerGroup string `json:"owner_group,omitempty"`
	// Status is the app's recorded status; ProcessStatus is what the backend
	// says about the workload right now, in the four-word vocabulary both
	// backends share plus "missing" for a name no backend holds at all. They
	// disagree exactly when reconciliation has not caught up, and that
	// disagreement is the only reason a row that is not up appears here at
	// all: the app is recorded running and there is no process behind it,
	// whether the backend says so ("stopped", "errored") or has never heard of
	// the name ("missing").
	Status        string `json:"status"`
	ProcessStatus string `json:"process_status"`
	// Absent for a workload whose backend takes no reading — the same sentence
	// as Host below, which was already written for the same page: a figure
	// nobody measured is not Toft's to report. The row still appears, and
	// still carries its status, uptime and restarts.
	CPU    *float64 `json:"cpu,omitempty"`
	Memory *float64 `json:"memory,omitempty"`
	// Uptime is zero for a workload that is not up, whatever its backend still
	// remembers: a stopped entry may keep the start time of the process that
	// used to be behind it (PM2's did), and reporting that is a figure counting
	// up for something that has not run in days. Restarts is a real fact either
	// way.
	Uptime   int64 `json:"uptime"`
	Restarts int   `json:"restarts"`
	// Host is this machine for a shared workload, and empty for an isolated one
	// — a pod runs on a node Toft does not name and should not pretend to.
	Host string `json:"host,omitempty"`
}

// handleProcesses — GET /system/processes. system_admin only: this is the
// estate, including apps the caller has no grant on, which is exactly the
// enumeration a non-admin must not get.
func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "sign in as an administrator to read the process list"})
		return
	}

	apps, err := s.db.ListAllApps()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// A backend that is briefly unreachable must not blank the page: List
	// already merges what answered, and an error with nothing behind it is
	// reported as a degraded read rather than a failure.
	metrics, listErr := s.rts.List()

	ownerIDs := make([]string, 0, len(apps))
	for _, app := range apps {
		if app.OwnerID != "" {
			ownerIDs = append(ownerIDs, app.OwnerID)
		}
	}
	owners, err := s.db.GetUsersByIDs(ownerIDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	host, _ := os.Hostname()
	rows := make([]processRow, 0, len(apps))
	// What this page leaves out, counted rather than listed. Both are apps
	// with no process and nobody worried about them: the page's subject is the
	// machine right now, and a row that reads "—" in every column it exists
	// for is not a workload, it is an app somewhere else on the estate. The
	// counts go back so the table can say so once underneath itself, which is
	// the alternative to an operator concluding their app has vanished.
	var asleep, halted int
	for _, app := range apps {
		m := metrics[app.ProcessName()]
		// A live workload, plus anything recorded as running that is not one.
		// An app that is down and known to be down is not a process and does
		// not belong on this list.
		//
		// "Down" is the backend's own word, not the absence of an entry, and
		// the difference is the whole of this condition. The agent keeps a
		// stopped workload's entry, and the backend its port, so a later start
		// lands on the same port — so in shared mode every app anyone ever
		// stopped, and every app the idle reaper ever slept, answers List()
		// for as long as the entry lasts, with a pid of 0 and no readings
		// (under PM2, with its last start time still ticking up as an uptime
		// too). The isolated
		// backend deletes the pod and this list was right there by accident.
		// Reading the word rather than the row is what makes the two backends
		// mean the same thing, and it is what the route already claimed.
		//
		// **A crash loop is a process.** Between restarts of one the backend
		// holds the workload back — `errored` with CrashLooping, the agent's
		// backoff and the kubelet's CrashLoopBackOff alike (ADR §138) — and
		// the health watcher calls the app crashed; dropped here, the one
		// workload an operator opened this page to find was not on it.
		looping := m != nil && m.CrashLooping
		if (m == nil || !runtime.WorkloadIsUp(m.Status)) && !looping && app.Status != database.AppStatusRunning {
			switch app.Status {
			case database.AppStatusSleeping:
				asleep++
			case database.AppStatusStopped:
				halted++
			}
			continue
		}

		row := processRow{
			AppID: app.ID, AppName: app.Name, AppSlug: app.Slug,
			Framework: app.Framework,
			Release:   app.ProcessName(),
			Mode:      string(app.RunningModeOrShared()),
			OwnerID:   app.OwnerID,
			Status:    app.Status,
		}
		if o, ok := owners[app.OwnerID]; ok {
			row.OwnerLogin, row.OwnerName = o.Login, o.Name
		}
		if app.OwnerGroupID != "" {
			if g, err := s.db.GetGroup(app.OwnerGroupID); err == nil && g != nil {
				row.OwnerGroup = g.Name
			}
		}
		if app.RunningModeOrShared() == runtime.ModeShared {
			row.Host = host
		}
		if m != nil {
			row.ProcessStatus = m.Status
			row.CPU, row.Memory = m.CPU, m.Memory
			row.Restarts = m.Restarts
			// Only a running workload has been up for something. A stopped
			// entry may keep the start time of the process that is no longer
			// behind it — PM2's did, and reading it here printed a figure that
			// grew every ten seconds for a workload that had not existed for
			// days — so the word is read rather than the figure trusted.
			if runtime.WorkloadIsUp(m.Status) {
				row.Uptime = m.Uptime
			}
		} else {
			row.ProcessStatus = "missing"
		}
		rows = append(rows, row)
	}

	// Busiest first: an operator opening this page is usually looking for what
	// is eating the box. A row with no reading sorts below every row that has
	// one, ordered by name among its own kind — treating nil as 0.0 here would
	// put it at the bottom too, but as the claim "least busy" rather than as
	// the absence of an answer, and one measured app idling at 0.0% would then
	// be indistinguishable from every unmeasured one.
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].CPU, rows[j].CPU
		if (a == nil) != (b == nil) {
			return a != nil
		}
		if a != nil && *a != *b {
			return *a > *b
		}
		return rows[i].AppName < rows[j].AppName
	})

	resp := map[string]any{
		"node":      host,
		"count":     len(rows),
		"processes": rows,
		// Not rows, and not a filter this page could offer: there is nothing
		// to filter to, because neither of these has a workload to describe.
		// They are here so the table can account for the estate it is not
		// showing, in one sentence, with the app list as the place to go.
		"sleeping": asleep,
		"stopped":  halted,
	}
	if listErr != nil {
		// Said out loud rather than swallowed: a partial list that looks
		// complete is how an operator concludes an app is gone.
		log.Printf("processes: listing workloads: %v", listErr)
		resp["degraded"] = "One execution backend did not answer; this list may be incomplete."
	}
	writeJSON(w, http.StatusOK, resp)
}

// settingsOrDefault is the install's settings, or the defaults when they cannot
// be read — for a figure a page shows beside a list, where a failed read must
// not fail the list.
func settingsOrDefault(db *database.DB) database.PlatformSettings {
	if ps, err := db.GetPlatformSettings(); err == nil {
		return ps
	}
	return database.DefaultPlatformSettings()
}

// handleSystemAgent — GET /system/agent. The shared-mode agent's own figures:
// its descriptors, memory, goroutines, the logs it holds open and the
// workloads it holds by kind and state — what a leak in the agent shows first.
// system_admin only, like /system/processes. Absent (404) where the shared
// runtime is not the agent; 503 when the agent does not answer.
func (s *Server) handleSystemAgent(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	src, ok := s.rts.Shared().(interface {
		AgentStats(ctx context.Context) (agent.Stats, error)
	})
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"code": "not_found", "error": "this install's shared workloads are not run by the agent",
			"hint": "GET /api/v1/system/processes shows what this install runs instead"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st, err := src.AgentStats(ctx)
	if err != nil {
		msg := "the shared-mode agent did not answer: " + err.Error()
		if errors.Is(err, agent.ErrNotFound) {
			msg = "the running agent predates its figures; it reports them after its next re-exec"
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"code": "agent_unavailable", "error": msg, "retryable": true})
		return
	}
	writeJSON(w, http.StatusOK, st)
}
