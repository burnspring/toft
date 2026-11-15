// Package tags holds the platform-owned parts of the tagging vocabulary.
//
// A tag is a key and an app holds one value for it. Admin-defined tags live in
// the database; system tags are owned by the platform and their values are
// *derived from the app row at read time* rather than stored in app_tags. That
// is what keeps them from ever drifting out of sync with the app's real state,
// and it is why deploying an app writes no tag rows.
//
// Today there is exactly one system tag: Framework.
package tags

// FrameworkTagID is the fixed id of the Framework tag, seeded by migration
// 00009. Handlers compare against it to refuse writes to a system tag.
// The tag `kind` column values live in package database, which owns the
// column; this package owns only the identity of the system tag itself.
const (
	FrameworkTagID   = "tag-framework"
	FrameworkTagSlug = "framework"
)

// Value is one possible value of a system tag: the display text plus the slug
// used in /tags/{tag}/{value} URLs.
type Value struct {
	Value string `json:"value"`
	Slug  string `json:"slug"`
}

// frameworkValues maps a detector identifier (internal/builder/detector.go) to
// its display text. The slug is the detector identifier itself, so a browse URL
// needs no lookup table.
//
// Keep this in sync with internal/builder/detector.go and, for the matching
// badge colors, web/src/lib/frameworks.ts. TestEveryFrameworkHasAValue fails
// the build if a newly detected framework is missing here.
var frameworkValues = map[string]string{
	"nextjs":    "Next.js",
	"node":      "Node.js",
	"go":        "Go",
	"flask":     "Flask",
	"fastapi":   "FastAPI",
	"streamlit": "Streamlit",
	"python":    "Python",
	"notebook":  "Notebook",
	"marimo":    "marimo",
	"shiny":     "Shiny",
	"plumber":   "plumber",
	"rmarkdown": "R Markdown",
	"quarto":    "Quarto",
	"static":    "Static",
	// Batch apps (ADR §107): the language, said as what the app is.
	"batch-node":   "Batch · Node.js",
	"batch-python": "Batch · Python",
	"batch-go":     "Batch · Go",
	"batch-r":      "Batch · R",
}

// frameworkOrder fixes the listing order, so the values page doesn't reshuffle
// between requests the way a map range would.
var frameworkOrder = []string{"nextjs", "node", "go", "flask", "fastapi", "streamlit", "python",
	"notebook", "marimo", "shiny", "plumber", "rmarkdown", "quarto", "static",
	"batch-node", "batch-python", "batch-go", "batch-r"}

// FrameworkValue resolves an app's framework column to its tag value. ok is
// false for an empty or unrecognized framework — an app that has never been
// built successfully simply carries no Framework value.
func FrameworkValue(framework string) (value string, slug string, ok bool) {
	if framework == "" {
		return "", "", false
	}
	label, found := frameworkValues[framework]
	if !found {
		return "", "", false
	}
	return label, framework, true
}

// FrameworkValues lists every value the Framework tag can take, in a stable
// order.
func FrameworkValues() []Value {
	out := make([]Value, 0, len(frameworkOrder))
	for _, slug := range frameworkOrder {
		out = append(out, Value{Value: frameworkValues[slug], Slug: slug})
	}
	return out
}

// IsSystemTagID reports whether id names a platform-owned tag. Such tags cannot
// be renamed, deleted, or set through the API.
func IsSystemTagID(id string) bool { return id == FrameworkTagID }
