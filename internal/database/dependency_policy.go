package database

import (
	"fmt"
	"strings"
	"time"

	"toft/internal/deps"
)

// ResolveDependencyPolicy answers "which third-party packages will this
// install run", in one place.
//
// One resolver, for the reason ResolveRetention, ResolveAttachment and
// ResolveSourcePolicy are each one function: the builder, the bootstrap
// handler, the sweep and every surface in the UI must not be able to disagree
// about what the policy says.
//
// `licensed` comes from the caller because the entitlements source lives in
// the API layer and this package has no way to ask. It is passed in rather
// than defaulted to true so that forgetting it is a compile error rather than
// a silently ungated feature.
func (db *DB) ResolveDependencyPolicy(licensed bool, licenseReason string) (deps.Policy, error) {
	settings, err := db.GetPlatformSettings()
	if err != nil {
		return deps.Policy{}, err
	}
	p := deps.Policy{
		Mode:             settings.DependencyPolicy,
		ScanOnDeploy:     settings.DependencyScanOnDeploy,
		WatchEnabled:     settings.DependencyWatchEnabled,
		BlockMalware:     settings.DependencyBlockMalware,
		BlockSeverity:    settings.DependencyBlockSeverity,
		BlockOnlyFixable: settings.DependencyBlockOnlyFixable,
		WaiverMaxDays:    settings.DependencyWaiverMaxDays,
		ScanInterval:     time.Duration(settings.DependencyScanIntervalHrs) * time.Hour,
		HistoryKept:      settings.DependencyScanHistoryKept,
		LockOn:           settings.DependencyLockOn,
		FeedMaxAge:       time.Duration(settings.DependencyFeedMaxAgeHours) * time.Hour,
		Licensed:         licensed,
		LicenseReason:    licenseReason,
	}
	// While the feature is off there is nothing to read a corpus for, and a
	// fresh install should not pay a query to learn that.
	if p.Off() {
		return p, nil
	}
	if p.FeedAsOf, err = db.OldestFeedAge(); err != nil {
		return deps.Policy{}, err
	}
	if p.LoadedEcosystems, err = db.LoadedEcosystems(); err != nil {
		return deps.Policy{}, err
	}
	return p, nil
}

// The mirror credential: its own encrypted document, never a field inside
// PlatformSettings.
//
// The reason is the same one the SMTP password and the collector headers have.
// SetPlatformSettings audits the full before/after JSON of every key it owns,
// and a credential must never appear in an audit row; the cheapest way to
// guarantee that is for the credential to live somewhere that blob never
// touches.
//
// The *endpoint* is not here — it is an ordinary setting, audited in the
// clear, because "who re-pointed this install's advisory feed, and where to"
// is precisely the question that log exists to answer.
const SettingDependencyFeedCredential = "dependency_feed.credential"

// GetDependencyFeedCredential returns the stored ciphertext, or "" when a
// public feed needs none. The caller decrypts: only the API layer holds the
// encryptor.
func (db *DB) GetDependencyFeedCredential() (string, error) {
	row, err := db.GetSetting(SettingDependencyFeedCredential)
	if err != nil || row == nil {
		return "", err
	}
	return row.Value, nil
}

// SetDependencyFeedCredential stores the ciphertext and audits the change with
// two booleans in place of the value — whether one is set, and whether this
// write changed it — which is the same treatment the SMTP password gets.
//
// An empty ciphertext deletes the row rather than storing a blank. A secret
// retained for no purpose is a secret somebody eventually leaks.
func (db *DB) SetDependencyFeedCredential(ciphertext, actorID string, meta RequestMeta) error {
	before, err := db.GetDependencyFeedCredential()
	if err != nil {
		return err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if strings.TrimSpace(ciphertext) == "" {
		if _, err := tx.Exec(`DELETE FROM settings WHERE key=?`, SettingDependencyFeedCredential); err != nil {
			return err
		}
	} else if _, err := tx.Exec(`
		INSERT INTO settings (key, value, updated_at, updated_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			value=excluded.value, updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
		SettingDependencyFeedCredential, ciphertext, time.Now().UTC(), nullIfEmpty(actorID)); err != nil {
		return err
	}

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "dependency_feed_credential.update", TargetType: "platform", TargetID: "dependency_policy",
		Before: jsonStr(map[string]any{"credential_set": before != ""}),
		After: jsonStr(map[string]any{
			"credential_set":     strings.TrimSpace(ciphertext) != "",
			"credential_changed": ciphertext != before,
		}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing the feed credential change: %w", err)
	}
	return tx.Commit()
}
