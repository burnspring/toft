package database

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// An app's own hostname: one name per app, recorded by an administrator and
// served when its own switch is on. The validator is the specification — the
// list of refusals below is the whole of what a name may not be.

// ErrHostnameTaken is a name another app has recorded, served or not.
var ErrHostnameTaken = errors.New("that hostname is already another app's")

// ParseAppHostname normalises and validates a hostname for an app. reserved
// is the install's own hosts — the platform's and the apps host — and extra
// the hosts of every EXTRA_ORIGINS entry.
//
// A bare DNS name: lowercased, at most 253 characters, labels of [a-z0-9-]
// neither starting nor ending in '-', at least two of them. Refused: a scheme,
// a port, a path, a '*' label, an IP literal, any reserved or extra host —
// and **any name that is a parent or a child of a reserved host**. That last
// is the one that matters: an app on apps.toft.example.com could set
// Domain=toft.example.com cookies the platform reads, which is the cookie-jar
// reach the two-origin split exists to refuse.
func ParseAppHostname(raw string, reserved, extra []string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = strings.TrimSuffix(name, ".")
	switch {
	case name == "":
		return "", errors.New("a hostname cannot be empty")
	case strings.Contains(name, "://"):
		return "", errors.New("a hostname has no scheme: give the name alone, e.g. reports.example.com")
	case strings.ContainsAny(name, "/?#"):
		return "", errors.New("a hostname has no path: give the name alone, e.g. reports.example.com")
	case strings.Contains(name, ":"):
		return "", errors.New("a hostname has no port: apps are served on the apps listener's port")
	case strings.Contains(name, "*"):
		return "", errors.New("a hostname cannot be a wildcard: name one host")
	case net.ParseIP(name) != nil:
		return "", errors.New("a hostname cannot be an IP address")
	case len(name) > 253:
		return "", errors.New("a hostname is at most 253 characters")
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return "", errors.New("a hostname needs at least two labels, e.g. reports.example.com")
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return "", fmt.Errorf("%q is not a valid hostname: each label is 1 to 63 characters", name)
		}
		if l[0] == '-' || l[len(l)-1] == '-' {
			return "", fmt.Errorf("%q is not a valid hostname: a label cannot start or end with '-'", name)
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("%q is not a valid hostname: only letters, digits and '-'", name)
			}
		}
	}
	for _, h := range reserved {
		h = strings.ToLower(h)
		if h == "" {
			continue
		}
		switch {
		case name == h:
			return "", fmt.Errorf("%s is this install's own address", name)
		case strings.HasSuffix(name, "."+h):
			return "", fmt.Errorf("%s is under this install's own address %s, and could set cookies the platform reads", name, h)
		case strings.HasSuffix(h, "."+name):
			return "", fmt.Errorf("%s is a parent of this install's own address %s, and could set cookies the platform reads", name, h)
		}
	}
	for _, h := range extra {
		if strings.EqualFold(h, name) {
			return "", fmt.Errorf("%s is one of this install's trusted origins (EXTRA_ORIGINS)", name)
		}
	}
	return name, nil
}

// SetAppHostname records an app's hostname and whether it is served, with its
// audit row in the same transaction. Never through UpdateApp: the builder holds
// a stale App across a build, and a hostname reverted by an overlapping deploy
// would move an app back without anyone asking.
//
// name "" clears the record. ErrHostnameTaken when another app has it.
func (db *DB) SetAppHostname(id, fromName string, fromEnabled bool, name string, enabled bool, actorType, actorID string, meta RequestMeta) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE apps SET hostname=?, hostname_enabled=?, updated_at=? WHERE id=?`,
		nullIfEmpty(name), enabled, time.Now().UTC(), id); err != nil {
		if isUniqueViolation(err) {
			return ErrHostnameTaken
		}
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action:     "app.hostname_change",
		TargetType: "app", TargetID: id,
		Before:    jsonStr(map[string]any{"hostname": fromName, "hostname_enabled": fromEnabled}),
		After:     jsonStr(map[string]any{"hostname": name, "hostname_enabled": enabled}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// GetAppByServedHostname is the app a request's host names, or nil: one indexed
// read and no write, because it runs on every request to an app host.
//
// **Served, which is not the same as switched on.** A name switched off keeps
// answering while its app's release is still the one built for the root —
// mid-move off, the old address serves until the flip, exactly as the gateway
// path keeps serving mid-move on.
func (db *DB) GetAppByServedHostname(host string) (*App, error) {
	a := &App{}
	err := scanApp(db.conn.QueryRow(`SELECT `+appColumns+` FROM apps
		WHERE apps.hostname = ? AND (apps.hostname_enabled OR apps.release_base_path = '')`,
		strings.ToLower(host)), a)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// ListServedHostnames is every hostname an app answers on now, by the rule
// GetAppByServedHostname reads one with.
func (db *DB) ListServedHostnames() ([]string, error) {
	rows, err := db.conn.Query(`SELECT hostname FROM apps
		WHERE hostname IS NOT NULL AND (hostname_enabled OR release_base_path = '')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListEnabledHostnames is every app with a hostname switched on, for the
// startup check that a changed BASE_URL has not made one collide.
func (db *DB) ListEnabledHostnames() (map[string]string, error) {
	rows, err := db.conn.Query(`SELECT id, hostname FROM apps WHERE hostname_enabled`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}

// HostnameHolder is the id of the app that has recorded a hostname, served or
// not, or "" when none has.
func (db *DB) HostnameHolder(name string) (string, error) {
	var id string
	err := db.conn.QueryRow(`SELECT id FROM apps WHERE hostname = ?`, strings.ToLower(name)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// CountRecordedHostnames is how many apps have a hostname recorded, served or
// not — the question the platform's cookie choice turns on.
func (db *DB) CountRecordedHostnames() (int, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM apps WHERE hostname IS NOT NULL`).Scan(&n)
	return n, err
}
