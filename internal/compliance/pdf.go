package compliance

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
)

// The control document as a file somebody can archive.
//
// # Why it is laid out rather than converted
//
// The product is a single Go binary with no browser and no Docker, on a box
// that may have no outbound HTTPS. That rules out every good HTML-to-PDF path,
// because they are all a browser in a trenchcoat. Pure Go has no real converter
// — the libraries claiming one are toys — so the document is laid out here, in
// code, a page at a time.
//
// # Why plain is the right register anyway
//
// This is not a compromise dressed up as a decision. An assessor wants a dense,
// numbered, stable document they can cite a page of; they do not want a designed
// one, and a designed one would raise the question of whether the design is
// doing persuasive work. Base-14 faces (Helvetica) embed nothing, add nothing to
// the binary and carry no font license — and the moment this document wants a
// real typeface it gains ~300 KB and a licensing question for no assessment
// value.
//
// # What it is not
//
// It is not the nicer artifact. The HTML page renders the same registry and
// will look better, and nothing in the product should imply the PDF is the
// primary rendering. It is the one you can put in a folder.

const (
	pdfMargin   = 18.0
	pdfBodyW    = 210.0 - 2*pdfMargin // A4 portrait, in mm
	pdfFontBody = 9.5
	pdfLeading  = 4.6
)

// PDFOptions carries the few facts the document says about itself.
type PDFOptions struct {
	// Version and Revision of the build. The document is a property of the
	// build, so naming it is what lets a reader tell whether the paragraph they
	// are reading still describes the software in front of them.
	Version  string
	Revision string

	// Generated is the moment the file was produced. It is NOT an assessment
	// date and the cover says so — this document describes a build, not an
	// install, and dating it as though it described an install is exactly the
	// confusion the live report exists to resolve.
	Generated time.Time
}

// ControlDocumentPDF renders the whole control document.
//
// It takes no install state — that is D2's property, and the signature is where
// it is enforced: there is no database handle to reach for, so a later change
// that wanted one would have to change this function's shape and argue for it.
func ControlDocumentPDF(reg *Registry, opt PDFOptions) ([]byte, error) {
	return controlDocumentPDF(reg, opt, true)
}

// controlDocumentPDF is the renderer, with compression as a parameter so a test
// can read the text back. Compression is a property of the file rather than of
// the document, so it is not on PDFOptions where a caller would have to have an
// opinion about it.
func controlDocumentPDF(reg *Registry, opt PDFOptions, compress bool) ([]byte, error) {
	d := &pdfDoc{pdf: fpdf.New("P", "mm", "A4", ""), reg: reg, opt: opt}
	d.pdf.SetCompression(compress)
	d.tr = d.pdf.UnicodeTranslatorFromDescriptor("")
	d.pdf.SetMargins(pdfMargin, pdfMargin, pdfMargin)
	d.pdf.SetAutoPageBreak(true, pdfMargin+8)
	d.pdf.SetTitle("Toft - NIST SP 800-171 Rev 3 control mapping", true)
	d.pdf.SetFooterFunc(d.footer)

	d.cover()
	d.responsibilityKey()
	d.contents()
	d.body()

	var buf bytes.Buffer
	if err := d.pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("rendering the control document: %w", err)
	}
	return buf.Bytes(), nil
}

type pdfDoc struct {
	pdf *fpdf.Fpdf
	reg *Registry
	opt PDFOptions

	// tr converts UTF-8 into the base-14 fonts' Windows-1252 encoding.
	//
	// It is not optional and its absence is not subtle: the registry's prose is
	// full of em-dashes and curly quotes, and without this every one of them
	// renders as mojibake — "â€"" in the middle of a sentence, on every page of
	// a document somebody is going to hand to an assessor. That is the cost of
	// base-14 faces, and it is still cheaper than embedding a font.
	//
	// Every string written to the page goes through d.t. There is no second
	// path, because a second path is where the one untranslated string lives.
	tr func(string) string
}

