package database

// SetDeploymentLogTimes stores a build log's line times (ADR §155), encoded by
// builder.EncodeLineTimes. A setter of its own, beside UpdateDeployment rather
// than in it, so no caller holding an older copy of the row can blank them.
func (db *DB) SetDeploymentLogTimes(deploymentID, encoded string) error {
	_, err := db.conn.Exec(`UPDATE deployments SET build_log_times = ? WHERE id = ?`, encoded, deploymentID)
	return err
}

// GetDeploymentLogTimes is the stored encoding, "" when the build recorded
// none — every build before migration 00090, and one whose log was written
// outside the builder (an upload refused before it was queued).
func (db *DB) GetDeploymentLogTimes(deploymentID string) (string, error) {
	var v string
	err := db.conn.QueryRow(`SELECT build_log_times FROM deployments WHERE id = ?`, deploymentID).Scan(&v)
	return v, err
}
