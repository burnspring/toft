package gallery

import (
	"encoding/json"
	"strings"
	"testing"
)

// doc builds a catalog document around a list of raw item objects.
func doc(schema int, items ...string) []byte {
	return []byte(`{"schema_version":` + itoa(schema) + `,"items":[` + strings.Join(items, ",") + `]}`)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

const goodExtension = `{
	"id":"ext-1","kind":"extension","name":"Audit Explorer","version":"1.4.0",
	"repo_url":"https://github.com/acme/audit-explorer","ref":"v1.4.0"
}`

const goodExample = `{
	"id":"ex-1","kind":"example","name":"Sales Dashboard","version":"2.1.0",
	"zip_url":"https://github.com/acme/gallery/releases/download/x/sales.zip"
}`

func TestParseKeepsGoodItemsAndIgnoresUnknownFields(t *testing.T) {
	// The unknown field is the point: a catalog written for a newer binary
	// must still work on an older one, or every catalog change becomes a
	// coordinated upgrade.
	withExtra := `{
		"id":"ext-2","kind":"extension","name":"Thing","version":"0.1.0",
		"repo_url":"https://github.com/acme/thing","ref":"main",
		"something_from_the_future":{"nested":true}
	}`

	c, err := Parse(doc(1, goodExtension, goodExample, withExtra))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Items) != 3 {
		t.Fatalf("kept %d items, want 3: %+v", len(c.Items), c.Items)
	}
	if len(c.Dropped) != 0 {
		t.Fatalf("dropped %+v", c.Dropped)
	}
	if it, ok := c.Item("ex-1"); !ok || it.Kind != KindExample {
		t.Fatalf("Item(ex-1) = %+v, %v", it, ok)
	}
}

func TestParseDropsBadEntriesIndividually(t *testing.T) {
	cases := []struct {
		name string
		item string
		why  string
	}{
		{"no id", `{"kind":"extension","name":"X","version":"1.0.0","repo_url":"https://github.com/a/b","ref":"v1"}`, "missing id"},
		{"no kind", `{"id":"a","name":"X","version":"1.0.0"}`, "missing kind"},
		{"unknown kind", `{"id":"a","kind":"plugin","name":"X","version":"1.0.0"}`, "unknown kind"},
		{"no version", `{"id":"a","kind":"extension","name":"X","repo_url":"https://github.com/a/b","ref":"v1"}`, "missing version"},
		{"extension without ref", `{"id":"a","kind":"extension","name":"X","version":"1.0.0","repo_url":"https://github.com/a/b"}`, "needs repo_url and ref"},
		{"extension with zip", `{"id":"a","kind":"extension","name":"X","version":"1.0.0","repo_url":"https://github.com/a/b","ref":"v1","zip_url":"https://x/y.zip"}`, "must not carry zip_url"},
		{"example without zip", `{"id":"a","kind":"example","name":"X","version":"1.0.0"}`, "needs zip_url"},
		{"example with repo", `{"id":"a","kind":"example","name":"X","version":"1.0.0","zip_url":"https://x/y.zip","repo_url":"https://github.com/a/b"}`, "must not carry repo_url"},
		{"http source", `{"id":"a","kind":"example","name":"X","version":"1.0.0","zip_url":"http://x/y.zip"}`, "must be https"},
		{"credentials in url", `{"id":"a","kind":"example","name":"X","version":"1.0.0","zip_url":"https://u:p@x/y.zip"}`, "must not carry credentials"},
		{"control character in name", "{\"id\":\"a\",\"kind\":\"example\",\"name\":\"X\\u0007Y\",\"version\":\"1.0.0\",\"zip_url\":\"https://x/y.zip\"}", "control characters"},
		{"path outside the source", `{"id":"a","kind":"extension","name":"X","version":"1.0.0","repo_url":"https://github.com/a/b","ref":"v1","path":"../other"}`, "path:"},
		{"path into .git", `{"id":"a","kind":"example","name":"X","version":"1.0.0","zip_url":"https://x/y.zip","path":"a/.git"}`, "path:"},
		{"bad min platform", `{"id":"a","kind":"example","name":"X","version":"1.0.0","min_platform_version":"soon","zip_url":"https://x/y.zip"}`, "min_platform_version"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The good item rides along in every case: one bad entry must cost
			// exactly one entry, never the catalog.
			c, err := Parse(doc(1, tc.item, goodExtension))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(c.Items) != 1 || c.Items[0].ID != "ext-1" {
				t.Fatalf("items = %+v, want only the good one", c.Items)
			}
			if len(c.Dropped) != 1 {
				t.Fatalf("dropped = %+v, want one", c.Dropped)
			}
			if !strings.Contains(c.Dropped[0].Reason, tc.why) {
				t.Errorf("reason = %q, want it to mention %q", c.Dropped[0].Reason, tc.why)
			}
		})
	}
}

