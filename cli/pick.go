package main

import (
	"errors"
	"net/http"

	"toft/cli/internal/config"
	"toft/cli/internal/ui"
)

// pickApp is session.app() for the commands that may ask which app: status,
// logs, events, deps, restart/start/stop, redeploy, rollback and the job verbs.
//
// **It chooses which app, never which app this directory is.** Two callers do
// not come here: `toft deploy`, which uploads this directory, so that a list
// would be choosing which live app this tree overwrites; and `toft check`, for
// which no app is a meaning of its own (the before-create check). Nor is it
// inside session.app(), which has no client and is the resolver check reads.
//
// Only when input(e, g) — a person on every stream — and only when nothing
// named the app. The choice holds for this run, ends by naming `toft link`, and
// writes nothing. Escape, Ctrl-C, a closed stdin or an empty list return
// exactly the refusal session.app() returned, exit 2; a list that cannot be
// read returns that error, as `toft apps` would.
func pickApp(e *env, s *session) (config.Value, error) {
	v, err := s.app()
	if err == nil || !input(e, s.flags) {
		return v, err
	}
	var apps []appSummary
	if listErr := s.client.Do(http.MethodGet, "/api/v1/apps", nil, &apps); listErr != nil {
		return v, listErr
	}
	if len(apps) == 0 {
		return v, err
	}
	choices := make([]ui.Choice, len(apps))
	for i, a := range apps {
		choices[i] = ui.Choice{Label: a.Slug, Detail: e.out.Badge(a.Status)}
	}
	picked, pickErr := e.out.Pick(e.ctx, "Which app?", choices, e.input(), e.interrupt)
	if pickErr != nil {
		if errors.Is(pickErr, ui.ErrNotAnswered) {
			return v, err
		}
		return v, pickErr
	}
	e.out.Dim("For this run only — `toft link %s` makes it this directory's app.", picked.Label)
	return config.Value{Value: picked.Label, Origin: "picked"}, nil
}