// t translates one string for the page. Named short because it wraps every
// piece of text in this file and a longer name would bury the content.
func (d *pdfDoc) t(s string) string { return d.tr(s) }

// footer carries the page number and the build.
//
// The build is on **every page** rather than only the cover, because pages of
// this document will be photocopied, quoted and attached to findings on their
// own, and a paragraph about what Toft does is only true of a version.
func (d *pdfDoc) footer() {
	d.pdf.SetY(-14)
	d.pdf.SetFont("Helvetica", "", 7.5)
	d.pdf.SetTextColor(120, 120, 120)
	left := "Toft " + d.opt.Version
	if d.opt.Revision != "" {
		left += " (" + d.opt.Revision + ")"
	}
	d.pdf.CellFormat(pdfBodyW/2, 6, d.t(left), "", 0, "L", false, 0, "")
	d.pdf.CellFormat(pdfBodyW/2, 6, fmt.Sprintf("Page %d", d.pdf.PageNo()), "", 0, "R", false, 0, "")
	d.pdf.SetTextColor(0, 0, 0)
}

func (d *pdfDoc) cover() {
	d.pdf.AddPage()
	d.pdf.SetY(70)

	d.pdf.SetFont("Helvetica", "B", 22)
	d.pdf.MultiCell(pdfBodyW, 9, d.t("NIST SP 800-171 Revision 3"), "", "L", false)
	d.pdf.Ln(2)
	d.pdf.SetFont("Helvetica", "", 15)
	d.pdf.MultiCell(pdfBodyW, 7, d.t("How Toft implements each requirement"), "", "L", false)
	d.pdf.Ln(10)

	c := d.reg.Coverage()
	d.pdf.SetFont("Helvetica", "", pdfFontBody)
	d.para(fmt.Sprintf(
		"%d requirements across %d families. Toft implements %d of them; %d are shared "+
			"with the organization operating this install; %d are inherited from the layer "+
			"below it — the host, the cluster, the facility or the identity provider — and %d "+
			"have nothing to do with Toft at all.",
		c.Requirements, c.Families,
		c.ByResponsibility[RespPlatform], c.ByResponsibility[RespShared],
		c.ByResponsibility[RespInherited], c.ByResponsibility[RespCustomer]))

	d.para("That split is the document. A claim to satisfy all " +
		fmt.Sprint(c.Requirements) + " requirements would be worthless to an assessor: " +
		"Toft is software, and it has no part in personnel screening, physical " +
		"protection or awareness training. The entries claiming nothing are what make the " +
		"entries claiming something credible.")

	d.para("Requirement identifiers, titles and requirement text are taken verbatim from " +
		"NIST's OSCAL release of SP 800-171 Revision 3, which is a work of the United " +
		"States Government. Everything under \"How Toft addresses it\" and \"What you " +
		"must still do\" is this project's own assertion about its software, and is what an " +
		"assessor should test rather than take on trust.")

	d.pdf.Ln(6)
	d.rule()
	d.pdf.Ln(4)
	d.pdf.SetFont("Helvetica", "B", pdfFontBody)
	d.pdf.MultiCell(pdfBodyW, pdfLeading, d.t("This document describes a build, not an install."), "", "L", false)
	d.pdf.SetFont("Helvetica", "", pdfFontBody)
	d.para("What Toft does about a requirement is the same on every install of version " +
		d.opt.Version + ". What YOUR install is configured to do is a different document — " +
		"the live compliance report, which reads this machine's own settings and is the one " +
		"that carries an assessment date. This file is dated " +
		d.opt.Generated.Format("2 January 2006") + " only to say when it was produced.")

	d.pdf.Ln(4)
	d.pdf.SetFont("Helvetica", "", 8)
	d.pdf.SetTextColor(110, 110, 110)
	d.pdf.MultiCell(pdfBodyW, 4, d.t(
		"Audit support. Not a certification, not an attestation, and not a substitute for "+
			"an assessment."), "", "L", false)
	d.pdf.SetTextColor(0, 0, 0)
}

