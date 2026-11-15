package tags_test

import (
	"testing"

	"toft/internal/builder"
	"toft/internal/tags"
)

// allFrameworks mirrors the constants in internal/builder/detector.go. Adding a
// framework there without adding it here fails this test, which is the point:
// the two lists are what keep a newly supported framework from silently
// producing a blank Framework tag value.
var allFrameworks = []builder.Framework{
	builder.FrameworkNextJS,
	builder.FrameworkNode,
	builder.FrameworkGo,
	builder.FrameworkFlask,
	builder.FrameworkFastAPI,
	builder.FrameworkStreamlit,
	builder.FrameworkPython,
	builder.FrameworkNotebook,
	builder.FrameworkMarimo,
	builder.FrameworkShiny,
	builder.FrameworkPlumber,
	builder.FrameworkRMarkdown,
	builder.FrameworkQuarto,
	builder.FrameworkStatic,
	builder.FrameworkBatchNode,
	builder.FrameworkBatchPython,
	builder.FrameworkBatchGo,
	builder.FrameworkBatchR,
}

func TestEveryFrameworkHasAValue(t *testing.T) {
	for _, fw := range allFrameworks {
		value, slug, ok := tags.FrameworkValue(string(fw))
		if !ok {
			t.Errorf("framework %q has no Framework tag value; add it to internal/tags/system.go", fw)
			continue
		}
		if value == "" {
			t.Errorf("framework %q maps to an empty display value", fw)
		}
		if slug != string(fw) {
			t.Errorf("framework %q: slug = %q, want the detector identifier", fw, slug)
		}
	}
}

// FrameworkValues drives the browse page's value list, so it must cover exactly
// the frameworks FrameworkValue resolves — no more, no fewer.
func TestFrameworkValuesCoversEveryFramework(t *testing.T) {
	listed := map[string]bool{}
	for _, v := range tags.FrameworkValues() {
		if listed[v.Slug] {
			t.Errorf("FrameworkValues lists %q twice", v.Slug)
		}
		listed[v.Slug] = true
	}
	if len(listed) != len(allFrameworks) {
		t.Errorf("FrameworkValues has %d entries, want %d", len(listed), len(allFrameworks))
	}
	for _, fw := range allFrameworks {
		if !listed[string(fw)] {
			t.Errorf("FrameworkValues is missing %q", fw)
		}
	}
}

func TestUnsupportedAndEmptyFrameworkHaveNoValue(t *testing.T) {
	for _, fw := range []string{"", string(builder.FrameworkUnsupported), "django"} {
		if _, _, ok := tags.FrameworkValue(fw); ok {
			t.Errorf("FrameworkValue(%q) resolved; an app with no detected framework must carry no Framework value", fw)
		}
	}
}

func TestIsSystemTagID(t *testing.T) {
	if !tags.IsSystemTagID(tags.FrameworkTagID) {
		t.Error("Framework tag is not recognized as a system tag")
	}
	if tags.IsSystemTagID("some-uuid") {
		t.Error("a custom tag id was treated as a system tag")
	}
}
