package database

import "time"

// SetAppViewerIdentity switches this app's ability to learn who is looking at
// it, and writes the audit row in the same transaction.
//
// One transaction, unlike SetAppNeverSleep, and the difference is the point.
// The sleep exemption spends the operator's footprint and changes nothing about
// who may do what; this one hands an app the name, address and effective role of
// every person who visits it. That is an authorization-relevant mutation, and
// the standing rule for those is that the record of the change cannot go missing
// because a second write failed. SetAppExecutionMode is the shape.
//
// Administrator-only at the API. Nothing here enforces that — the handler does,
// as it does for every other setter in this package.
func (db *DB) SetAppViewerIdentity(id string, on bool, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE apps SET viewer_identity=?, updated_at=? WHERE id=?`,
		on, time.Now().UTC(), id); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "app.viewer_identity.change",
		TargetType: "app", TargetID: id,
		Before:    jsonStr(map[string]bool{"viewer_identity": !on}),
		After:     jsonStr(map[string]bool{"viewer_identity": on}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}
