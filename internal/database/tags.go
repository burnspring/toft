package database

import (
	"database/sql"
	"strings"
	"time"
)

// Tag kinds. A 'system' tag is platform-owned: its values are derived from the
// app row rather than stored in app_tags, and it cannot be renamed, deleted, or
// set through the API.
const (
	TagKindSystem = "system"
	TagKindCustom = "custom"
)

// tagValueSuggestionLimit bounds the value autocomplete. The picker is a
// suggestion aid, not a browse surface — /tags/{slug} is where the full list lives.
const tagValueSuggestionLimit = 50

// Tag is a key that apps carry a value for.
type Tag struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Kind        string `json:"kind"`
	Color       string `json:"color"`
	Description string `json:"description"`
	CreatedBy   string `json:"created_by,omitempty"`
	// ValueCount and AppCount are scoped to the caller (see AppScope): a viewer
	// must not learn from a count that a private app carries this tag.
	ValueCount int       `json:"value_count"`
	AppCount   int       `json:"app_count"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// AppTag is one key/value pair on an app.
type AppTag struct {
	TagID     string `json:"tag_id"`
	TagName   string `json:"tag_name"`
	TagSlug   string `json:"tag_slug"`
	Kind      string `json:"kind"`
	Color     string `json:"color"`
	Value     string `json:"value"`
	ValueSlug string `json:"value_slug"`
}

// TagValue is one value in use for a tag, with how many apps carry it.
type TagValue struct {
	Value    string `json:"value"`
	Slug     string `json:"slug"`
	AppCount int    `json:"app_count"`
}

// AppScope describes which apps a query is allowed to see. It exists so tag
// listings, value suggestions, counts, browse queries, cost figures and the
// estate census all narrow through the same definitions ListApps uses — a
// second, hand-rolled visibility filter is how one of them eventually goes
// wrong.
//
// **There are two definitions and this struct answers to both**, which is why it
// has two predicates rather than one and why every query that takes an AppScope
// has to say which it means:
//
//   - [AppScope.listingPredicate] is listable — the apps whose existence the
//     caller has been told about. A tag count that omitted the apps beside it in
//     the list would disagree with the list, so every tag surface reads this.
//   - [AppScope.readingPredicate] is readable — the apps the caller may open.
//     A cost figure, a language and a version are facts *about the app*, not the
//     row that names it, and the list rung discloses the row and not one field
//     more. So consumption and the estate census read this, and a caller holding
//     only the rung finds their app absent from both, exactly as they would have
//     been before the rung existed.
//
// The split is the same one readableAppsPredicate and listableAppsPredicate
// make, carried to the callers that reach them through a struct;
// TestAppsPredicateCallers asserts both caller sets.
type AppScope struct {
	PrincipalID   string
	PrincipalType string
	Groups        []string
	// All lifts the restriction entirely (system_admin), mirroring ListAllApps.
	All bool
}

func (s AppScope) listingPredicate() (string, []interface{}) {
	if s.All {
		return "(TRUE)", nil
	}
	return listableAppsPredicate(s.PrincipalID, s.PrincipalType, s.Groups)
}

func (s AppScope) readingPredicate() (string, []interface{}) {
	if s.All {
		return "(TRUE)", nil
	}
	return readableAppsPredicate(s.PrincipalID, s.PrincipalType, s.Groups)
}

// ListTags returns every tag with caller-scoped usage counts.
//
// System tags have no app_tags rows — their values are derived — so their
// counts are filled in from the app column afterwards rather than by the join.
func (db *DB) ListTags(scope AppScope) ([]Tag, error) {
	pred, args := scope.listingPredicate()

	rows, err := db.conn.Query(`
		SELECT t.id, t.name, t.slug, t.kind, t.color, t.description, COALESCE(t.created_by,''),
		       t.created_at, t.updated_at,
		       -- Both counts must ignore app_tags rows whose app failed the
		       -- readability join, or the value count tells a viewer how many
		       -- distinct values exist on apps they cannot see.
		       COUNT(DISTINCT CASE WHEN apps.id IS NOT NULL THEN at.value_slug END),
		       COUNT(DISTINCT apps.id)
		FROM tags t
		LEFT JOIN app_tags at ON at.tag_id = t.id
		LEFT JOIN apps ON apps.id = at.app_id AND `+pred+`
		GROUP BY t.id, t.name, t.slug, t.kind, t.color, t.description, t.created_by,
		         t.created_at, t.updated_at
		ORDER BY t.kind DESC, t.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Slug, &t.Kind, &t.Color, &t.Description,
			&t.CreatedBy, &t.CreatedAt, &t.UpdatedAt, &t.ValueCount, &t.AppCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		if out[i].Kind != TagKindSystem {
			continue
		}
		values, apps, err := db.frameworkCounts(scope)
		if err != nil {
			return nil, err
		}
		out[i].ValueCount, out[i].AppCount = values, apps
	}
	return out, nil
}

