package database

import "testing"

func TestRebind(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "no placeholders is returned unchanged",
			query: `SELECT id FROM apps`,
			want:  `SELECT id FROM apps`,
		},
		{
			name:  "placeholders are numbered from one",
			query: `UPDATE users SET system_role=? WHERE id=?`,
			want:  `UPDATE users SET system_role=$1 WHERE id=$2`,
		},
		{
			name:  "numbering continues past nine",
			query: `INSERT INTO audit_log VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			want:  `INSERT INTO audit_log VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		},
		{
			// A '?' inside a string literal is data, not a placeholder.
			name:  "question mark inside a literal is left alone",
			query: `SELECT * FROM apps WHERE name = '???' AND id = ?`,
			want:  `SELECT * FROM apps WHERE name = '???' AND id = $1`,
		},
		{
			// Postgres escapes a quote by doubling it; the literal continues.
			name:  "doubled quote keeps the literal open",
			query: `SELECT * FROM t WHERE a = 'it''s ?' AND b = ?`,
			want:  `SELECT * FROM t WHERE a = 'it''s ?' AND b = $1`,
		},
		{
			name:  "placeholders around a literal are numbered in order",
			query: `SELECT ? , 'x?y' , ?`,
			want:  `SELECT $1 , 'x?y' , $2`,
		},
		{
			// The one that bit. An apostrophe in a comment used to flip the
			// in-a-string state for the rest of the query, so every later ?
			// stayed a ? — and Postgres blamed a line nobody had touched.
			name: "an apostrophe in a line comment is not a string",
			query: "SELECT a, -- the app's slug\n       b FROM t WHERE id = ?",
			want:  "SELECT a, -- the app's slug\n       b FROM t WHERE id = $1",
		},
		{
			// The comment marker itself must not be special inside a literal,
			// or a string containing "--" would swallow the rest of the query.
			name:  "a comment marker inside a literal is data",
			query: `SELECT * FROM t WHERE a = 'x--y' AND b = ?`,
			want:  `SELECT * FROM t WHERE a = 'x--y' AND b = $1`,
		},
		{
			// Subtraction is not a comment, and one hyphen must not arm it.
			name:  "a single hyphen is arithmetic",
			query: `SELECT a - 1 FROM t WHERE id = ?`,
			want:  `SELECT a - 1 FROM t WHERE id = $1`,
		},
		{
			name:  "real query from the grants path",
			query: `INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by) VALUES (?, ?, ?, ?, ?)`,
			want:  `INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by) VALUES ($1, $2, $3, $4, $5)`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rebind(tt.query); got != tt.want {
				t.Errorf("rebind(%q)\n got: %q\nwant: %q", tt.query, got, tt.want)
			}
		})
	}
}