func (d *pdfDoc) responsibilityKey() {
	d.pdf.AddPage()
	d.heading("How to read this document")

	for _, r := range Responsibilities {
		d.pdf.SetFont("Helvetica", "B", pdfFontBody)
		d.pdf.MultiCell(pdfBodyW, pdfLeading, d.t(strings.ToUpper(r[:1])+r[1:]), "", "L", false)
		d.pdf.SetFont("Helvetica", "", pdfFontBody)
		d.pdf.MultiCell(pdfBodyW, pdfLeading, d.t(responsibilityCopy[r]), "", "L", false)
		d.pdf.Ln(3)
	}

	c := d.reg.Coverage()
	d.pdf.Ln(2)
	d.heading("Organization-defined parameters")
	d.para(fmt.Sprintf(
		"SP 800-171 asks the organization to define %d parameters — a period here, a "+
			"frequency there. %d of them are values a Toft install already holds as a "+
			"setting, and the live compliance report binds each to the setting that answers "+
			"it rather than asking anybody to write the number down twice. %d more are noted "+
			"as the organization's, and %d are not addressed by this mapping at all.",
		c.ODPs, c.ODPsBound, c.ODPsNoted, c.ODPsUnbound))
	d.para("That last figure is printed because a coverage number you cannot see is a " +
		"coverage number you cannot trust.")
}

var responsibilityCopy = map[string]string{
	RespPlatform: "Toft implements it. The install's own configuration is the evidence, " +
		"and the live report reads it.",
	RespShared: "Toft provides the mechanism; the organization operating the install " +
		"configures and runs it. Both halves are stated, and the second one is yours.",
	RespInherited: "Satisfied below Toft — the host, the cluster, the hosting facility, " +
		"or the identity provider. Toft neither implements nor weakens it, and an " +
		"assessor should look at that layer rather than at this software.",
	RespCustomer: "Toft has no part in it. Said plainly rather than padded out, so that " +
		"the requirements it does answer can be believed.",
}

func (d *pdfDoc) contents() {
	d.pdf.AddPage()
	d.heading("Contents")
	d.pdf.SetFont("Helvetica", "", pdfFontBody)

	for _, f := range d.reg.Catalog.Families {
		n := 0
		for _, q := range d.reg.Requirements {
			if q.Family == f.Code {
				n++
			}
		}
		d.pdf.CellFormat(20, 5.6, d.t(f.Code), "", 0, "L", false, 0, "")
		d.pdf.CellFormat(pdfBodyW-40, 5.6, d.t(f.Title), "", 0, "L", false, 0, "")
		d.pdf.CellFormat(20, 5.6, d.t(fmt.Sprintf("%d", n)), "", 1, "R", false, 0, "")
	}
}

func (d *pdfDoc) body() {
	family := ""
	for _, q := range d.reg.Requirements {
		if q.Family != family {
			family = q.Family
			d.pdf.AddPage()
			d.pdf.SetFont("Helvetica", "B", 14)
			d.pdf.MultiCell(pdfBodyW, 7, d.t(q.Family+"  "+q.FamilyTitle), "", "L", false)
			d.pdf.Ln(2)
		}
		d.requirement(q)
	}
}

