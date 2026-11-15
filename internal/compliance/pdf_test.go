package compliance

import (
	"bytes"
	"testing"
	"time"
)

// **The document renders, and it renders every requirement.**
//
// A golden page count would be brittle — the mapping's prose changes and the
// pagination moves — so the assertion is on what must be true regardless: it is
// a PDF, it is substantial, and every requirement identifier appears in it.
//
// That last check is the one worth having. A layout change that silently
// dropped a family would produce a shorter, perfectly valid document that
// renders, opens and reads correctly right up to the requirement nobody
// noticed was missing.
func TestControlDocumentRendersEveryRequirement(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	body, err := ControlDocumentPDF(reg, PDFOptions{
		Version: "1.2.3", Revision: "deadbee", Generated: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("ControlDocumentPDF: %v", err)
	}
	if !bytes.HasPrefix(body, []byte("%PDF-")) {
		t.Fatal("the output is not a PDF")
	}
	if len(body) < 40_000 {
		t.Errorf("the document is %d bytes, which is too small to hold %d requirements",
			len(body), len(reg.Requirements))
	}

	// fpdf writes text into compressed streams, so the ids are not greppable in
	// the bytes. Re-render with compression off, which is a rendering option
	// rather than a different document.
	plain := renderUncompressed(t, reg)
	for _, q := range reg.Requirements {
		if !bytes.Contains(plain, []byte(q.ID)) {
			t.Errorf("requirement %s does not appear in the rendered document", q.ID)
		}
	}
}

// **No UTF-8 leaks into a cp1252 page.**
//
// The base-14 fonts are Windows-1252, and text written without the translator
// renders as mojibake — "aEURtm" where an em-dash should be — on every page of
// a document somebody hands to an assessor. It is not a crash and not a
// warning; it is a document that looks slightly wrong everywhere, which is
// exactly the kind of defect that ships.
//
// This caught the first draft, where every em-dash in the registry's prose came
// out as three characters.
func TestNoMojibakeInTheRenderedDocument(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plain := renderUncompressed(t, reg)

	// What to look for is the RAW UTF-8, not the mojibake.
	//
	// An earlier version of this test searched the file for the rendered
	// mojibake and passed with the translator switched off, which is worse than
	// having no test at all. The mojibake does not exist in the file: it is what
	// a viewer produces when it reads these bytes as cp1252. Translated, an
	// em-dash is the single byte 0x97; untranslated it is the three bytes
	// E2 80 94, and those in the content stream are the actual defect.
	for name, seq := range map[string][]byte{
		"em dash":     {0xE2, 0x80, 0x94},
		"en dash":     {0xE2, 0x80, 0x93},
		"right quote": {0xE2, 0x80, 0x99},
		"left quote":  {0xE2, 0x80, 0x98},
		"section":     {0xC2, 0xA7},
		"open quote":  {0xE2, 0x80, 0x9C},
	} {
		if bytes.Contains(plain, seq) {
			t.Errorf("the rendered document contains a raw UTF-8 %s (% X) - a string "+
				"reached the page without the cp1252 translator, and every one of them "+
				"renders as mojibake in a viewer", name, seq)
		}
	}
}

// The document names the build on every page, because pages of it get quoted
// and attached to findings on their own, and a paragraph about what Toft
// does is only true of a version.
func TestTheDocumentNamesItsBuild(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plain := renderUncompressedWith(t, reg, PDFOptions{
		Version: "9.9.9-marker", Revision: "cafe123", Generated: time.Now().UTC(),
	})
	if !bytes.Contains(plain, []byte("9.9.9-marker")) {
		t.Error("the rendered document does not name the version it was built from")
	}
	if !bytes.Contains(plain, []byte("cafe123")) {
		t.Error("the rendered document does not name the build revision")
	}
}

func renderUncompressed(t *testing.T, reg *Registry) []byte {
	t.Helper()
	return renderUncompressedWith(t, reg, PDFOptions{
		Version: "1.2.3", Revision: "deadbee", Generated: time.Now().UTC(),
	})
}

func renderUncompressedWith(t *testing.T, reg *Registry, opt PDFOptions) []byte {
	t.Helper()
	body, err := controlDocumentPDF(reg, opt, false)
	if err != nil {
		t.Fatalf("rendering uncompressed: %v", err)
	}
	return body
}
