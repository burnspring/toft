package database

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"toft/internal/sources"
)

// Which git repositories this install will deploy from: the stored half of
// internal/sources.
//
// The matching itself is over there, with no database import, because it is the
// piece most likely to be got subtly wrong and is worth testing on its own.
// What lives here is where the mode and the patterns come from, who changed
// them, and the three questions an administrator asks before changing them:
// what is in use, what would break, and which entries are still earning their
// place.

// AllowedGitSource is one entry on the list.
type AllowedGitSource struct {
	ID      string `json:"id"`
	Pattern string `json:"pattern"`
	// Note is why this entry exists, in the administrator's own words. The list
	// is expected to grow — an exact-repository entry is also the per-app
	// exemption mechanism — and a list nobody can prune grows forever.
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	// Apps is how many apps currently match this entry, filled in by
	// ListAllowedGitSourcesWithUsage. Zero from the plain listing.
	Apps int `json:"apps"`
}

// SourcePolicy is the resolved answer for the whole install, read as one
// document so no caller decides on a half-loaded view.
type SourcePolicy struct {
	sources.Policy
	// UploadsEnabled is the other half of "where may code come from". It is a
	// separate switch (see SettingSourceUploads) and it is checked at a
	// different point — creation only — so it is carried rather than folded in.
	UploadsEnabled bool `json:"uploads_enabled"`
}

// ResolveSourcePolicy answers "may this install deploy from that repository",
// in one place.
//
// One resolver, for the reason ResolveRetention and ResolveAttachment are
// each one function: two call sites that could disagree about whether a source
// is allowed is a bug nobody finds until it matters. Every enforcement point —
// the create route, the upload route, the builder, the auto-deploy poller and
// the gallery — calls this and sources.Policy.Allows, and nothing else.
//
// The pattern list is loaded only under ModeAllowlist. It is the only mode that
// consults it, and a create request on the overwhelmingly common "any" install
// should not pay for a second query to learn that.
func (db *DB) ResolveSourcePolicy() (SourcePolicy, error) {
	settings, err := db.GetPlatformSettings()
	if err != nil {
		return SourcePolicy{}, err
	}
	p := SourcePolicy{
		Policy:         sources.Policy{Mode: settings.GitSourcePolicy},
		UploadsEnabled: settings.SourceUploadsEnabled,
	}
	if p.Mode != sources.ModeAllowlist {
		return p, nil
	}

	entries, err := db.ListAllowedGitSources()
	if err != nil {
		return SourcePolicy{}, err
	}
	for _, e := range entries {
		pat, err := sources.ParsePattern(e.Pattern)
		if err != nil {
			// A stored pattern that no longer parses is dropped rather than
			// treated as a literal, and it can only mean a row edited outside
			// the API — the save path stores the canonical form. Dropping it
			// narrows the allowlist, which is the safe direction; the entry is
			// still visible on the admin page, where it can be corrected.
			continue
		}
		p.Patterns = append(p.Patterns, pat)
	}
	return p, nil
}