func (d *pdfDoc) requirement(q Requirement) {
	// Keep a requirement's opening on one page. A heading stranded at the foot
	// of a page is the one layout failure that makes a long document hard to
	// cite, because the reader loses which requirement they are inside.
	if d.pdf.GetY() > 230 {
		d.pdf.AddPage()
	}

	d.pdf.SetFont("Helvetica", "B", 11)
	d.pdf.MultiCell(pdfBodyW, 5.8, d.t(q.ID+"  "+q.Title), "", "L", false)

	d.pdf.SetFont("Helvetica", "I", 8.5)
	d.pdf.SetTextColor(110, 110, 110)
	d.pdf.MultiCell(pdfBodyW, 4.2, d.t(strings.ToUpper(q.Responsibility)+" — "+q.Summary), "", "L", false)
	d.pdf.SetTextColor(0, 0, 0)
	d.pdf.Ln(1.5)

	d.label("What NIST requires")
	d.pdf.SetFont("Helvetica", "", pdfFontBody)
	for _, st := range q.Statement {
		indent := float64(st.Depth) * 5
		d.pdf.SetX(pdfMargin + indent)
		text := st.Prose
		if st.Label != "" {
			text = st.Label + "  " + text
		}
		d.pdf.MultiCell(pdfBodyW-indent, pdfLeading, d.t(text), "", "L", false)
	}
	d.pdf.Ln(1)

	if q.Mechanism != "" {
		d.label("How Toft addresses it")
		d.para(q.Mechanism)
	}
	if q.Customer != "" {
		d.label("What you must still do")
		d.para(q.Customer)
	}

	if bound := boundParameters(q); len(bound) > 0 {
		d.label("Organization-defined parameters answered by an install setting")
		d.pdf.SetFont("Helvetica", "", 8.5)
		for _, b := range bound {
			d.pdf.MultiCell(pdfBodyW, 4.2, d.t("  "+b), "", "L", false)
		}
		d.pdf.Ln(1)
	}

	if len(q.Refs) > 0 {
		d.pdf.SetFont("Helvetica", "I", 8)
		d.pdf.SetTextColor(120, 120, 120)
		d.pdf.MultiCell(pdfBodyW, 4, d.t("Where this is written down in Toft: "+
			strings.Join(q.Refs, " · ")), "", "L", false)
		d.pdf.SetTextColor(0, 0, 0)
	}

	d.pdf.Ln(3)
	d.rule()
	d.pdf.Ln(3)
}

// boundParameters names the parameters this requirement answers from a setting,
// with the setting that answers each. Only the bound ones: an unaddressed
// parameter is a fact about the live report's coverage rather than about the
// mapping, and it is reported there.
func boundParameters(q Requirement) []string {
	var out []string
	for _, o := range q.ODPs {
		b, ok := q.Bindings[o.ID]
		if !ok {
			continue
		}
		switch {
		case b.Field != "":
			out = append(out, o.ID+" — "+b.Field)
		case b.Value != "":
			out = append(out, o.ID+" — "+b.Value)
		}
	}
	return out
}

func (d *pdfDoc) heading(s string) {
	d.pdf.SetFont("Helvetica", "B", 14)
	d.pdf.MultiCell(pdfBodyW, 7, d.t(s), "", "L", false)
	d.pdf.Ln(2)
	d.pdf.SetFont("Helvetica", "", pdfFontBody)
}

func (d *pdfDoc) label(s string) {
	d.pdf.SetFont("Helvetica", "B", 8.5)
	d.pdf.MultiCell(pdfBodyW, 4.4, d.t(s), "", "L", false)
	d.pdf.SetFont("Helvetica", "", pdfFontBody)
}

func (d *pdfDoc) para(s string) {
	d.pdf.SetFont("Helvetica", "", pdfFontBody)
	d.pdf.MultiCell(pdfBodyW, pdfLeading, d.t(collapse(s)), "", "L", false)
	d.pdf.Ln(2)
}

func (d *pdfDoc) rule() {
	d.pdf.SetDrawColor(210, 210, 210)
	y := d.pdf.GetY()
	d.pdf.Line(pdfMargin, y, pdfMargin+pdfBodyW, y)
	d.pdf.SetDrawColor(0, 0, 0)
}

// collapse folds the registry's wrapped YAML prose into single-spaced running
// text. The mapping is authored at 80 columns for review, and those newlines
// are an artefact of the file rather than of the sentence.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
