// Package gallery reads the catalog of deployable things the Toft team
// publishes — extensions and examples — and answers questions about it.
//
// The catalog is a JSON document at a URL compiled into the binary. It is data,
// never authority: an entry may name code and may ask for environment
// variables, but nothing in it can grant a capability, change a platform
// setting, or widen anyone's permissions. Everything an extension needs beyond
// its own code is an administrator's separate, audited action afterwards.
//
// This package holds no HTTP handler and no database. It parses, it fetches,
// and it answers "what is in the catalog"; who may deploy what lives in
// internal/api, and what an install has installed lives on the app rows.
package gallery

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"toft/internal/builder"
)

// The two kinds of entry, and the whole vocabulary. An item never changes kind.
const (
	KindExtension = "extension"
	KindExample   = "example"
)

// The three states an installed item may be born in. See Item.Visibility.
const (
	BornListable = "listable"
	BornViewable = "viewable"
	BornHidden   = "hidden"
)

// SchemaVersion is the document shape this binary understands. A catalog
// declaring a higher number is not a parse failure — it is a state, and the
// admin panel says "this catalog needs a newer Toft" rather than
// "sync failed". Bump it only for a change an older binary would misread.
const SchemaVersion = 1

// ErrCatalogTooNew is returned when the document declares a schema version this
// binary does not understand. The caller keeps the previous catalog and reports
// the state; it must never render as an empty gallery.
var ErrCatalogTooNew = errors.New("catalog schema is newer than this Toft")

// Item is one entry. Fields the platform does not understand are ignored on
// purpose: a newer catalog must not break an older install.
type Item struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Language    string   `json:"language"`
	Tags        []string `json:"tags"`

	// Icon is a name from the set the SPA already ships, never a URL. A remote
	// image would be a request to a third party from every viewer's browser,
	// which turns an internal tool's page into a beacon.
	Icon string `json:"icon"`

	Version            string `json:"version"`
	MinPlatformVersion string `json:"min_platform_version"`

	// Extensions only: a git source, cloned at Ref. Ref is a tag or a branch
	// and is resolved at deploy time — see the accepted risk in the PRD.
	RepoURL string `json:"repo_url"`
	Ref     string `json:"ref"`

	// Examples only: a ZIP fetched and staged exactly as a human upload is.
	ZipURL string `json:"zip_url"`

	// Path is the folder of the source the item is, for a catalog whose items
	// share one repository: for an extension, the folder of RepoURL its app is
	// built from (apps.repo_subdir); for an example, the folder of ZipURL's
	// archive that is staged, after the archive's own top-level folder is
	// stripped. Empty is the whole source. builder.ValidateSubdir is its check.
	Path string `json:"path"`

	// SourceURL is where a person can go and read this item's code. It is
	// informational only: nothing fetches it, clones it or builds from it, and
	// it may appear on either kind.
	//
	// It exists because RepoURL is the *update channel*, not "the source", and
	// an example is refused if it carries one — so an example's only address is
	// an opaque archive, and a card for one could link nowhere. Reusing RepoURL
	// for the human link would make one field mean two things, and the field
	// that decides what gets cloned is the last one to overload.
	SourceURL string `json:"source_url"`

	SuggestedSlug string     `json:"suggested_slug"`
	Env           []EnvField `json:"env"`
	Notes         string     `json:"notes"`

	// Visibility is the state the installed app is *born* in, and it is the
	// catalog asking rather than the catalog deciding: it moves two columns an
	// administrator can change a minute later on the app's Sharing tab, and it
	// grants nothing that was not already the deployer's to grant.
	//
	// Three values, because two booleans can express nonsense — the same
	// argument gallery_deploy_mode and dependency_policy are one field for:
	//
	//   BornListable (default)  private + listed. The historical behaviour and
	//                           what every entry that says nothing still gets.
	//   BornViewable            authenticated + listed. Every signed-in person
	//                           may open it. For an item whose whole purpose is
	//                           to be used by the people on the install.
	//   BornHidden              private + unlisted. Nobody but the owner and an
	//                           administrator learns it exists. For an item that
	//                           reads the estate and whose existence is itself a
	//                           hint about the install.
	//
	// Never `public`. Anonymous reach is the install's decision, gated on a
	// transition and withdrawable install-wide, and a line in a document fetched
	// over the network must not be able to reach across all of that — so the
	// most a catalog can ask for is "everyone who has signed in".
	//
	// An unrecognised value reads as BornListable rather than dropping the
	// entry: this field is a preference about presentation, and a newer catalog
	// must not break an older install. It is not `refused` in the sense the
	// boolean-switch rule means, because there is no value here that grants
	// more than the deployer already had.
	Visibility string `json:"visibility"`
}

