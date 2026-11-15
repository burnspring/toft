package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// The section list is valid against this build's registry: known gates, a
// mirrored route for every gated section, citations that resolve.
func TestTheAppFileSectionsAreValid(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAppFile(reg); err != nil {
		t.Fatal(err)
	}
}

type panicky struct{}

func (panicky) CollectSection(_ context.Context, sec AppFileSection) Result {
	switch sec.Key {
	case "identity":
		panic("boom")
	case "source":
		return Result{}
	}
	return OK(map[string]string{"k": sec.Key})
}

// Nothing is skipped, a panic is contained, and the export's digest is
// recomputable from the file alone through the same rule as the install's.
func TestTheAppFileDigestIsRecomputableFromTheFile(t *testing.T) {
	f := AssembleAppFile(context.Background(), panicky{})
	f.App = AppFileSubject{ID: "a1", Slug: "claims", Name: "Claims"}
	if len(f.Sections) != len(AppFileSections) {
		t.Fatalf("%d sections, want %d", len(f.Sections), len(AppFileSections))
	}
	if f.Sections[0].Outcome != OutcomeError || f.Sections[1].Outcome != OutcomeError {
		t.Errorf("the panic and the empty answer are %s and %s, want errors", f.Sections[0].Outcome, f.Sections[1].Outcome)
	}
	raw, err := FreezeAppFile(f)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Artifact string          `json:"artifact"`
		Digest   string          `json:"sha256"`
		Report   json.RawMessage `json:"report"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	canon, err := Canonical(doc.Report)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canon)
	if doc.Artifact != "toft-app-audit-file" || doc.Digest != hex.EncodeToString(sum[:]) {
		t.Errorf("artifact %q, digest %s, want toft-app-audit-file over the member", doc.Artifact, doc.Digest)
	}
	if got := AppFileFilename(f); len(got) < len("claims-audit-") || got[:13] != "claims-audit-" {
		t.Errorf("filename = %q", got)
	}
}
