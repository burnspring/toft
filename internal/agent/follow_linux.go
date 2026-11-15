//go:build linux

package agent

import (
	"encoding/json"
	"net/http"
)

// handleFollow streams events as JSON lines: one workload's, or every
// workload's for GET /follow, until the caller goes away. A follower that falls
// a buffer behind is ended by the agent, and finds out by the stream ending.
func (a *Agent) handleFollow(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != "" && a.status(name).Name == "" {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no such workload"})
		return
	}
	flusher, _ := w.(http.Flusher)
	sub := a.subscribe(name)
	defer a.unsubscribe(sub)

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-sub.ch:
			if !ok {
				return
			}
			if err := enc.Encode(e); err != nil {
				return
			}
			// Drain what is already waiting before flushing, so a burst is one
			// write rather than one per line.
			for more := true; more; {
				select {
				case e, ok := <-sub.ch:
					if !ok {
						return
					}
					if err := enc.Encode(e); err != nil {
						return
					}
				default:
					more = false
				}
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
