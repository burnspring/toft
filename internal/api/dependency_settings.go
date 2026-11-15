package api

import (
	"log"
	"strconv"
	"strings"

	"toft/internal/advisory"
	"toft/internal/database"
	"toft/internal/safefetch"
)

// The dependency policy's half of Admin → Settings.
//
// Four cross-field rules live here, and each one exists to stop a
// configuration that *reads* as a control and is not. Every refusal names both
// halves rather than clamping one, because an administrator who typed
// something meant it, and a silently adjusted setting is how a page comes to
// disagree with the person who set it.

// applyDependencySettings validates and applies the thirteen fields, returning
// a refusal or "".
func (s *Server) applyDependencySettings(req *updateSettingsRequest,
	settings *database.PlatformSettings, warning *string) string {

	policyWasOff := settings.DependencyPolicy == database.DependencyPolicyOff
	lockWarned := false

	if req.DependencyPolicy != nil {
		mode := strings.TrimSpace(*req.DependencyPolicy)
		if !database.ValidDependencyPolicy(mode) {
			return "dependency_policy must be one of " + database.DependencyPolicyOff + ", " +
				database.DependencyPolicyWarn + ", " + database.DependencyPolicyBlock + "."
		}
		// Said rather than assumed, and it is the honest recommendation: an
		// administrator switching straight to `block` across forty apps has no
		// idea what it breaks.
		if mode == database.DependencyPolicyBlock && settings.DependencyPolicy == database.DependencyPolicyOff {
			*warning = "Going straight from off to blocking means the first thing anyone learns " +
				"about this control is a deploy that failed. Preview what it would refuse first, " +
				"or run in warn for a fortnight — warn records and shows everything and refuses only a " +
				"malicious package."
		}
		settings.DependencyPolicy = mode
	}

	if req.DependencyBlockSeverity != nil {
		level := strings.TrimSpace(*req.DependencyBlockSeverity)
		if !database.ValidDependencySeverityThreshold(level) {
			return "dependency_block_severity must be one of " + advisory.SeverityCritical + ", " +
				advisory.SeverityHigh + ", " + advisory.SeverityModerate + ", " + advisory.SeverityLow +
				", or " + database.DependencySeverityOff + " to stop blocking on severity at all. " +
				"An advisory nobody scored never meets a threshold, whichever one is set."
		}
		settings.DependencyBlockSeverity = level
	}

	if req.DependencyScanOnDeploy != nil {
		settings.DependencyScanOnDeploy = *req.DependencyScanOnDeploy
	}
	if req.DependencyWatchEnabled != nil {
		// Turning the watch off leaves the figures where they are, and the
		// surfaces have to say so rather than showing a number that quietly
		// stops moving.
		if !*req.DependencyWatchEnabled && settings.DependencyWatchEnabled {
			*warning = "Findings on every app stop being re-checked from now on. They keep their " +
				"date and the tabs say so, and Check again still works — the question stays " +
				"answerable, just only when somebody asks."
		}
		settings.DependencyWatchEnabled = *req.DependencyWatchEnabled
	}
	if req.DependencyBlockMalware != nil {
		settings.DependencyBlockMalware = *req.DependencyBlockMalware
	}
	if req.DependencyBlockOnlyFixable != nil {
		settings.DependencyBlockOnlyFixable = *req.DependencyBlockOnlyFixable
	}

	if req.DependencyLockOn != nil {
		level := strings.TrimSpace(*req.DependencyLockOn)
		if !database.ValidDependencyLockOn(level) {
			return "dependency_lock_on must be one of " + database.DependencyLockMalware + ", " +
				database.DependencyLockCritical + ", " + database.DependencyLockHigh + ", or " +
				database.DependencyLockOff + ". There is no moderate or low: a moderate " +
				"transitive advisory with no fix is on nearly every Node app, and a value that " +
				"would take an estate off the air over one is not one to be able to pick by " +
				"accident."
		}
		// **The one setting on this page that can take a running app off the
		// air, so the warning carries §39's argument rather than a softened
		// version of it.** Said on the transition to each value, because the
		// two halves of it are different arguments and an administrator moving
		// from malware to critical is not being told the same thing as one
		// switching it on.
		if level != settings.DependencyLockOn {
			lockWarned = true
			switch level {
			case database.DependencyLockMalware:
				*warning = "From now on, an app whose serving release installs a package " +
					"reported as malicious is locked and stopped automatically, with nobody " +
					"present. That is the one case this install reverses its own rule for: a " +
					"malicious package is running on this machine with the app's environment, " +
					"and every hour it keeps serving is the harm. An unlocked app whose finding " +
					"still matches is locked again on the next pass — a waiver is how to mean it."
			case database.DependencyLockCritical, database.DependencyLockHigh:
				*warning = "This takes running apps off the air over a third party's advisory " +
					"data, with nobody present, possibly at 3am. A bad or over-broad advisory " +
					"is why this platform's default is never to stop a serving app, and that " +
					"reasoning is unchanged for vulnerabilities — malware is the one class it " +
					"does not hold for. Preview what it would catch first, and remember that " +
					"the way back for one app is a waiver rather than an unlock."
			case database.DependencyLockOff:
				*warning = "Nothing will be locked automatically from now on. Apps this policy " +
					"has already locked stay locked: no verdict withdraws anything, and " +
					"unlocking one is a decision somebody makes."
			}
		}
		settings.DependencyLockOn = level
	}

	if req.DependencyFeedEndpoint != nil {
		endpoint := strings.TrimSpace(*req.DependencyFeedEndpoint)
		if endpoint == "" {
			endpoint = advisory.DefaultEndpoint
		}
		// A URL this server fetches on its own initiative is server-side input
		// whoever typed it. https only, and no credentials in the authority.
		if err := safefetch.CheckURL(endpoint); err != nil {
			return "dependency_feed_endpoint " + err.Error() + ". This is an address this install " +
				"downloads advisory data from; leave it empty for the default."
		}
		settings.DependencyFeedEndpoint = endpoint
	}

	for _, f := range []struct {
		val    *int
		field  *int
		name   string
		lo, hi int
		tail   string
	}{
		{req.DependencyFeedIntervalHrs, &settings.DependencyFeedIntervalHrs,
			"dependency_feed_interval_hours",
			database.MinDependencyFeedIntervalHours, database.MaxDependencyFeedIntervalHours,
			"This is how often the advisory corpus is refreshed. Nothing about this install's " +
				"apps is sent anywhere — the snapshots are downloaded and matched here."},
		{req.DependencyFeedMaxAgeHours, &settings.DependencyFeedMaxAgeHours,
			"dependency_feed_max_age_hours",
			database.MinDependencyFeedMaxAgeHours, database.MaxDependencyFeedMaxAgeHours,
			"Past this age the corpus is called stale on every surface. Staleness warns and " +
				"never blocks: an outage at the feed's host must not become an install where " +
				"nobody can deploy."},
		{req.DependencyWaiverMaxDays, &settings.DependencyWaiverMaxDays,
			"dependency_waiver_max_days",
			database.MinDependencyWaiverMaxDays, database.MaxDependencyWaiverMaxDays,
			"This caps how long an accepted risk may run. 0 turns waivers off entirely, which " +
				"changes what a refused deploy tells the developer — with no waiver to ask for, " +
				"the threshold is the only thing that can move."},
		{req.DependencyScanIntervalHrs, &settings.DependencyScanIntervalHrs,
			"dependency_scan_interval_hours",
			database.MinDependencyScanIntervalHours, database.MaxDependencyScanIntervalHours,
			"This is how often every serving release is re-judged against the advisory data " +
				"already held here. It is not the feed's interval: that one governs an " +
				"outbound download, and this one is a local join. Sweeping more often than the " +
				"feed refreshes still finds things, because a redeploy changes what is serving " +
				"under a corpus that has not moved."},
		{req.DependencyScanHistoryKept, &settings.DependencyScanHistoryKept,
			"dependency_scan_history_kept",
			database.MinDependencyScanHistoryKept, database.MaxDependencyScanHistoryKept,
			"This is how many readings each app keeps — a count, not a number of days, because " +
				"an install sweeping hourly and one sweeping weekly want the same number of " +
				"readings rather than the same span. Lowering it drops the oldest rows on the " +
				"next pass, and a reading cannot be re-derived: the advisory data it was taken " +
				"against is replaced wholesale every sync."},
	} {
		if f.val == nil {
			continue
		}
		if *f.val < f.lo || *f.val > f.hi {
			return f.name + " must be between " + strconv.Itoa(f.lo) + " and " +
				strconv.Itoa(f.hi) + ". " + f.tail
		}
		*f.field = *f.val
	}

	// **Blocking with nothing to check against is refused**, not clamped. The
	// gate would have nothing to evaluate and would pass every deploy while
	// the admin page said `block` — which is the one way these switches can do
	// harm, and the same shape as the startup refusal below and half a
	// telemetry pipeline.
	if settings.DependencyPolicy == database.DependencyPolicyBlock && !settings.DependencyScanOnDeploy {
		return "dependency_policy cannot be 'block' while dependency_scan_on_deploy is off. " +
			"The gate would have nothing to evaluate and every deploy would pass while this page " +
			"said it was blocking. Turn the deploy scan back on, or set the policy to 'warn'."
	}

	// **BUILD_ALLOW_SCRIPTS and blocking are mutually exclusive.** With
	// lifecycle scripts enabled, `npm ci` executes arbitrary code from every
	// package in the tree after the gate has approved it by name, so the gate
	// is not a control. Refused here as well as at startup, because an
	// administrator who saves this would otherwise find out at the next
	// restart — by the binary refusing to come up.
	if settings.DependencyPolicy == database.DependencyPolicyBlock && s.cfg.AllowBuildScripts {
		return "dependency_policy cannot be 'block' while BUILD_ALLOW_SCRIPTS is set on this " +
			"install. With dependency lifecycle scripts enabled, `npm ci` runs arbitrary code " +
			"from every package in the tree after the gate has approved it by name — there is no " +
			"useful meaning to blocking rogue packages and also running their install scripts. " +
			"Unset BUILD_ALLOW_SCRIPTS and restart, or set the policy to 'warn'."
	}

	// **Setting a lock threshold with the watch off is a control that never
	// fires**, so that save is refused rather than clamped and names both
	// halves: the sweep is what takes the reading, and a page reading `malware`
	// over a watch that is not running is the same lie as `block` over a feed
	// that has never synced.
	//
	// The other direction is deliberately not refused. Turning the *watch* off
	// while a threshold is set only ever takes a consequence away, which is
	// safe and is somebody's decision to make; it is warned about instead,
	// because the one thing that must not happen is for it to be silent.
	if req.DependencyLockOn != nil &&
		settings.DependencyLockOn != database.DependencyLockOff && !settings.DependencyWatchEnabled {
		return "dependency_lock_on cannot be set while dependency_watch_enabled is off. " +
			"Locking happens on a scheduled pass, and with the watch off there are no passes — " +
			"the setting would read as a control and never fire. Turn the watch back on in the " +
			"same save, or set dependency_lock_on to 'off'."
	}
	if req.DependencyWatchEnabled != nil && !*req.DependencyWatchEnabled &&
		settings.DependencyLockOn != database.DependencyLockOff {
		*warning = "Findings on every app stop being re-checked from now on, and with no passes " +
			"nothing is locked automatically either — the lock threshold is still " +
			settings.DependencyLockOn + " and will apply again if you turn the watch back on. " +
			"Apps this policy has already locked stay locked."
	}

	// **Switching the policy on is what arms the lock**, because the threshold
	// defaults to malware and was inert until now. An administrator who never
	// opened that select must still hear, in this save, that a running app can
	// now be stopped with nobody present.
	if policyWasOff && settings.DependencyPolicy != database.DependencyPolicyOff && !lockWarned &&
		settings.DependencyLockOn != database.DependencyLockOff && settings.DependencyWatchEnabled {
		armed := "Automatic locking is now live at " + settings.DependencyLockOn + ": a running app " +
			"whose release installs a package at that level is locked and stopped by a scheduled " +
			"pass, with nobody present, and an unlocked app is locked again while it still matches — a waiver is how " +
			"to mean it. Set the lock to off to never stop a running app."
		if *warning == "" {
			*warning = armed
		} else {
			*warning += " " + armed
		}
	}

	return ""
}

// resumesDependencyWatch reports whether this save switches the watch back
// on. The re-baseline that follows is a write, and it runs after the settings
// write — see handleUpdateSettings — not here among the validations: a 400 for
// an unrelated field must not leave every app's baseline cleared.
func resumesDependencyWatch(req *updateSettingsRequest, before, after *database.PlatformSettings) bool {
	return req.DependencyWatchEnabled != nil && *req.DependencyWatchEnabled &&
		after.DependencyPolicy != database.DependencyPolicyOff &&
		!before.DependencyWatchEnabled
}

// rebaselineOnResume clears every app's baseline when the watch has just been
// switched back on, so notification begins again from the next genuine change
// rather than from a month of accumulated news fired at forty owners at once.
// Recorded here because this is the transition, and the sweep cannot see that
// it happened. Best-effort after a committed write: the setting is saved, and
// a failed clear is logged rather than reported as a failed save.
func (s *Server) rebaselineOnResume() {
	if err := s.db.ClearDependencyBaselines(); err != nil {
		log.Printf("settings: re-baselining after re-enabling the dependency watch: %v", err)
	}
}
