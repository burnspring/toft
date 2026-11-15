package database

import (
	"fmt"
	"strings"
	"time"
)

// Place kinds, as integration_places.place_kind stores them.
const (
	PlaceTeam = "team"
	PlaceChat = "chat"
)

// IntegrationPlace is one far-side place an administrator pasted onto a
// connection and the far side verified (migration 00069).
type IntegrationPlace struct {
	ID             string    `json:"id"`
	ConnectionID   string    `json:"connection_id"`
	Kind           string    `json:"kind"`
	ConversationID string    `json:"conversation_id"`
	Name           string    `json:"name"`
	AddedBy        string    `json:"added_by,omitempty"`
	AddedAt        time.Time `json:"added_at"`
}

const placeColumns = `id, connection_id, place_kind, conversation_id, name,
	COALESCE(added_by, ''), added_at`

// ListPlaces is a connection's places, teams first, then by name.
func (db *DB) ListPlaces(connectionID string) ([]IntegrationPlace, error) {
	rows, err := db.conn.Query(`SELECT `+placeColumns+` FROM integration_places
		WHERE connection_id = ? ORDER BY place_kind DESC, name, id`, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IntegrationPlace{}
	for rows.Next() {
		var p IntegrationPlace
		if err := rows.Scan(&p.ID, &p.ConnectionID, &p.Kind, &p.ConversationID, &p.Name,
			&p.AddedBy, &p.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPlace is one place of one connection, or nil.
func (db *DB) GetPlace(connectionID, placeID string) (*IntegrationPlace, error) {
	places, err := db.ListPlaces(connectionID)
	if err != nil {
		return nil, err
	}
	for i := range places {
		if places[i].ID == placeID {
			return &places[i], nil
		}
	}
	return nil, nil
}

// AddPlace records a verified place and audits it in the same transaction.
// A conversation already on the connection is a duplicate-key error, which the
// handler turns into a 409 naming the place.
func (db *DB) AddPlace(p *IntegrationPlace, actorID string, meta RequestMeta) error {
	p.Name = strings.TrimSpace(p.Name)
	p.AddedAt = time.Now().UTC()
	p.AddedBy = actorID
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO integration_places
			(id, connection_id, place_kind, conversation_id, name, added_by, added_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.ConnectionID, p.Kind, p.ConversationID, p.Name, nullIfEmpty(actorID), p.AddedAt); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "integration.place.add",
		TargetType: "integration_connection", TargetID: p.ConnectionID,
		Before:    jsonStr(map[string]interface{}{"exists": false}),
		After:     jsonStr(placeAudit(p)),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing a place: %w", err)
	}
	return tx.Commit()
}

// DeletePlace removes one place and audits it. The caller has already refused
// a place an attachment or the default destination points into
// (PlaceInUse); this does not re-check, so the refusal can name who holds it.
func (db *DB) DeletePlace(p *IntegrationPlace, actorID string, meta RequestMeta) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM integration_places WHERE id = ? AND connection_id = ?`,
		p.ID, p.ConnectionID); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "integration.place.remove",
		TargetType: "integration_connection", TargetID: p.ConnectionID,
		Before:    jsonStr(placeAudit(p)),
		After:     jsonStr(map[string]interface{}{"deleted": true}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing a place removal: %w", err)
	}
	return tx.Commit()
}

// PlaceInUse names what points into a place: the slugs of apps whose
// attachment destination is in it, and whether the connection's own default
// destination is. destinationPrefix is the destination id prefix that means
// "in this place" — the transport owns the encoding, so it supplies it.
func (db *DB) PlaceInUse(connectionID, destinationPrefix string) (apps []string, isDefault bool, err error) {
	rows, err := db.conn.Query(`SELECT a.slug FROM app_integrations ai
		JOIN apps a ON a.id = ai.app_id
		WHERE ai.connection_id = ? AND starts_with(ai.destination_id, ?)
		ORDER BY a.slug`, connectionID, destinationPrefix)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, false, err
		}
		apps = append(apps, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	err = db.conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM integration_connections
		WHERE id = ? AND starts_with(default_destination_id, ?))`,
		connectionID, destinationPrefix).Scan(&isDefault)
	return apps, isDefault, err
}

func placeAudit(p *IntegrationPlace) map[string]interface{} {
	return map[string]interface{}{
		"place_id":        p.ID,
		"place_kind":      p.Kind,
		"conversation_id": p.ConversationID,
		"name":            p.Name,
	}
}