// ListAllowedGitSources returns the list in display order.
func (db *DB) ListAllowedGitSources() ([]AllowedGitSource, error) {
	rows, err := db.conn.Query(
		`SELECT id, pattern, note, created_at, COALESCE(created_by,'') FROM allowed_git_sources ORDER BY pattern`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AllowedGitSource{}
	for rows.Next() {
		var e AllowedGitSource
		if err := rows.Scan(&e.ID, &e.Pattern, &e.Note, &e.CreatedAt, &e.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListAllowedGitSourcesWithUsage is the listing plus, for each entry, how many
// apps currently match it.
//
// The counting is done in Go against every app's repository URL rather than in
// SQL, because the comparison is sources.Pattern.Matches and there is exactly
// one implementation of it. A LIKE that approximated the matcher would be a
// second opinion about what an entry covers, which is the thing this whole
// design refuses to have.
func (db *DB) ListAllowedGitSourcesWithUsage() ([]AllowedGitSource, error) {
	entries, err := db.ListAllowedGitSources()
	if err != nil {
		return nil, err
	}
	apps, err := db.ListAppSources()
	if err != nil {
		return nil, err
	}
	for i := range entries {
		pat, err := sources.ParsePattern(entries[i].Pattern)
		if err != nil {
			continue
		}
		for _, a := range apps {
			c, err := sources.Normalize(a.RepoURL)
			if err != nil {
				continue
			}
			if pat.Matches(c) {
				entries[i].Apps++
			}
		}
	}
	return entries, nil
}

// AppSource is one app reduced to the question "where does its code come
// from", for the counting, preview and seeding paths.
type AppSource struct {
	AppID   string `json:"app_id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	OwnerID string `json:"owner_id"`
	// OwnerName is filled in for display; the preview names owners because the
	// administrator about to break somebody's deploys should know whose.
	OwnerName string `json:"owner_name,omitempty"`
	RepoURL   string `json:"repo_url"`
}

// ListAppSources returns every git-sourced app on the install.
//
// Uploaded apps are excluded at the query, not filtered afterwards: they have
// no remote, they are never non-compliant on the git policy, and including them
// would make every count and every preview wrong in the same direction.
func (db *DB) ListAppSources() ([]AppSource, error) {
	rows, err := db.conn.Query(`
		SELECT apps.id, apps.slug, apps.name, COALESCE(apps.owner_user_id,''),
		       COALESCE(NULLIF(users.name, ''), NULLIF(users.login, ''), users.email, ''), apps.repo_url
		FROM apps
		LEFT JOIN users ON users.id = apps.owner_user_id
		WHERE apps.repo_url NOT LIKE 'upload://%'
		ORDER BY apps.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AppSource{}
	for rows.Next() {
		var a AppSource
		if err := rows.Scan(&a.AppID, &a.Slug, &a.Name, &a.OwnerID, &a.OwnerName, &a.RepoURL); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// NonCompliantApps returns the apps whose source would not pass a policy.
//
// It takes the policy rather than reading it, because its whole purpose is to
// answer "what would this do" before the administrator saves — the question is
// being asked about a policy that is not in force yet.
func (db *DB) NonCompliantApps(p sources.Policy) ([]AppSource, error) {
	apps, err := db.ListAppSources()
	if err != nil {
		return nil, err
	}
	out := []AppSource{}
	for _, a := range apps {
		if !p.Matches(a.RepoURL) {
			out = append(out, a)
		}
	}
	return out, nil
}

// ObservedSource is one host, organization or repository already in use, with
// how many apps deploy from it.
type ObservedSource struct {
	// Pattern is the canonical form, ready to be added to the list as-is.
	Pattern string `json:"pattern"`
	// Kind is "host", "org" or "repo" — how wide the suggestion is, so an
	// administrator can tell "everything on gitlab.internal" from "this one
	// repository" without parsing the string.
	Kind string `json:"kind"`
	Apps int    `json:"apps"`
}

// ObservedSources is what an install is already deploying from, offered so that
// a starting list is a few clicks rather than transcription.
//
// Three widths per app, because the right entry differs by install: a host for
// an intranet GitLab that serves nothing else, an organization for a shared
// github.com, an exact repository for the one third-party tool everybody uses.
// The administrator chooses; the platform does not guess.
func (db *DB) ObservedSources() ([]ObservedSource, error) {
	apps, err := db.ListAppSources()
	if err != nil {
		return nil, err
	}

	type key struct{ pattern, kind string }
	counts := map[key]int{}
	for _, a := range apps {
		c, err := sources.Normalize(a.RepoURL)
		if err != nil {
			// A URL this platform cannot read is no basis for a suggestion. It
			// still shows up in the impact preview as non-compliant, which is
			// where an administrator needs to see it.
			continue
		}
		counts[key{c.Host + "/*", "host"}]++
		if len(c.Segments) > 1 {
			counts[key{c.Host + "/" + c.Segments[0] + "/*", "org"}]++
		}
		if len(c.Segments) > 0 {
			counts[key{c.String(), "repo"}]++
		}
	}

	out := make([]ObservedSource, 0, len(counts))
	for k, n := range counts {
		out = append(out, ObservedSource{Pattern: k.pattern, Kind: k.kind, Apps: n})
	}
	// Widest first, then by how much is behind it, then alphabetically: the
	// entry that covers the most apps is usually the one an administrator wants
	// to click, and a stable order keeps the list from reshuffling on reload.
	rank := map[string]int{"host": 0, "org": 1, "repo": 2}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		if out[i].Apps != out[j].Apps {
			return out[i].Apps > out[j].Apps
		}
		return out[i].Pattern < out[j].Pattern
	})
	return out, nil
}

// SetSourcePolicy stores the mode, the upload switch and the pattern list, and
// audits the change as a **diff** in the same transaction.
//
// The diff is not a nicety. An administrator — or an attacker holding an
// administrator's session — can widen this list and then deploy; that is an
// accepted risk of keeping the list in the database with no configuration
// override, and `audit_log` recording exactly which pattern appeared is the
// compensating control. "Settings were changed" would not be one.
//
// Patterns are expected canonical: the caller parses them with
// sources.ParsePattern so that a bad entry is refused with a message naming the
// entry, which is not something this layer can phrase.
func (db *DB) SetSourcePolicy(mode string, uploads bool, entries []AllowedGitSource,
	actorID, requestID, ip, userAgent string) error {

	if !sources.ValidMode(mode) {
		return fmt.Errorf("unknown source policy mode %q", mode)
	}

	before, err := db.GetPlatformSettings()
	if err != nil {
		return err
	}
	existing, err := db.ListAllowedGitSources()
	if err != nil {
		return err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	audit := func(action, targetID, beforeJSON, afterJSON string) error {
		return writeAuditTx(tx, AuditEntry{
			ActorType: PrincipalTypeUser, ActorID: actorID,
			Action: action, TargetType: "platform", TargetID: targetID,
			Before: beforeJSON, After: afterJSON,
			RequestID: requestID, IP: ip, UserAgent: userAgent,
		})
	}

	if before.GitSourcePolicy != mode || before.SourceUploadsEnabled != uploads {
		if err := audit("source_policy_changed", "source_policy",
			jsonStr(map[string]any{"mode": before.GitSourcePolicy, "uploads_enabled": before.SourceUploadsEnabled}),
			jsonStr(map[string]any{"mode": mode, "uploads_enabled": uploads})); err != nil {
			return fmt.Errorf("auditing the source policy change: %w", err)
		}
	}

	for _, kv := range []struct{ key, val string }{
		{SettingGitSourcePolicy, mode},
		{SettingSourceUploads, boolText(uploads)},
	} {
		if _, err := tx.Exec(`
			INSERT INTO settings (key, value, updated_at, updated_by)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET
				value=excluded.value, updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
			kv.key, kv.val, now, nullIfEmpty(actorID)); err != nil {
			return err
		}
	}

	old := map[string]AllowedGitSource{}
	for _, e := range existing {
		old[e.Pattern] = e
	}
	kept := map[string]bool{}

	for _, e := range entries {
		e.Pattern = strings.TrimSpace(e.Pattern)
		e.Note = strings.TrimSpace(e.Note)
		if e.Pattern == "" || kept[e.Pattern] {
			continue
		}
		kept[e.Pattern] = true

		prev, existed := old[e.Pattern]
		if !existed {
			id := uuid.New().String()
			if _, err := tx.Exec(`
				INSERT INTO allowed_git_sources (id, pattern, note, created_at, created_by)
				VALUES (?, ?, ?, ?, ?)`, id, e.Pattern, e.Note, now, nullIfEmpty(actorID)); err != nil {
				return err
			}
			if err := audit("source_pattern_added", id, "",
				jsonStr(map[string]string{"pattern": e.Pattern, "note": e.Note})); err != nil {
				return fmt.Errorf("auditing an added pattern: %w", err)
			}
			continue
		}
		if prev.Note != e.Note {
			if _, err := tx.Exec(`UPDATE allowed_git_sources SET note=? WHERE id=?`, e.Note, prev.ID); err != nil {
				return err
			}
			// A note changes no decision, but an audit gap in a table whose
			// whole point is the diff is worse than a quiet row.
			if err := audit("source_pattern_updated", prev.ID,
				jsonStr(map[string]string{"pattern": prev.Pattern, "note": prev.Note}),
				jsonStr(map[string]string{"pattern": e.Pattern, "note": e.Note})); err != nil {
				return fmt.Errorf("auditing a pattern note: %w", err)
			}
		}
	}

	for _, e := range existing {
		if kept[e.Pattern] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM allowed_git_sources WHERE id=?`, e.ID); err != nil {
			return err
		}
		if err := audit("source_pattern_removed", e.ID,
			jsonStr(map[string]string{"pattern": e.Pattern, "note": e.Note}), ""); err != nil {
			return fmt.Errorf("auditing a removed pattern: %w", err)
		}
	}

	return tx.Commit()
}

// RecordSourceRefusal writes the one audit row this feature adds to a request
// path: who tried to deploy what, and from where.
//
// It is a write on a path that is already refusing, so it costs nothing anyone
// is waiting on, and it is exactly the record that makes "is anybody hitting
// this fence" answerable — which is the observation that decides whether the
// deliberate absence of an exception-request workflow was the right call.
func (db *DB) RecordSourceRefusal(actorID, repoURL, reason, appID string,
	requestID, ip, userAgent, apiKeyID string) error {

	target, targetID := "platform", "source_policy"
	if appID != "" {
		target, targetID = "app", appID
	}
	return db.WriteAuditRow(AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "deploy_source_refused", TargetType: target, TargetID: targetID,
		After:     jsonStr(map[string]string{"repo_url": repoURL, "reason": reason}),
		RequestID: requestID, IP: ip, UserAgent: userAgent, APIKeyID: apiKeyID,
	})
}

// SetAppRepoURL re-points an app at a different repository.
//
// It exists because repo_url was immutable, which meant an app blocked by a new
// policy could only be deleted and recreated — losing its environment
// variables, storage, schedules, grants and history. On an install that already
// has apps (which is every install that would want this feature) that made the
// switch unadoptable.
//
// last_seen_sha and last_seen_tree are cleared in the same statement: the next deploy is a full
// clone of a repository the platform has never seen, and carrying the old
// commit forward would make the poller believe it had already deployed it.
//
// Admin-only; nothing here enforces that, the handler does. An audit entry,
// when given, is written in the same transaction as the change.
func (db *DB) SetAppRepoURL(id, repoURL string, audit ...AuditEntry) error {
	return db.execAudited(audit,
		`UPDATE apps SET repo_url=?, last_seen_sha='', last_seen_tree='', updated_at=? WHERE id=?`,
		repoURL, time.Now().UTC(), id)
}
