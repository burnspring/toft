package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/runtime"
)

const (
	consumptionDefaultDays = 7
	// consumptionMaxDays matches the daily roll-up's retention, so a caller
	// cannot ask for a window the data does not cover and read the empty part
	// as "this app was idle".
	consumptionMaxDays = 400
)

// consumptionResponse renders one row. CPU-seconds and byte-hours are the
// units, not money: converting to cost needs operator-owned pricing and
// belongs in a reporting feature, not here.
type consumptionResponse struct {
	AppID    string `json:"app_id"`
	JobRunID string `json:"job_run_id,omitempty"`

	CPUSeconds        float64 `json:"cpu_seconds"`
	AverageMemoryMB   float64 `json:"average_memory_mb"`
	PeakMemoryMB      float64 `json:"peak_memory_mb"`
	MemoryByteHours   float64 `json:"memory_byte_hours"`
	ObservedSeconds   float64 `json:"observed_seconds"`
	Incomplete        bool    `json:"incomplete"`
	IncompleteMessage string  `json:"incomplete_message,omitempty"`
}

func renderConsumption(c database.Consumption) consumptionResponse {
	const mib = 1 << 20
	out := consumptionResponse{
		AppID:           c.AppID,
		JobRunID:        c.JobRunID,
		CPUSeconds:      c.CPUSeconds,
		AverageMemoryMB: c.AverageMemoryBytes() / mib,
		PeakMemoryMB:    float64(c.PeakMemoryBytes) / mib,
		MemoryByteHours: c.MemoryByteSeconds / 3600,
		ObservedSeconds: c.ObservedSeconds,
		Incomplete:      c.HasGap,
	}
	if c.HasGap {
		// A gap is surfaced rather than smoothed over. An operator sizing a
		// host or charging a team needs to know the figure is a floor.
		out.IncompleteMessage = "part of this period was not observed — the figures are a lower bound"
	}
	return out
}

// consumptionPeriod reads the ?days= window, defaulting to a week.
func consumptionPeriod(r *http.Request) (from, to time.Time, err error) {
	days := consumptionDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 1 || n > consumptionMaxDays {
			return time.Time{}, time.Time{}, errBadPeriod
		}
		days = n
	}
	to = time.Now().UTC().Add(time.Minute) // include the interval in progress
	from = to.AddDate(0, 0, -days)
	return from, to, nil
}

type periodError struct{}

func (periodError) Error() string { return "days must be between 1 and 400" }

var errBadPeriod = periodError{}

// handleAppConsumption reports what one app actually consumed.
//
// Answers "am I sized correctly" for an owner and "what is this costing" for
// an admin — questions request counts cannot answer. Authorization is the
// app's own read rule: consumption is no more sensitive than the app's
// existence, and no less.
func (s *Server) handleAppConsumption(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	if d := auth.Authorize(p, auth.ActionRead, objectFromApp(app), s.db); !d.Allow {
		writeAuthzError(w, r, d, false)
		return
	}

	from, to, err := consumptionPeriod(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass days as a whole number from 1 to 400, like ?days=30"})
		return
	}

	// The scope is passed even though the app was authorized above: the
	// visibility rule stays in one place, and a handler that authorized
	// against one object and then queried without the predicate is exactly the
	// shape of an IDOR.
	scope := scopeFor(p)
	total, err := s.db.AppConsumption(scope, appID, from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	runs, err := s.db.JobRunConsumption(scope, appID, from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// One read of the envelope for the whole payload, so the three figures on
	// this page describe one instant rather than three.
	env := s.envelope()

	byRun := make([]consumptionResponse, 0, len(runs))
	for _, c := range runs {
		byRun = append(byRun, renderConsumption(c))
	}

	res := env.ResourcesForApp(app.MemoryLimitMB, app.CPULimitMillicores)
	mode := app.RunningModeOrShared()
	held := s.containment(r.Context())
	out := map[string]any{
		"from":     from,
		"to":       to,
		"total":    renderConsumption(total),
		"job_runs": byRun,
		// The app's configured limits, so an owner can read consumption
		// against them rather than against nothing — resolved the way a start
		// resolves them, because a page reporting a different number from the
		// one a workload was launched with is worse than a page reporting
		// none — each with where it came from and how it is held.
		"memory_limit_mb": res.MemoryMB.Value,
		"memory_limit": map[string]any{"value_mb": res.MemoryMB.Value, "source": res.MemoryMB.Source,
			"enforcement": runtime.Enforcement(mode, held, runtime.DialMemory)},
		"cpu_limit": map[string]any{"value_millicores": res.CPUMillicores.Value, "source": res.CPUMillicores.Source,
			"enforcement": cpuEnforcement(mode, held, res.CPUMillicores.Value)},
	}
	// The scheduler's reservation, and only where there is a scheduler. A
	// shared workload runs beside the platform under the host's own scheduler
	// and reserves nothing — absent rather than zero, for the reason a
	// backend's missing metric is absent. The mode read is the *running* one:
	// an app promoted to isolated and not yet redeployed is still a process on
	// this host.
	if mode == runtime.ModeIsolated {
		out["cpu_request_millicores"] = env.AppCPURequestMillicores
	}
	// The last hour held at the CPU limit, the figure app_cpu_throttled is
	// judged on — absent where nothing measures it, never zero.
	if t, err := s.db.AppCPUThrottling(app.ID, time.Now().Add(-time.Hour)); err == nil && t.Periods > 0 {
		out["cpu_throttled_last_hour"] = map[string]any{
			"periods": t.Periods, "throttled_periods": t.ThrottledPeriods,
			"throttled_seconds": t.ThrottledSeconds, "measured_seconds": t.MeasuredSeconds,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// cpuEnforcement is the CPU limit's word, and not_enforced for no limit at
// all: an install that uncapped CPU holds nothing, whatever its tier.
func cpuEnforcement(mode runtime.Mode, held runtime.Containment, millicores int) string {
	if millicores <= 0 {
		return runtime.NotEnforced
	}
	return runtime.Enforcement(mode, held, runtime.DialCPU)
}

// handleConsumptionSummary ranks apps by what they consumed, which is the
// question an operator managing a shared footprint actually has: which app is
// starving the others, and which deserves its own resource envelope.
//
// It narrows through the same readable-apps predicate as every other listing,
// so it cannot be used to enumerate private apps by their cost.
func (s *Server) handleConsumptionSummary(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}

	from, to, err := consumptionPeriod(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass days as a whole number from 1 to 400, like ?days=30"})
		return
	}

	rows, err := s.db.ListConsumption(scopeFor(p), from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	out := make([]consumptionResponse, 0, len(rows))
	for _, c := range rows {
		out = append(out, renderConsumption(c))
	}

	// Host capacity gives the figures a denominator: per-app consumption
	// against the fixed capacity of the shared footprint is what tells an
	// admin how close it is to exhaustion. Admins only, matching the system
	// health endpoint — host metrics are infrastructure detail.
	var capacity map[string]interface{}
	if p.IsAdmin() {
		if m, err := s.rts.SystemMetrics(); err == nil && m != nil {
			capacity = map[string]interface{}{
				"memory_total_mb": float64(m.MemoryTotal) / (1 << 20),
				"memory_used_mb":  float64(m.MemoryUsed) / (1 << 20),
				"cpu_count":       m.CPUCount,
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"from":          from,
		"to":            to,
		"apps":          out,
		"host_capacity": capacity,
	})
}