// EnvField is a variable the item asks the deployer to fill in. Asking is all
// it can do: the value is stored as an ordinary app env var, and nothing about
// the entry grants the app anything.
type EnvField struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Default     string `json:"default"`
	Description string `json:"description"`
	// Sensitive marks a field whose value must not be rendered back — a
	// connection string, a passphrase, a token an extension needs and the
	// platform has no other way to be given.
	//
	// It does not change what happens to the value: every env var is encrypted
	// under the install's own key ring on the way in, whatever this says. What
	// it changes is the deploy dialog, which collects a sensitive field as a
	// password and marks it so the app's Settings tab shows it as stored rather
	// than showing it. §28's rule is untouched — the catalog *asked*, it granted
	// nothing, and the value is an ordinary app variable an editor can replace.
	Sensitive bool `json:"sensitive"`
}

// Catalog is a parsed document plus what was thrown away parsing it.
type Catalog struct {
	SchemaVersion int    `json:"schema_version"`
	GeneratedAt   string `json:"generated_at"`
	Items         []Item `json:"items"`

	// Dropped records every entry that failed validation, with its reason.
	// Counted and surfaced to administrators rather than logged and forgotten:
	// a catalog that silently lists nine of ten items is a support call nobody
	// can answer.
	Dropped []Drop `json:"-"`
}

// Drop is one rejected entry.
type Drop struct {
	ID     string
	Name   string
	Reason string
}

// Item returns the entry with this id.
func (c *Catalog) Item(id string) (Item, bool) {
	if c == nil {
		return Item{}, false
	}
	for _, it := range c.Items {
		if it.ID == id {
			return it, true
		}
	}
	return Item{}, false
}

// maxItems and maxNameLen bound a document that is fetched from elsewhere.
// maxNameLen matches the platform's own app-name limit, because an item's name
// is proposed as one.
const (
	maxItems    = 500
	maxNameLen  = 100
	maxIDLen    = 100
	maxTextLen  = 4000
	maxTagCount = 12
)

// Parse reads a catalog document.
//
// Lenient per document, strict per entry: unknown fields are ignored, and an
// entry that breaks a rule is dropped on its own with a reason. One malformed
// entry never empties the gallery — which is the difference between a bad
// commit in the catalog repo costing one item and costing every install's
// gallery.
func Parse(data []byte) (*Catalog, error) {
	var doc Catalog
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing the catalog: %w", err)
	}
	if doc.SchemaVersion <= 0 {
		return nil, errors.New("catalog has no schema_version")
	}
	if doc.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("%w (document says %d, this build understands %d)",
			ErrCatalogTooNew, doc.SchemaVersion, SchemaVersion)
	}
	if len(doc.Items) > maxItems {
		return nil, fmt.Errorf("catalog lists %d items; the limit is %d", len(doc.Items), maxItems)
	}

	kept := make([]Item, 0, len(doc.Items))
	seen := make(map[string]bool, len(doc.Items))
	for _, it := range doc.Items {
		it.ID = strings.TrimSpace(it.ID)
		it.Name = strings.TrimSpace(it.Name)
		if seen[it.ID] && it.ID != "" {
			doc.Dropped = append(doc.Dropped, Drop{it.ID, it.Name, "duplicate id"})
			continue
		}
		if err := validateItem(&it); err != nil {
			doc.Dropped = append(doc.Dropped, Drop{it.ID, it.Name, err.Error()})
			continue
		}
		seen[it.ID] = true
		kept = append(kept, it)
	}
	doc.Items = kept
	return &doc, nil
}