// frameworkCounts counts distinct frameworks and tagged apps among the apps the
// caller can read. This is the Framework tag's usage, derived from apps.framework.
func (db *DB) frameworkCounts(scope AppScope) (values int, apps int, err error) {
	pred, args := scope.listingPredicate()
	err = db.conn.QueryRow(`
		SELECT COUNT(DISTINCT apps.framework), COUNT(DISTINCT apps.id)
		FROM apps WHERE apps.framework <> '' AND `+pred, args...).Scan(&values, &apps)
	return values, apps, err
}

func (db *DB) GetTag(id string) (*Tag, error) {
	return db.getTagBy("id", id)
}

func (db *DB) GetTagBySlug(slug string) (*Tag, error) {
	return db.getTagBy("slug", slug)
}

func (db *DB) getTagBy(column, value string) (*Tag, error) {
	t := &Tag{}
	err := db.conn.QueryRow(`
		SELECT id, name, slug, kind, color, description, COALESCE(created_by,''), created_at, updated_at
		FROM tags WHERE `+column+` = ?`, value).
		Scan(&t.ID, &t.Name, &t.Slug, &t.Kind, &t.Color, &t.Description, &t.CreatedBy,
			&t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// TagSlugExists reports whether a slug is taken, for suffixing at create time.
func (db *DB) TagSlugExists(slug string) (bool, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM tags WHERE slug = ?`, slug).Scan(&n)
	return n > 0, err
}

func (db *DB) CreateTagTx(tx *Tx, t *Tag) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	t.UpdatedAt = t.CreatedAt
	if t.Kind == "" {
		t.Kind = TagKindCustom
	}
	_, err := tx.Exec(`
		INSERT INTO tags (id, name, slug, kind, color, description, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Slug, t.Kind, t.Color, t.Description, nullIfEmpty(t.CreatedBy), t.CreatedAt, t.UpdatedAt)
	return err
}

// UpdateTagTx writes the editable fields. Kind is deliberately not among them:
// a custom tag can never become system-owned.
func (db *DB) UpdateTagTx(tx *Tx, t *Tag) error {
	t.UpdatedAt = time.Now().UTC()
	_, err := tx.Exec(`
		UPDATE tags SET name = ?, slug = ?, color = ?, description = ?, updated_at = ?
		WHERE id = ?`,
		t.Name, t.Slug, t.Color, t.Description, t.UpdatedAt, t.ID)
	return err
}

// DeleteTagTx removes a tag; app_tags rows cascade.
func (db *DB) DeleteTagTx(tx *Tx, id string) error {
	_, err := tx.Exec(`DELETE FROM tags WHERE id = ?`, id)
	return err
}

// CountAppsWithTag counts every app carrying the tag, unscoped. Callers are
// admins deciding whether a delete is destructive, so they get the true figure.
func (db *DB) CountAppsWithTag(tagID string) (int, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM app_tags WHERE tag_id = ?`, tagID).Scan(&n)
	return n, err
}

// AppIDsWithTag lists the apps a tag is set on, bounded, for the audit record
// written when an admin force-deletes it.
func (db *DB) AppIDsWithTag(tagID string, limit int) ([]string, error) {
	rows, err := db.conn.Query(
		`SELECT app_id FROM app_tags WHERE tag_id = ? ORDER BY app_id LIMIT ?`, tagID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CanonicalValue returns the spelling already stored for this tag/value_slug,
// or "" if the value is new. The write path snaps to it so that "engineering"
// typed today joins "Engineering" from yesterday instead of forking the
// vocabulary — the main hazard of letting users type values freely.
func (db *DB) CanonicalValue(tagID, valueSlug string) (string, error) {
	var v string
	err := db.conn.QueryRow(
		`SELECT value FROM app_tags WHERE tag_id = ? AND value_slug = ? LIMIT 1`,
		tagID, valueSlug).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetAppTagTx sets an app's value for a tag, replacing any previous one. The
// app_tags primary key is what makes this a replace — it is the single-select
// rule, so no read-then-write check is needed and concurrent writers cannot
// leave an app holding two values for one tag.
func (db *DB) SetAppTagTx(tx *Tx, appID, tagID, value, valueSlug, actor string) error {
	_, err := tx.Exec(`
		INSERT INTO app_tags (app_id, tag_id, value, value_slug, added_by, added_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (app_id, tag_id) DO UPDATE
		SET value = EXCLUDED.value, value_slug = EXCLUDED.value_slug,
		    added_by = EXCLUDED.added_by, added_at = EXCLUDED.added_at`,
		appID, tagID, value, valueSlug, nullIfEmpty(actor), time.Now().UTC())
	return err
}

func (db *DB) ClearAppTagTx(tx *Tx, appID, tagID string) error {
	_, err := tx.Exec(`DELETE FROM app_tags WHERE app_id = ? AND tag_id = ?`, appID, tagID)
	return err
}

// TagsForApp returns one app's stored (custom) tag values. The Framework pair
// is derived by the caller from the app row.
func (db *DB) TagsForApp(appID string) ([]AppTag, error) {
	byApp, err := db.TagsForApps([]string{appID})
	if err != nil {
		return nil, err
	}
	return byApp[appID], nil
}

// TagsForApps loads tags for many apps in one query. The app list has already
// been authorized by the caller, so this does no filtering of its own.
//
// Batched deliberately: the app listing is a hot path that already carries one
// per-app lookup, and a second would compound it.
func (db *DB) TagsForApps(appIDs []string) (map[string][]AppTag, error) {
	out := map[string][]AppTag{}
	if len(appIDs) == 0 {
		return out, nil // an empty IN () is a syntax error
	}

	placeholders := make([]string, len(appIDs))
	args := make([]interface{}, len(appIDs))
	for i, id := range appIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	rows, err := db.conn.Query(`
		SELECT at.app_id, t.id, t.name, t.slug, t.kind, t.color, at.value, at.value_slug
		FROM app_tags at
		JOIN tags t ON t.id = at.tag_id
		WHERE at.app_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY t.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var appID string
		var t AppTag
		if err := rows.Scan(&appID, &t.TagID, &t.TagName, &t.TagSlug, &t.Kind, &t.Color,
			&t.Value, &t.ValueSlug); err != nil {
			return nil, err
		}
		out[appID] = append(out[appID], t)
	}
	return out, rows.Err()
}

// ListTagValues returns the values in use for a tag, most-used first, drawn
// only from apps the caller can read. Scoping matters here beyond tidiness:
// unscoped, the picker's autocomplete would reveal that some private app is
// tagged "Project Titan".
func (db *DB) ListTagValues(tagID, q string, scope AppScope) ([]TagValue, error) {
	pred, args := scope.listingPredicate()
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)

	args = append(args, tagID, q, "%"+escaped+"%", tagValueSuggestionLimit)
	rows, err := db.conn.Query(`
		SELECT at.value, at.value_slug, COUNT(DISTINCT apps.id) AS app_count
		FROM app_tags at
		JOIN apps ON apps.id = at.app_id AND `+pred+`
		WHERE at.tag_id = ?
		  AND (? = '' OR at.value ILIKE ? ESCAPE '\')
		GROUP BY at.value, at.value_slug
		ORDER BY app_count DESC, at.value ASC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TagValue
	for rows.Next() {
		var v TagValue
		if err := rows.Scan(&v.Value, &v.Slug, &v.AppCount); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListAppsByTag returns readable apps carrying a value for the tag. An empty
// valueSlug means "any value".
func (db *DB) ListAppsByTag(tagSlug, valueSlug string, scope AppScope) ([]App, error) {
	pred, args := scope.listingPredicate()

	query := `SELECT ` + appColumns + ` FROM apps
		JOIN app_tags at ON at.app_id = apps.id
		JOIN tags t ON t.id = at.tag_id
		WHERE ` + pred + ` AND t.slug = ?`
	args = append(args, tagSlug)
	if valueSlug != "" {
		query += ` AND at.value_slug = ?`
		args = append(args, valueSlug)
	}
	query += ` ORDER BY apps.created_at DESC`

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	return scanAppRows(rows)
}

// ListAppsByFramework is the Framework tag's browse query. It reads the app
// column directly because that tag's values are derived, never stored.
func (db *DB) ListAppsByFramework(framework string, scope AppScope) ([]App, error) {
	pred, args := scope.listingPredicate()

	query := `SELECT ` + appColumns + ` FROM apps WHERE ` + pred + ` AND apps.framework <> ''`
	if framework != "" {
		query += ` AND apps.framework = ?`
		args = append(args, framework)
	}
	query += ` ORDER BY apps.created_at DESC`

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	return scanAppRows(rows)
}

// FrameworkValueCounts returns how many readable apps carry each framework,
// for the Framework tag's value listing.
func (db *DB) FrameworkValueCounts(scope AppScope) (map[string]int, error) {
	pred, args := scope.listingPredicate()
	rows, err := db.conn.Query(`
		SELECT apps.framework, COUNT(*) FROM apps
		WHERE apps.framework <> '' AND `+pred+`
		GROUP BY apps.framework`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var fw string
		var n int
		if err := rows.Scan(&fw, &n); err != nil {
			return nil, err
		}
		out[fw] = n
	}
	return out, rows.Err()
}
