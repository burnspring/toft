package database

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"
)

// Group membership written by a directory.
//
// ADR §51 refused this and set the terms; the migration's comment carries the
// revisit. What lives here is the half that runs on every sign-in: given the
// group claim a provider asserted, make this person's *directory* memberships
// match it, and touch nothing else.
//
// Three rules decide everything below, and each of them is a way of not
// destroying something:
//
//  1. **A manual row wins, permanently.** An administrator who added somebody by
//     hand asserted something the directory does not know. A sync that took
//     ownership of that row would erase the assertion at exactly the moment the
//     directory disagreed with it.
//  2. **Another provider's rows are as untouchable as a manual one.** Two IdPs
//     can both assert `engineering` and mean different rooms of people, and a
//     reconciler that narrowed only on `source = 'directory'` would delete B's
//     memberships every time somebody signed in through A.
//  3. **An absent claim reconciles nothing.** See DirectorySync.Asserted.

// DirectorySync is one sign-in's assertion about one person's groups.
type DirectorySync struct {
	UserID     string
	ProviderID string
	// Claimed is the raw claim values from the token. Values with no mapping
	// are ignored in silence — not logged as a near miss, not counted.
	Claimed []string
	// Asserted is whether the provider said anything about groups *at all*,
	// and it is not the same as len(Claimed) == 0.
	//
	// Microsoft Entra omits the groups claim entirely once a person is in more
	// groups than it will assert (150 for SAML, 200 for JWT) and sends an
	// overage indicator instead; Okta can be configured to omit it per
	// application. A reconciler reading absence as "removed from everything"
	// would strip every directory membership on the install, for everybody, on
	// a sign-in nobody changed anything to cause.
	//
	// So absent reconciles nothing and present-and-empty removes. That is a
	// real assertion and is honoured.
	Asserted bool
}

// DirectorySyncResult is what the reconciler did, for the caller to log and for
// the tests to assert on. Group ids, sorted, so a result is comparable.
type DirectorySyncResult struct {
	Added   []string
	Removed []string
	// Capped are the groups this person is claimed into and could not join,
	// because they already hold the maximum. Recorded rather than dropped: an
	// access control that silently did four fifths of its job is worse than one
	// that refused, and there is no administrator in front of this to be told.
	Capped []string
}

// Changed reports whether the reconciler wrote anything at all. A sign-in that
// changes nothing must not bump a token version — that would end every other
// session the person holds, on every sign-in, forever.
func (r DirectorySyncResult) Changed() bool {
	return len(r.Added) > 0 || len(r.Removed) > 0
}