func TestParseDropsDuplicateIDsKeepingTheFirst(t *testing.T) {
	second := `{"id":"ext-1","kind":"extension","name":"Impostor","version":"9.9.9",
		"repo_url":"https://github.com/evil/x","ref":"main"}`

	c, err := Parse(doc(1, goodExtension, second))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Items) != 1 {
		t.Fatalf("items = %+v", c.Items)
	}
	if got := c.Items[0].RepoURL; got != "https://github.com/acme/audit-explorer" {
		t.Errorf("kept %q; the first entry with an id must win", got)
	}
	if len(c.Dropped) != 1 || !strings.Contains(c.Dropped[0].Reason, "duplicate") {
		t.Errorf("dropped = %+v", c.Dropped)
	}
}

func TestParseDocumentLevelFailures(t *testing.T) {
	if _, err := Parse([]byte(`not json`)); err == nil {
		t.Error("expected an error for a document that is not JSON")
	}
	if _, err := Parse([]byte(`{"items":[]}`)); err == nil {
		t.Error("expected an error for a document with no schema_version")
	}

	_, err := Parse(doc(SchemaVersion+1, goodExtension))
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("a newer schema must be its own state, got %v", err)
	}
}

func TestParseRefusesAnOversizeCatalog(t *testing.T) {
	items := make([]string, maxItems+1)
	for i := range items {
		items[i] = `{"id":"` + itoa(i) + `","kind":"example","name":"X","version":"1.0.0","zip_url":"https://x/y.zip"}`
	}
	if _, err := Parse(doc(1, items...)); err == nil {
		t.Error("expected an error for more items than the limit")
	}
}

func TestParseKeepsAnUnreadableVersionButNeverCallsItAnUpdate(t *testing.T) {
	odd := `{"id":"a","kind":"extension","name":"X","version":"nightly",
		"repo_url":"https://github.com/a/b","ref":"main"}`

	c, err := Parse(doc(1, odd))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Items) != 1 {
		t.Fatalf("an unreadable version must not drop the item: %+v", c.Dropped)
	}
	if IsNewer("nightly", "1.0.0") || IsNewer("1.0.0", "nightly") {
		t.Error("an unreadable version can never be newer — that would be a guess")
	}
}

func TestParseDropsUnusableEnvNames(t *testing.T) {
	item := `{"id":"a","kind":"extension","name":"X","version":"1.0.0",
		"repo_url":"https://github.com/a/b","ref":"main",
		"env":[{"name":"GOOD_ONE"},{"name":"has-dash"},{"name":"9LEADING"},{"name":""}]}`

	c, err := Parse(doc(1, item))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	env := c.Items[0].Env
	if len(env) != 1 || env[0].Name != "GOOD_ONE" {
		t.Fatalf("env = %+v, want only GOOD_ONE", env)
	}
}