// validateItem applies the per-entry rules and normalizes in place.
//
// An unparseable version is deliberately *not* fatal: the item is still
// installable, it simply can never show an update, which the app page says out
// loud rather than implying currency.
func validateItem(it *Item) error {
	if it.ID == "" {
		return errors.New("missing id")
	}
	if len(it.ID) > maxIDLen {
		return errors.New("id is too long")
	}
	switch it.Kind {
	case KindExtension, KindExample:
	case "":
		return errors.New("missing kind")
	default:
		return fmt.Errorf("unknown kind %q", it.Kind)
	}
	if it.Name == "" {
		return errors.New("missing name")
	}
	if err := validName(it.Name); err != nil {
		return err
	}
	if strings.TrimSpace(it.Version) == "" {
		return errors.New("missing version")
	}
	if len(it.Summary) > maxTextLen || len(it.Description) > maxTextLen || len(it.Notes) > maxTextLen {
		return errors.New("summary, description or notes is too long")
	}
	if len(it.Tags) > maxTagCount {
		it.Tags = it.Tags[:maxTagCount]
	}
	if it.MinPlatformVersion != "" {
		if _, ok := parseVersion(it.MinPlatformVersion); !ok {
			return fmt.Errorf("min_platform_version %q is not a version", it.MinPlatformVersion)
		}
	}

	if err := builder.ValidateSubdir(it.Path); err != nil {
		return fmt.Errorf("path: %w", err)
	}

	switch it.Kind {
	case KindExtension:
		if it.RepoURL == "" || it.Ref == "" {
			return errors.New("an extension needs repo_url and ref")
		}
		if it.ZipURL != "" {
			return errors.New("an extension must not carry zip_url")
		}
		if err := CheckSourceURL(it.RepoURL); err != nil {
			return fmt.Errorf("repo_url: %w", err)
		}
		if strings.ContainsAny(it.Ref, " \t\n") || strings.HasPrefix(it.Ref, "-") {
			return errors.New("ref is not a usable git ref")
		}
	case KindExample:
		if it.ZipURL == "" {
			return errors.New("an example needs zip_url")
		}
		if it.RepoURL != "" || it.Ref != "" {
			return errors.New("an example must not carry repo_url or ref")
		}
		if err := CheckSourceURL(it.ZipURL); err != nil {
			return fmt.Errorf("zip_url: %w", err)
		}
	}

	// An unusable source_url costs the link, not the item. It feeds no deploy
	// path, so dropping an otherwise valid entry over a cosmetic field would
	// trade a whole gallery card for a broken hyperlink — the same trade the
	// env-name rule below already refuses. CheckSourceURL is syntax alone
	// (https, a host, no credentials), which is what keeps parsing free of
	// anything that touches the network.
	it.SourceURL = strings.TrimSpace(it.SourceURL)
	if it.SourceURL != "" && CheckSourceURL(it.SourceURL) != nil {
		it.SourceURL = ""
	}

	// Env fields that will not survive as environment variable names are
	// dropped from the prompt rather than passed to the app, where they would
	// arrive as something the app never declared.
	env := it.Env[:0]
	for _, f := range it.Env {
		f.Name = strings.TrimSpace(f.Name)
		if validEnvName(f.Name) {
			env = append(env, f)
		}
	}
	it.Env = env

	// The born state settles here rather than at install, so every reader of a
	// parsed catalog sees one of exactly three words and no caller has to
	// remember the fallback. An unrecognised value — including the empty string
	// every entry written before this field existed carries — reads as
	// BornListable, which is the behaviour those entries already had.
	it.Visibility = NormalizeVisibility(it.Visibility)
	return nil
}

// NormalizeVisibility maps whatever a catalog wrote to one of the three born
// states, defaulting to BornListable. Exported because the admin panel renders
// the answer beside the card and must not re-derive it.
func NormalizeVisibility(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case BornViewable:
		return BornViewable
	case BornHidden:
		return BornHidden
	default:
		return BornListable
	}
}

// validName mirrors the platform's own app-name rule, because this name is
// proposed as an app name and a catalog must not be able to smuggle in one the
// API would refuse.
func validName(name string) error {
	if len(name) > maxNameLen {
		return fmt.Errorf("name is longer than %d characters", maxNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("name contains control characters")
		}
	}
	return nil
}

func validEnvName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// --- versions ---

type semver struct {
	major, minor, patch int
	pre                 string
}

// parseVersion reads a dotted version, tolerating a leading "v" and ignoring
// build metadata. It is deliberately small: the catalog is written by one team
// and the only question asked of a version is whether one is newer.
func parseVersion(s string) (semver, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return semver{}, false
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v semver
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.pre, s = s[i+1:], s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return semver{}, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		nums[i] = n
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	return v, true
}

// CompareVersions returns -1, 0 or 1, and false when either side is not a
// version this package can read. Callers must treat the false case as "cannot
// tell" — never as "up to date".
func CompareVersions(a, b string) (int, bool) {
	va, ok := parseVersion(a)
	if !ok {
		return 0, false
	}
	vb, ok := parseVersion(b)
	if !ok {
		return 0, false
	}
	for _, pair := range [][2]int{
		{va.major, vb.major}, {va.minor, vb.minor}, {va.patch, vb.patch},
	} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1, true
			}
			return 1, true
		}
	}
	// A prerelease is older than the release it leads to; two prereleases
	// compare as strings, which is enough for one team's catalog.
	switch {
	case va.pre == vb.pre:
		return 0, true
	case va.pre == "":
		return 1, true
	case vb.pre == "":
		return -1, true
	case va.pre < vb.pre:
		return -1, true
	default:
		return 1, true
	}
}

// IsNewer reports whether candidate is strictly newer than current. Never on
// mere inequality: a catalog that rolls a version back must not present as an
// upgrade.
func IsNewer(candidate, current string) bool {
	c, ok := CompareVersions(candidate, current)
	return ok && c > 0
}

// SatisfiesPlatform reports whether this build is new enough for the item, and
// the requirement when it is not. An item that needs a newer Toft stays
// *listed* and is not deployable — "why can I not see it" is a worse support
// call than "why can I not deploy it".
func (it Item) SatisfiesPlatform(platformVersion string) (bool, string) {
	if it.MinPlatformVersion == "" {
		return true, ""
	}
	c, ok := CompareVersions(platformVersion, it.MinPlatformVersion)
	if !ok {
		// An unreadable platform version is this build's problem, not the
		// catalog's, and blocking every item over it would be worse.
		return true, ""
	}
	if c < 0 {
		return false, it.MinPlatformVersion
	}
	return true, ""
}