// ReconcileDirectoryMemberships makes a person's directory memberships match
// what a provider just claimed.
//
// It runs in its own transaction, deliberately, rather than inside the one
// ResolveIdentity holds. The ten-group cap is a database trigger that RAISEs,
// and a RAISE aborts the transaction it fires in — so a membership problem
// inside the sign-in's transaction would take the identity write down with it,
// and a person in eleven groups would not fail to gain a membership, they would
// fail to sign in. The worst case here is instead somebody signing in with
// yesterday's groups, which their next sign-in fixes.
//
// The caller must commit this before minting a session. `mintSession` re-reads
// both the group list and the token version from the database, so that is the
// whole of the ordering requirement.
func (db *DB) ReconcileDirectoryMemberships(ctx context.Context, in DirectorySync, meta RequestMeta) (DirectorySyncResult, error) {
	var out DirectorySyncResult
	if !in.Asserted {
		// No transaction, no read, no row. An absent claim is not an empty one.
		return out, nil
	}
	if in.UserID == "" || in.ProviderID == "" {
		return out, nil
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	desired, err := mappedGroupIDs(tx, in.ProviderID, in.Claimed)
	if err != nil {
		return out, err
	}

	// What this person holds now, and how each row got there.
	type membership struct {
		source   string
		provider string
	}
	held := map[string]membership{}
	rows, err := tx.Query(`SELECT group_id, source, source_provider_id FROM group_members WHERE user_id=?`, in.UserID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id string
		var m membership
		if err := rows.Scan(&id, &m.source, &m.provider); err != nil {
			rows.Close()
			return out, err
		}
		held[id] = m
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()

	now := time.Now().UTC()

	// Removals first, and the order is load-bearing: a removal frees a slot
	// against the cap, so a person moved from one group to another under a full
	// allowance succeeds here and would have been refused the other way round.
	for id, m := range held {
		if m.source != sourceDirectory || m.provider != in.ProviderID {
			continue // manual, or another provider's — not ours to remove
		}
		if desired[id] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM group_members WHERE group_id=? AND user_id=?`, id, in.UserID); err != nil {
			return out, err
		}
		out.Removed = append(out.Removed, id)
	}

	// Additions, bounded by what the cap leaves. Counted after the removals
	// above, inside the same transaction, so the count is of what will actually
	// be there.
	slots, err := groupSlotsLeft(tx, in.UserID)
	if err != nil {
		return out, err
	}
	for _, id := range sortedKeys(desired) {
		if _, ok := held[id]; ok {
			// Already a member. If a person wrote that row it stays theirs —
			// rule 1 — and if this provider wrote it there is nothing to do.
			// Another provider's row also stands: two directories agreeing
			// about a membership is not a conflict.
			continue
		}
		if slots == 0 {
			out.Capped = append(out.Capped, id)
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO group_members (group_id, user_id, added_at, source, source_provider_id)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			id, in.UserID, now, sourceDirectory, in.ProviderID); err != nil {
			if isGroupCapViolation(err) {
				// The race the trigger exists to win: legal when counted, not
				// by the time it landed. Recorded like any other refusal.
				out.Capped = append(out.Capped, id)
				continue
			}
			return out, err
		}
		out.Added = append(out.Added, id)
		slots--
	}

	if !out.Changed() && len(out.Capped) == 0 {
		// Nothing happened, which is every sign-in after the first. Commit
		// rather than roll back — there is nothing to undo — and write no
		// token version, no audit row and no noise.
		return out, tx.Commit()
	}

	if out.Changed() {
		// Membership rides in the session JWT, so a change has to end the
		// sessions minted before it. The session about to be minted is not
		// among them: mintSession re-reads this column afterwards.
		if _, err := tx.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id=?`, in.UserID); err != nil {
			return out, err
		}
	}

	sort.Strings(out.Added)
	sort.Strings(out.Removed)
	sort.Strings(out.Capped)

	// The actor is the system and the *request* is still recorded: nobody
	// decided this, but it happened during somebody's sign-in and the request
	// is what ties it to that sign-in.
	//
	// The actor is the system for ADR §48's reason: this is a decision nobody
	// was present for, and naming the administrator who attached the mapping
	// months ago would be the audit trail lying about the one thing it records.
	// The provider is on every row, because "why am I in this group" has to be
	// answerable from the row alone.
	for _, id := range out.Added {
		if err := writeAuditTx(tx, AuditEntry{
			ActorType: PrincipalTypeSystem, ActorID: "",
			Action: "group.member_add", TargetType: "group", TargetID: id,
			After: jsonStr(map[string]any{
				"user_id": in.UserID, "source": sourceDirectory, "provider_id": in.ProviderID,
			}),
			OccurredAt: now,
			RequestID:  meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
		}); err != nil {
			return out, err
		}
	}
	for _, id := range out.Removed {
		if err := writeAuditTx(tx, AuditEntry{
			ActorType: PrincipalTypeSystem, ActorID: "",
			Action: "group.member_remove", TargetType: "group", TargetID: id,
			Before: jsonStr(map[string]any{
				"user_id": in.UserID, "source": sourceDirectory, "provider_id": in.ProviderID,
			}),
			OccurredAt: now,
			RequestID:  meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
		}); err != nil {
			return out, err
		}
	}
	if len(out.Capped) > 0 {
		// One row for the lot. The administrator's question is "which groups
		// did this person not get", and that is one answer, not five.
		if err := writeAuditTx(tx, AuditEntry{
			ActorType: PrincipalTypeSystem, ActorID: "",
			Action: "group.sync_capped", TargetType: "user", TargetID: in.UserID,
			After: jsonStr(map[string]any{
				"group_ids": out.Capped, "provider_id": in.ProviderID, "cap": GroupMembershipCap,
			}),
			OccurredAt: now,
			RequestID:  meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
		}); err != nil {
			return out, err
		}
	}

	return out, tx.Commit()
}

// sourceDirectory is the provenance a reconciler writes. The other value is
// 'manual', which is every row on every install that predates this.
const sourceDirectory = "directory"

// mappedGroupIDs resolves claim values to the groups an administrator attached
// them to, for one provider. A value nobody mapped resolves to nothing.
func mappedGroupIDs(tx *Tx, providerID string, claimed []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(claimed) == 0 {
		return out, nil
	}
	// One statement per value rather than an IN list: the claim is untrusted
	// input of unbounded length, the index is on (provider_id, claim_value),
	// and a person is in a handful of groups rather than a thousand.
	for _, v := range claimed {
		if v == "" {
			continue
		}
		rows, err := tx.Query(
			`SELECT group_id FROM group_claim_mappings WHERE provider_id=? AND claim_value=?`,
			providerID, v)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- The mappings themselves ---

// GroupClaimMapping is one claim value attached to one group.
type GroupClaimMapping struct {
	GroupID    string    `json:"group_id"`
	ProviderID string    `json:"provider_id"`
	ClaimValue string    `json:"claim_value"`
	CreatedAt  time.Time `json:"created_at"`
	CreatedBy  string    `json:"created_by,omitempty"`
}

// A mapping on a group that owns apps is refused with ErrGroupOwnsApps, which
// `groups.go` already declares for the two refusals ADR §51 built around
// ownership. This is the third, and it belongs to the same sentence: a group
// that owns apps is not one a directory may fill.
//
// ADR §51's objection is that mapping claims makes whoever administers the
// directory an owner of every app those groups own. The answer is that a mapped
// group may hold grants and may not *own* — so this refusal, and the one in the
// ownership transfer, are the two places that answer is enforced. The caller
// pairs the error with AppsOwnedByGroup, because a refusal an administrator
// cannot act on is only half a refusal.

// AppsOwnedByGroup names the apps a group owns, for a refusal to quote.
func (db *DB) AppsOwnedByGroup(groupID string) ([]string, error) {
	rows, err := db.conn.Query(`SELECT name FROM apps WHERE owner_group_id=? ORDER BY name`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GroupIsMapped reports whether a group draws membership from a directory.
//
// Read by the ownership-transfer refusal, which is the other end of the rule.
func (db *DB) GroupIsMapped(groupID string) (bool, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM group_claim_mappings WHERE group_id=?`, groupID).Scan(&n)
	return n > 0, err
}

// ListGroupClaimMappings returns one group's mappings.
func (db *DB) ListGroupClaimMappings(groupID string) ([]GroupClaimMapping, error) {
	rows, err := db.conn.Query(`
		SELECT group_id, provider_id, claim_value, created_at, COALESCE(created_by, '')
		FROM group_claim_mappings WHERE group_id=?
		ORDER BY provider_id, claim_value`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GroupClaimMapping{}
	for rows.Next() {
		var m GroupClaimMapping
		if err := rows.Scan(&m.GroupID, &m.ProviderID, &m.ClaimValue, &m.CreatedAt, &m.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddGroupClaimMapping attaches a claim value to a group.
//
// Refused if the group owns apps, before anything is written: attaching to an
// owning group is how a directory administrator would acquire ownership, which
// is the thing ADR §51 refused and the thing this design answers by drawing the
// line at the owner column rather than at the mapping.
func (db *DB) AddGroupClaimMapping(ctx context.Context, groupID, providerID, claimValue, actorID string, meta RequestMeta) error {
	if groupID == "" || providerID == "" || claimValue == "" {
		return errors.New("a group, a provider and a claim value are all required")
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var owned int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM apps WHERE owner_group_id=?`, groupID).Scan(&owned); err != nil {
		return err
	}
	if owned > 0 {
		return ErrGroupOwnsApps
	}

	if _, err := tx.Exec(`
		INSERT INTO group_claim_mappings (group_id, provider_id, claim_value, created_at, created_by)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		groupID, providerID, claimValue, time.Now().UTC(), nullIfEmpty(actorID)); err != nil {
		return err
	}

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "group.claim_map", TargetType: "group", TargetID: groupID,
		After:     jsonStr(map[string]string{"provider_id": providerID, "claim_value": claimValue}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveGroupClaimMapping detaches a claim value **and removes the memberships
// it wrote**, in one transaction.
//
// Not "stops asserting them", which is what the reconciler would do on its own:
// those rows would then be deleted one person at a time, over the following
// weeks, as each of them happened to sign in. A revocation that lands
// person-by-person over a month is not a revocation. Off means absent, and off
// means now.
//
// What it does not touch: a membership an administrator wrote by hand, and a
// membership another provider wrote. Both stand, for the reasons at the top of
// this file.
//
// The memberships go even if the same person is claimed by *another* mapping on
// the same group and provider — they will be re-added on that person's next
// sign-in, which is a moment of over-revocation rather than under, and that is
// the direction to fail in.
func (db *DB) RemoveGroupClaimMapping(ctx context.Context, groupID, providerID, claimValue, actorID string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		DELETE FROM group_claim_mappings
		WHERE group_id=? AND provider_id=? AND claim_value=?`,
		groupID, providerID, claimValue)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Nothing to detach, so nothing to revoke and nothing to record.
		return tx.Commit()
	}

	// Everyone this mapping's provider put in this group. Read before the
	// delete so the audit rows can name them.
	rows, err := tx.Query(`
		SELECT user_id FROM group_members
		WHERE group_id=? AND source=? AND source_provider_id=?`,
		groupID, sourceDirectory, providerID)
	if err != nil {
		return err
	}
	var affected []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		affected = append(affected, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// Only if this provider has no other mapping on this group. Otherwise the
	// remaining mapping still claims these people and removing them would be a
	// revocation nobody asked for.
	var remaining int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM group_claim_mappings WHERE group_id=? AND provider_id=?`,
		groupID, providerID).Scan(&remaining); err != nil {
		return err
	}
	now := time.Now().UTC()
	if remaining == 0 && len(affected) > 0 {
		if _, err := tx.Exec(`
			DELETE FROM group_members
			WHERE group_id=? AND source=? AND source_provider_id=?`,
			groupID, sourceDirectory, providerID); err != nil {
			return err
		}
		for _, userID := range affected {
			if _, err := tx.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id=?`, userID); err != nil {
				return err
			}
			if err := writeAuditTx(tx, AuditEntry{
				ActorType: PrincipalTypeUser, ActorID: actorID,
				Action: "group.member_remove", TargetType: "group", TargetID: groupID,
				Before: jsonStr(map[string]any{
					"user_id": userID, "source": sourceDirectory, "provider_id": providerID,
				}),
				After:      jsonStr(map[string]any{"reason": "claim mapping removed"}),
				OccurredAt: now,
				RequestID:  meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
			}); err != nil {
				return err
			}
		}
	}

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "group.claim_unmap", TargetType: "group", TargetID: groupID,
		Before: jsonStr(map[string]string{"provider_id": providerID, "claim_value": claimValue}),
		After: jsonStr(map[string]any{
			"memberships_removed": len(affected), "provider_still_mapped": remaining > 0,
		}),
		OccurredAt: now,
		RequestID:  meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// DirectoryCoverage is one provider's mapping situation: how many claim values
// are attached to groups, and how many memberships that provider has actually
// written.
type DirectoryCoverage struct {
	ProviderID  string
	Mappings    int
	Memberships int
}

// DirectoryMappingCoverage reports, per provider, what has been mapped and what
// has arrived.
//
// It is the honest half of an unanswerable question. What an administrator wants
// to know is "is my IdP asserting the claim", and this install cannot ask the
// IdP — it can only notice that mappings exist and nothing has ever come through
// them. That covers the two cases with the same remedy: a claim the provider was
// never configured to send, and Entra's overage, where a person in too many
// groups gets no claim at all. Both look identical from here and both are fixed
// by going and looking at the IdP.
//
// What it does not catch is a claim that worked and stopped, which would need a
// timestamp written on every sign-in — a write on the sign-in path to report a
// condition nobody is watching for.
func (db *DB) DirectoryMappingCoverage() ([]DirectoryCoverage, error) {
	rows, err := db.conn.Query(`
		SELECT m.provider_id,
		       COUNT(*),
		       COALESCE((SELECT COUNT(*) FROM group_members gm
		                 WHERE gm.source = 'directory' AND gm.source_provider_id = m.provider_id), 0)
		FROM group_claim_mappings m
		GROUP BY m.provider_id
		ORDER BY m.provider_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DirectoryCoverage
	for rows.Next() {
		var c DirectoryCoverage
		if err := rows.Scan(&c.ProviderID, &c.Mappings, &c.Memberships); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- Single-membership writes, for a caller that is not reconciling ---

// AddDirectoryMembership puts one person in one group on a directory's word.
//
// The reconciler works from a whole claim and computes a difference; a SCIM
// group PATCH arrives as one add or one remove and has no set to compare
// against. Both need the same rules — a manual row wins, another provider's row
// is untouchable, the cap is counted before the trigger can fire, the audit
// actor is not a person — so the rules live here and the reconciler is one
// caller rather than the implementation.
//
// Returns false when nothing was written: already a member (by any provenance),
// or the cap is full. The second is distinguishable through capped.
func (db *DB) AddDirectoryMembership(ctx context.Context, groupID, userID, providerID string, audit AuditEntry) (added bool, capped bool, err error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()

	var existing string
	err = tx.QueryRow(`SELECT source FROM group_members WHERE group_id=? AND user_id=?`, groupID, userID).Scan(&existing)
	if err == nil {
		// Already a member. A manual row stays the administrator's and a
		// directory row is already what this would write.
		return false, false, tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, false, err
	}

	slots, err := groupSlotsLeft(tx, userID)
	if err != nil {
		return false, false, err
	}
	if slots == 0 {
		return false, true, tx.Commit()
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(`
		INSERT INTO group_members (group_id, user_id, added_at, source, source_provider_id)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		groupID, userID, now, sourceDirectory, providerID); err != nil {
		if isGroupCapViolation(err) {
			return false, true, nil
		}
		return false, false, err
	}
	if _, err := tx.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id=?`, userID); err != nil {
		return false, false, err
	}

	audit.Action = "group.member_add"
	audit.TargetType = "group"
	audit.TargetID = groupID
	audit.After = jsonStr(map[string]any{
		"user_id": userID, "source": sourceDirectory, "provider_id": providerID,
	})
	audit.OccurredAt = now
	if err := writeAuditTx(tx, audit); err != nil {
		return false, false, err
	}
	return true, false, tx.Commit()
}

// RemoveDirectoryMembership takes one person out of one group on a directory's
// word, and **only if that directory put them there**.
//
// A membership an administrator wrote by hand is not this caller's to remove,
// and neither is one another provider wrote. Both are left in place and the
// result is false — which is not an error: the directory asked for a state that
// is already true from its own point of view, since it never put that row there.
func (db *DB) RemoveDirectoryMembership(ctx context.Context, groupID, userID, providerID string, audit AuditEntry) (bool, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		DELETE FROM group_members
		WHERE group_id=? AND user_id=? AND source=? AND source_provider_id=?`,
		groupID, userID, sourceDirectory, providerID)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n == 0 {
		return false, tx.Commit()
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id=?`, userID); err != nil {
		return false, err
	}
	audit.Action = "group.member_remove"
	audit.TargetType = "group"
	audit.TargetID = groupID
	audit.Before = jsonStr(map[string]any{
		"user_id": userID, "source": sourceDirectory, "provider_id": providerID,
	})
	audit.OccurredAt = now
	if err := writeAuditTx(tx, audit); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// GroupByClaimValue resolves a directory's name for a group to a local one.
//
// The same `group_claim_mappings` row a sign-in claim resolves through, which is
// the point: an administrator maps a directory group to a local group once, and
// both the token claim and the SCIM feed find it there. A name nobody mapped
// resolves to nothing and the caller ignores it in silence.
func (db *DB) GroupByClaimValue(providerID, claimValue string) (string, error) {
	var id string
	err := db.conn.QueryRow(
		`SELECT group_id FROM group_claim_mappings WHERE provider_id=? AND claim_value=? LIMIT 1`,
		providerID, claimValue).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