// source_url is the one field whose failure costs a link rather than the item:
// it feeds no deploy path, so an entry that is otherwise fine must survive a
// bad one. It is also the only address an *example* can offer a reader, since
// an example carrying repo_url is refused outright.
func TestParseKeepsTheItemWhenSourceURLIsUnusable(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"good", `"https://github.com/acme/thing"`, "https://github.com/acme/thing"},
		{"not https", `"http://github.com/acme/thing"`, ""},
		{"carries credentials", `"https://user:pw@github.com/acme/thing"`, ""},
		{"no host", `"https:///acme/thing"`, ""},
		{"not a url at all", `"github.com acme"`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := `{"id":"a","kind":"example","name":"X","version":"1.0.0",
				"zip_url":"https://github.com/a/b/archive/refs/tags/v1.0.0.zip",
				"source_url":` + tc.raw + `}`

			c, err := Parse(doc(1, item))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(c.Items) != 1 {
				t.Fatalf("kept %d items, want the entry to survive: dropped %+v", len(c.Items), c.Dropped)
			}
			if got := c.Items[0].SourceURL; got != tc.want {
				t.Fatalf("SourceURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"1.4.0", "1.3.9", 1, true},
		{"1.3.9", "1.4.0", -1, true},
		{"1.4.0", "1.4.0", 0, true},
		{"v1.4.0", "1.4.0", 0, true},
		{"2.0.0", "1.99.99", 1, true},
		{"1.4.0", "1.4.0-rc1", 1, true},
		{"1.4.0-rc1", "1.4.0-rc2", -1, true},
		{"1.4.0+build9", "1.4.0", 0, true},
		{"1.4", "1.4.0", 0, true},
		{"", "1.0.0", 0, false},
		{"latest", "1.0.0", 0, false},
	}
	for _, tc := range cases {
		got, ok := CompareVersions(tc.a, tc.b)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("CompareVersions(%q, %q) = %d, %v; want %d, %v", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSatisfiesPlatform(t *testing.T) {
	it := Item{MinPlatformVersion: "0.4.0"}
	if ok, _ := it.SatisfiesPlatform("0.3.9"); ok {
		t.Error("0.3.9 must not satisfy 0.4.0")
	}
	if ok, need := it.SatisfiesPlatform("0.4.0"); !ok {
		t.Errorf("0.4.0 must satisfy 0.4.0 (need %q)", need)
	}
	if ok, _ := (Item{}).SatisfiesPlatform("0.1.0"); !ok {
		t.Error("an item with no requirement is always satisfied")
	}
}

// The born state is a preference about presentation, so an unrecognised value
// is normalized rather than dropping the entry — a newer catalog must not break
// an older install. Every entry written before the field existed carries the
// empty string and must keep the behaviour it already had.
func TestNormalizeVisibility(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", BornListable},
		{"listable", BornListable},
		{"viewable", BornViewable},
		{"hidden", BornHidden},
		{"  Hidden  ", BornHidden},
		{"VIEWABLE", BornViewable},
		// Never public: the catalog cannot ask for anonymous reach, and a value
		// that tries reads as the default rather than as anything wider.
		{"public", BornListable},
		{"anonymous", BornListable},
		{"nonsense", BornListable},
	} {
		if got := NormalizeVisibility(tc.in); got != tc.want {
			t.Errorf("NormalizeVisibility(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Parsing settles the field, so no consumer has to remember the fallback.
func TestParseNormalizesVisibility(t *testing.T) {
	doc := []byte(`{"schema_version":1,"items":[
	  {"id":"a","kind":"extension","name":"A","summary":"s","repo_url":"https://github.com/x/y","ref":"v1","version":"1.0.0"},
	  {"id":"b","kind":"extension","name":"B","summary":"s","repo_url":"https://github.com/x/y","ref":"v1","version":"1.0.0","visibility":"hidden"},
	  {"id":"c","kind":"extension","name":"C","summary":"s","repo_url":"https://github.com/x/y","ref":"v1","version":"1.0.0","visibility":"public"}
	]}`)
	cat, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cat.Items) != 3 {
		t.Fatalf("got %d items, want 3 (dropped: %v)", len(cat.Items), cat.Dropped)
	}
	want := map[string]string{"a": BornListable, "b": BornHidden, "c": BornListable}
	for _, it := range cat.Items {
		if it.Visibility != want[it.ID] {
			t.Errorf("item %s: visibility = %q, want %q", it.ID, it.Visibility, want[it.ID])
		}
	}
}
