// gendoc generates dicomqr-user-manual.docx and dicomqr-user-manual.md from
// embedded content. Run with: go run ./gendoc
package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image/png"
	"os"
	"strings"
	"time"
)

// ── XML helpers ───────────────────────────────────────────────────────────────

func esc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// ── Document builder ──────────────────────────────────────────────────────────

type Doc struct {
	b              strings.Builder
	logoCX, logoCY int
	hasLogo        bool
}

func (d *Doc) w(s string) { d.b.WriteString(s) }

// runs converts text containing `backtick` spans into Word XML runs.
func (d *Doc) runs(text string) {
	parts := strings.Split(text, "`")
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i%2 == 0 {
			d.w(`<w:r><w:t xml:space="preserve">` + esc(p) + `</w:t></w:r>`)
		} else {
			d.w(`<w:r><w:rPr>` +
				`<w:rFonts w:ascii="Courier New" w:hAnsi="Courier New"/>` +
				`<w:sz w:val="18"/><w:szCs w:val="18"/>` +
				`<w:shd w:val="clear" w:color="auto" w:fill="EBEBEB"/>` +
				`</w:rPr><w:t xml:space="preserve">` + esc(p) + `</w:t></w:r>`)
		}
	}
}

// Cover renders the title page: optional centred logo, title, subtitle, date,
// description, and a developer attribution line. The developer paragraph
// carries the cover section properties (page size, margins, vertical centring,
// no header/footer) so the running header and footer begin on the next section.
func (d *Doc) Cover(title, subtitle, date, description string) {
	if d.hasLogo {
		d.w(`<w:p><w:pPr><w:spacing w:before="0" w:after="240"/><w:jc w:val="center"/></w:pPr>`)
		d.w(`<w:r><w:drawing><wp:inline distT="0" distB="0" distL="0" distR="0">`)
		d.w(fmt.Sprintf(`<wp:extent cx="%d" cy="%d"/>`, d.logoCX, d.logoCY))
		d.w(`<wp:effectExtent l="0" t="0" r="0" b="0"/><wp:docPr id="1" name="Logo"/>`)
		d.w(`<wp:cNvGraphicFramePr><a:graphicFrameLocks noChangeAspect="1"/></wp:cNvGraphicFramePr>`)
		d.w(`<a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture">`)
		d.w(`<pic:pic><pic:nvPicPr><pic:cNvPr id="0" name="logo.png"/><pic:cNvPicPr/></pic:nvPicPr>`)
		d.w(`<pic:blipFill><a:blip r:embed="rId6"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill>`)
		d.w(`<pic:spPr><a:xfrm><a:off x="0" y="0"/>`)
		d.w(fmt.Sprintf(`<a:ext cx="%d" cy="%d"/>`, d.logoCX, d.logoCY))
		d.w(`</a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr>`)
		d.w(`</pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>`)
	}
	// Title.
	d.w(`<w:p><w:pPr><w:spacing w:before="0" w:after="120"/><w:jc w:val="center"/></w:pPr>` +
		`<w:r><w:rPr><w:b/><w:color w:val="2E74B5"/><w:sz w:val="72"/><w:szCs w:val="72"/></w:rPr>` +
		`<w:t xml:space="preserve">` + esc(title) + `</w:t></w:r></w:p>`)
	// Subtitle.
	d.w(`<w:p><w:pPr><w:spacing w:before="0" w:after="240"/><w:jc w:val="center"/></w:pPr>` +
		`<w:r><w:rPr><w:color w:val="595959"/><w:sz w:val="32"/><w:szCs w:val="32"/></w:rPr>` +
		`<w:t xml:space="preserve">` + esc(subtitle) + `</w:t></w:r></w:p>`)
	// Date.
	d.w(`<w:p><w:pPr><w:spacing w:before="0" w:after="240"/><w:jc w:val="center"/></w:pPr>` +
		`<w:r><w:rPr><w:color w:val="595959"/><w:sz w:val="24"/><w:szCs w:val="24"/></w:rPr>` +
		`<w:t xml:space="preserve">` + esc(date) + `</w:t></w:r></w:p>`)
	// Description.
	d.w(`<w:p><w:pPr><w:spacing w:before="0" w:after="480"/><w:jc w:val="center"/></w:pPr>` +
		`<w:r><w:t xml:space="preserve">` + esc(description) + `</w:t></w:r></w:p>`)

	// Developer line — carries the cover section properties so the body section
	// (with running header/footer) begins on the following page.
	dev := "Jeffrey Leal  ·  jeffrey.leal@gmail.com  ·  github.com/jeffrey-leal/dicomqr"
	d.w(`<w:p><w:pPr><w:spacing w:before="0" w:after="0"/><w:jc w:val="center"/>`)
	d.w(`<w:sectPr>` +
		`<w:pgSz w:w="12240" w:h="15840"/>` +
		`<w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" w:header="720" w:footer="720" w:gutter="0"/>` +
		`<w:vAlign w:val="center"/>` +
		`</w:sectPr>`)
	d.w(`</w:pPr>`)
	d.w(`<w:r><w:rPr><w:sz w:val="20"/><w:szCs w:val="20"/></w:rPr>` +
		`<w:t xml:space="preserve">` + esc(dev) + `</w:t></w:r>`)
	d.w(`</w:p>`)
}

func (d *Doc) H1(text string) {
	d.w(`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>` + esc(text) + `</w:t></w:r></w:p>`)
}
func (d *Doc) H2(text string) {
	d.w(`<w:p><w:pPr><w:pStyle w:val="Heading2"/></w:pPr><w:r><w:t>` + esc(text) + `</w:t></w:r></w:p>`)
}
func (d *Doc) H3(text string) {
	d.w(`<w:p><w:pPr><w:pStyle w:val="Heading3"/></w:pPr><w:r><w:t>` + esc(text) + `</w:t></w:r></w:p>`)
}
func (d *Doc) P(text string) {
	d.w(`<w:p>`)
	d.runs(text)
	d.w(`</w:p>`)
}
func (d *Doc) Bullet(text string) {
	d.w(`<w:p><w:pPr><w:pStyle w:val="ListBullet"/></w:pPr>`)
	d.runs(text)
	d.w(`</w:p>`)
}
func (d *Doc) Code(text string) {
	for _, line := range strings.Split(text, "\n") {
		d.w(`<w:p><w:pPr><w:pStyle w:val="Code"/></w:pPr>`)
		if line != "" {
			d.w(`<w:r><w:t xml:space="preserve">` + esc(line) + `</w:t></w:r>`)
		}
		d.w(`</w:p>`)
	}
}
func (d *Doc) Space() {
	d.w(`<w:p><w:pPr><w:spacing w:after="0"/></w:pPr></w:p>`)
}
func (d *Doc) PageBreak() {
	d.w(`<w:p><w:r><w:br w:type="page"/></w:r></w:p>`)
}

// tableN is the shared implementation for 2-, 3-, and 4-column tables.
// rows[0] is the header row; subsequent rows alternate light shading.
func (d *Doc) tableN(colWidths []int, rows [][]string) {
	border := `w:val="single" w:sz="4" w:space="0" w:color="BBBBBB"`
	d.w(`<w:tbl><w:tblPr>`)
	d.w(`<w:tblW w:w="0" w:type="auto"/>`)
	d.w(`<w:tblBorders>`)
	d.w(`<w:top ` + border + `/><w:left ` + border + `/><w:bottom ` + border + `/>` +
		`<w:right ` + border + `/><w:insideH ` + border + `/><w:insideV ` + border + `/>`)
	d.w(`</w:tblBorders>`)
	d.w(`<w:tblCellMar>` +
		`<w:top w:w="80" w:type="dxa"/><w:left w:w="140" w:type="dxa"/>` +
		`<w:bottom w:w="80" w:type="dxa"/><w:right w:w="140" w:type="dxa"/>` +
		`</w:tblCellMar>`)
	d.w(`</w:tblPr><w:tblGrid>`)
	for _, cw := range colWidths {
		d.w(fmt.Sprintf(`<w:gridCol w:w="%d"/>`, cw))
	}
	d.w(`</w:tblGrid>`)

	for i, row := range rows {
		isHdr := i == 0
		d.w(`<w:tr>`)
		for j, text := range row {
			cw := fmt.Sprintf("%d", colWidths[j])
			d.w(`<w:tc><w:tcPr><w:tcW w:w="` + cw + `" w:type="dxa"/>`)
			if isHdr {
				d.w(`<w:shd w:val="clear" w:color="auto" w:fill="2E74B5"/>`)
			} else if i%2 == 0 {
				d.w(`<w:shd w:val="clear" w:color="auto" w:fill="F5F5F5"/>`)
			}
			d.w(`</w:tcPr><w:p>`)
			if isHdr {
				d.w(`<w:r><w:rPr><w:b/><w:color w:val="FFFFFF"/>` +
					`<w:sz w:val="20"/><w:szCs w:val="20"/>` +
					`</w:rPr><w:t xml:space="preserve">` + esc(text) + `</w:t></w:r>`)
			} else {
				parts := strings.Split(text, "`")
				for k, p := range parts {
					if p == "" {
						continue
					}
					if k%2 == 0 {
						d.w(`<w:r><w:t xml:space="preserve">` + esc(p) + `</w:t></w:r>`)
					} else {
						d.w(`<w:r><w:rPr>` +
							`<w:rFonts w:ascii="Courier New" w:hAnsi="Courier New"/>` +
							`<w:sz w:val="18"/><w:szCs w:val="18"/>` +
							`<w:shd w:val="clear" w:color="auto" w:fill="EBEBEB"/>` +
							`</w:rPr><w:t xml:space="preserve">` + esc(p) + `</w:t></w:r>`)
					}
				}
			}
			d.w(`</w:p></w:tc>`)
		}
		d.w(`</w:tr>`)
	}
	d.w(`</w:tbl><w:p/>`)
}

type Row struct{ A, B string }

func (d *Doc) Table(rows []Row) {
	data := make([][]string, len(rows))
	for i, r := range rows {
		data[i] = []string{r.A, r.B}
	}
	d.tableN([]int{2700, 6300}, data)
}

type Row3 struct{ A, B, C string }

func (d *Doc) Table3(rows []Row3) {
	data := make([][]string, len(rows))
	for i, r := range rows {
		data[i] = []string{r.A, r.B, r.C}
	}
	d.tableN([]int{2200, 1600, 5200}, data)
}

type Row4 struct{ A, B, C, D string }

func (d *Doc) Table4(rows []Row4) {
	data := make([][]string, len(rows))
	for i, r := range rows {
		data[i] = []string{r.A, r.B, r.C, r.D}
	}
	d.tableN([]int{2500, 1800, 1200, 3500}, data)
}

// ── Formatter interface ───────────────────────────────────────────────────────

type Formatter interface {
	Cover(title, subtitle, date, description string)
	H1(string)
	H2(string)
	H3(string)
	P(string)
	Bullet(string)
	Code(string)
	Table([]Row)
	Table3([]Row3)
	Table4([]Row4)
	Space()
	PageBreak()
}

// ── Markdown builder ──────────────────────────────────────────────────────────

type MDDoc struct{ b strings.Builder }

func (d *MDDoc) w(s string) { d.b.WriteString(s) }

func (d *MDDoc) Cover(title, subtitle, date, description string) {
	d.w("# " + title + "\n\n")
	d.w("**" + subtitle + "**\n\n")
	d.w(date + "\n\n")
	d.w(description + "\n\n---\n\n")
}
func (d *MDDoc) H1(text string)     { d.w("\n## " + text + "\n\n") }
func (d *MDDoc) H2(text string)     { d.w("\n### " + text + "\n\n") }
func (d *MDDoc) H3(text string)     { d.w("\n#### " + text + "\n\n") }
func (d *MDDoc) P(text string)      { d.w(text + "\n\n") }
func (d *MDDoc) Bullet(text string) { d.w("- " + text + "\n") }
func (d *MDDoc) Space()             {}
func (d *MDDoc) PageBreak()         { d.w("\n---\n\n") }
func (d *MDDoc) Code(text string)   { d.w("```\n" + text + "\n```\n\n") }

func (d *MDDoc) Table(rows []Row) {
	if len(rows) == 0 {
		return
	}
	d.w("| " + rows[0].A + " | " + rows[0].B + " |\n")
	d.w("|---|---|\n")
	for _, row := range rows[1:] {
		d.w("| " + row.A + " | " + row.B + " |\n")
	}
	d.w("\n")
}

func (d *MDDoc) Table3(rows []Row3) {
	if len(rows) == 0 {
		return
	}
	d.w("| " + rows[0].A + " | " + rows[0].B + " | " + rows[0].C + " |\n")
	d.w("|---|---|---|\n")
	for _, row := range rows[1:] {
		d.w("| " + row.A + " | " + row.B + " | " + row.C + " |\n")
	}
	d.w("\n")
}

func (d *MDDoc) Table4(rows []Row4) {
	if len(rows) == 0 {
		return
	}
	d.w("| " + rows[0].A + " | " + rows[0].B + " | " + rows[0].C + " | " + rows[0].D + " |\n")
	d.w("|---|---|---|---|\n")
	for _, row := range rows[1:] {
		d.w("| " + row.A + " | " + row.B + " | " + row.C + " | " + row.D + " |\n")
	}
	d.w("\n")
}

// ── Static XML parts ──────────────────────────────────────────────────────────

const contentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Default Extension="png" ContentType="image/png"/>
  <Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
  <Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>
  <Override PartName="/word/numbering.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.numbering+xml"/>
  <Override PartName="/word/settings.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.settings+xml"/>
  <Override PartName="/word/header1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.header+xml"/>
  <Override PartName="/word/footer1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.footer+xml"/>
  <Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>
</Types>`

const rootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>
</Relationships>`

const wordRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/numbering" Target="numbering.xml"/>
  <Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/settings" Target="settings.xml"/>
  <Relationship Id="rId4" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/header" Target="header1.xml"/>
  <Relationship Id="rId5" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer" Target="footer1.xml"/>
  <Relationship Id="rId6" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="media/image1.png"/>
</Relationships>`

const settings = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:settings xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:defaultTabStop w:val="720"/>
  <w:compat>
    <w:compatSetting w:name="compatibilityMode" w:uri="http://schemas.microsoft.com/office/word" w:val="15"/>
  </w:compat>
</w:settings>`

const numbering = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:numbering xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:abstractNum w:abstractNumId="0">
    <w:multiLevelType w:val="hybridMultilevel"/>
    <w:lvl w:ilvl="0">
      <w:start w:val="1"/>
      <w:numFmt w:val="bullet"/>
      <w:lvlText w:val="&#x2022;"/>
      <w:lvlJc w:val="left"/>
      <w:pPr><w:ind w:left="720" w:hanging="360"/></w:pPr>
      <w:rPr><w:rFonts w:ascii="Arial" w:hAnsi="Arial"/><w:sz w:val="22"/></w:rPr>
    </w:lvl>
  </w:abstractNum>
  <w:num w:numId="1">
    <w:abstractNumId w:val="0"/>
  </w:num>
</w:numbering>`

// header1: software name left, page number right, horizontal rule below.
const header1 = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:hdr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:p>
    <w:pPr>
      <w:pBdr><w:bottom w:val="single" w:sz="6" w:space="1" w:color="auto"/></w:pBdr>
      <w:tabs><w:tab w:val="right" w:pos="9360"/></w:tabs>
      <w:spacing w:after="0"/>
    </w:pPr>
    <w:r><w:t>dicomqr</w:t></w:r>
    <w:r><w:tab/></w:r>
    <w:r><w:t xml:space="preserve">Page: </w:t></w:r>
    <w:r><w:fldChar w:fldCharType="begin"/></w:r>
    <w:r><w:instrText xml:space="preserve"> PAGE </w:instrText></w:r>
    <w:r><w:fldChar w:fldCharType="end"/></w:r>
  </w:p>
</w:hdr>`

// footer1: application description left, horizontal rule above.
const footer1 = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:ftr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:p>
    <w:pPr>
      <w:pBdr><w:top w:val="single" w:sz="6" w:space="1" w:color="auto"/></w:pBdr>
      <w:spacing w:after="0"/>
      <w:jc w:val="left"/>
    </w:pPr>
    <w:r><w:rPr><w:sz w:val="16"/><w:szCs w:val="16"/></w:rPr><w:t>dicomqr &#x2014; DICOM Query &amp; Retrieve Tool</w:t></w:r>
  </w:p>
</w:ftr>`

const coreProps = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties
  xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties"
  xmlns:dc="http://purl.org/dc/elements/1.1/">
  <dc:title>dicomqr User Manual</dc:title>
  <dc:creator>Jeffrey Leal</dc:creator>
</cp:coreProperties>`

const stylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:docDefaults>
    <w:rPrDefault>
      <w:rPr>
        <w:rFonts w:ascii="Calibri" w:hAnsi="Calibri" w:cs="Calibri"/>
        <w:sz w:val="22"/><w:szCs w:val="22"/>
        <w:lang w:val="en-US"/>
      </w:rPr>
    </w:rPrDefault>
    <w:pPrDefault>
      <w:pPr>
        <w:spacing w:after="160" w:line="259" w:lineRule="auto"/>
      </w:pPr>
    </w:pPrDefault>
  </w:docDefaults>

  <w:style w:type="paragraph" w:default="1" w:styleId="Normal">
    <w:name w:val="Normal"/>
  </w:style>

  <w:style w:type="paragraph" w:styleId="Heading1">
    <w:name w:val="heading 1"/>
    <w:basedOn w:val="Normal"/>
    <w:next w:val="Normal"/>
    <w:pPr>
      <w:keepNext/>
      <w:spacing w:before="480" w:after="120"/>
      <w:outlineLvl w:val="0"/>
    </w:pPr>
    <w:rPr>
      <w:b/>
      <w:color w:val="2E74B5"/>
      <w:sz w:val="40"/><w:szCs w:val="40"/>
    </w:rPr>
  </w:style>

  <w:style w:type="paragraph" w:styleId="Heading2">
    <w:name w:val="heading 2"/>
    <w:basedOn w:val="Normal"/>
    <w:next w:val="Normal"/>
    <w:pPr>
      <w:keepNext/>
      <w:spacing w:before="320" w:after="80"/>
      <w:outlineLvl w:val="1"/>
    </w:pPr>
    <w:rPr>
      <w:b/>
      <w:color w:val="2E74B5"/>
      <w:sz w:val="28"/><w:szCs w:val="28"/>
    </w:rPr>
  </w:style>

  <w:style w:type="paragraph" w:styleId="Heading3">
    <w:name w:val="heading 3"/>
    <w:basedOn w:val="Normal"/>
    <w:next w:val="Normal"/>
    <w:pPr>
      <w:keepNext/>
      <w:spacing w:before="200" w:after="40"/>
      <w:outlineLvl w:val="2"/>
    </w:pPr>
    <w:rPr>
      <w:b/>
      <w:color w:val="595959"/>
      <w:sz w:val="24"/><w:szCs w:val="24"/>
    </w:rPr>
  </w:style>

  <w:style w:type="paragraph" w:styleId="Code">
    <w:name w:val="Code"/>
    <w:basedOn w:val="Normal"/>
    <w:pPr>
      <w:spacing w:before="40" w:after="40" w:line="240" w:lineRule="auto"/>
      <w:ind w:left="400" w:right="400"/>
      <w:shd w:val="clear" w:color="auto" w:fill="F2F2F2"/>
      <w:pBdr>
        <w:left w:val="single" w:sz="12" w:space="4" w:color="AAAAAA"/>
      </w:pBdr>
    </w:pPr>
    <w:rPr>
      <w:rFonts w:ascii="Courier New" w:hAnsi="Courier New" w:cs="Courier New"/>
      <w:sz w:val="18"/><w:szCs w:val="18"/>
    </w:rPr>
  </w:style>

  <w:style w:type="paragraph" w:styleId="ListBullet">
    <w:name w:val="List Bullet"/>
    <w:basedOn w:val="Normal"/>
    <w:pPr>
      <w:numPr>
        <w:ilvl w:val="0"/>
        <w:numId w:val="1"/>
      </w:numPr>
      <w:spacing w:before="0" w:after="80"/>
    </w:pPr>
  </w:style>
</w:styles>`

// ── Document content ──────────────────────────────────────────────────────────

func buildContent(d Formatter) {

	d.Cover("dicomqr", "User Manual  v1.4.0",
		time.Now().Format("January 2, 2006"),
		"A Windows desktop application for querying, retrieving, and managing DICOM medical imaging studies.")

	// 1. Overview
	d.H1("1  Overview")
	d.P("dicomqr is a Windows desktop application for querying, retrieving, and managing DICOM medical imaging studies. It communicates with PACS servers using standard DICOM networking services and includes tools for browsing, previewing, importing, routing, and deleting local DICOM files.")
	d.P("Key capabilities:")
	d.Bullet("Connect to any DICOM-compatible PACS server using configurable server profiles")
	d.Bullet("Search for studies by patient name, patient ID, accession number, date range, and modality")
	d.Bullet("Browse query results in an expandable Patient > Study > Series tree, sorted alphabetically, chronologically, and numerically")
	d.Bullet("Retrieve entire studies or individual series to a local folder using C-MOVE or C-GET")
	d.Bullet("Automatically organise downloaded files by patient, study, and series")
	d.Bullet("Query a Modality Worklist server independently of the active PACS connection")
	d.Bullet("Browse local DICOM files in the download folder; push them to any PACS via C-STORE or delete them")
	d.Bullet("Preview DICOM images in the built-in viewer with interactive window/level, zoom and pan, modality-specific W/L presets, colour maps for PET/SPECT, DICOM annotation overlays, and study overview grids")
	d.Bullet("Import DICOM files from external folders into the organised download folder")
	d.Bullet("Support for multiple saved server profiles with independent connection and retrieve settings")
	d.Bullet("Optionally request uncompressed pixel data transfer per server profile, ensuring the built-in viewer can display all received images regardless of how the PACS stores them")
	d.Bullet("Automatic wildcard search — trailing `*` appended to text fields so partial names match without manual wildcarding")
	d.Bullet("Customisable appearance — selection colour, font style, external viewer path, and window size are remembered between sessions")

	// 2. Getting Started
	d.H1("2  Getting Started")

	d.H2("2.1  System Requirements")
	d.Bullet("Windows 10 or later (64-bit)")
	d.Bullet("Network access to a DICOM PACS server")
	d.Bullet("A configured PACS that accepts DICOM associations from this workstation")

	d.H2("2.2  PACS Registration")
	d.P("Before connecting, the PACS administrator must register this workstation as a known Application Entity (AE). The required details are shown in Help > Client info… once the application is running:")
	d.Table3([]Row3{
		{"Field", "Default", "Description"},
		{"Local AE Title", "DICOMQR", "The name the PACS uses to identify this workstation."},
		{"Local SCP port", "11112", "The TCP port on which dicomqr listens for incoming file transfers."},
		{"Local IP", "Detected automatically", "The IP address of this workstation as seen by the PACS."},
	})
	d.P("The AE Title and port can be changed in File > Preferences… > Retrieve.")
	d.P("For C-MOVE file retrieval to work, the PACS must be able to initiate an outbound TCP connection from its own network address to the Local IP and Local SCP port shown in Client info. Ensure that any firewall on this workstation permits inbound connections on that port.")

	d.H2("2.3  Starting the Application")
	d.P("Double-click `dicomqr.exe` to launch the application. The main window opens with an empty results tree and the status bar showing the application version. The window is restored to the size it had when last closed.")

	// 3. The Main Window
	d.H1("3  The Main Window")
	d.P("The main window is divided into a connection panel at the top, a tab area in the centre, and a status bar at the bottom.")
	d.P("Connection panel — the topmost area, visible from all tabs. Left side: server profile selector, Filters button, Search button. Right side: Connect, Disconnect, and Test (C-ECHO) buttons. A second row shows the SCP status indicator.")
	d.P("Tab area — four tabs:")
	d.Table([]Row{
		{"Tab", "Purpose"},
		{"PACS Query", "Search a remote PACS and retrieve studies."},
		{"Worklist", "Query a Modality Worklist server for scheduled procedures."},
		{"Local Browse", "Browse, preview, push, and delete files in the download folder."},
		{"Import", "Copy DICOM files from an external folder into the download folder."},
	})
	d.P("Status bar — the bottom strip. A coloured LED indicator precedes the status text. A clock shows the current date and time. A progress bar appears during queries and retrieves.")

	// 4. Connecting
	d.H1("4  Connecting to a PACS Server")

	d.H2("4.1  Server Profiles")
	d.P("A server profile stores the connection details for one PACS destination. Profiles are managed in File > Preferences… > Connections. Each profile records:")
	d.Table([]Row{
		{"Field", "Description"},
		{"Profile name", "A label used to identify the server in the dropdown."},
		{"Remote AE Title", "The Application Entity Title of the PACS (case-sensitive)."},
		{"Host", "The hostname or IP address of the PACS server."},
		{"Port", "The TCP port on which the PACS listens (typically 104 or 11112)."},
		{"Info model", "The DICOM Query/Retrieve information model. `study` = Study Root (most common). `patient` = Patient Root. `patient-study-only` = legacy retired model used by some older systems; SERIES-level queries are not available with this model."},
		{"Retrieve method", "C-MOVE (default) instructs the PACS to push files to the local C-STORE SCP listener. C-GET requests that the PACS return files over the same association — no inbound port or PACS-side destination registration is required. Auto tries C-GET first and falls back to C-MOVE if the PACS rejects it."},
		{"Connect timeout", "Seconds to wait for the initial C-ECHO before reporting a failure. Default: 10 s."},
		{"Transfer", "When 'Request uncompressed transfer syntax only' is ticked, dicomqr restricts its A-ASSOCIATE negotiation to Explicit VR Little Endian and Implicit VR Little Endian only. A conformant PACS must transcode compressed pixel data before sending. Useful when the PACS stores data in JPEG 2000 or JPEG-LS format that the built-in viewer cannot decode. Leave unticked unless needed — some PACS systems cannot transcode and will fail the transfer."},
	})
	d.P("The first profile in the list is selected by default when the application starts.")

	d.H2("4.2  Connection Indicators")
	d.P("The coloured LED to the left of the status bar text shows the connection state:")
	d.Table([]Row{
		{"Colour", "Meaning"},
		{"Gray", "Disconnected"},
		{"Amber", "Connecting (C-ECHO in progress)"},
		{"Green", "Connected"},
	})
	d.P("A second indicator in the connection panel row below the server selector shows the embedded C-STORE SCP state:")
	d.Table([]Row{
		{"Colour", "Meaning"},
		{"Gray", "Not running"},
		{"Green", "Listening — shows the bound address and local AE Title"},
		{"Red", "Error — shows the error reason"},
	})

	d.H2("4.3  Connecting")
	d.P("Select a server profile from the dropdown in the server row, then click Connect (or select File > Connect). The application sends a C-ECHO to verify basic DICOM connectivity. If the C-ECHO succeeds, dicomqr starts the embedded C-STORE listener and the connection LED turns green.")
	d.P("If the C-ECHO fails, the status bar shows a connection error and the application remains disconnected.")
	d.P("If the SCP port is already in use — most often because a previous copy of dicomqr was force-closed — a dialog reports \"port N is already in use\". Close the other instance and click Connect again.")

	d.H2("4.4  Testing Connectivity")
	d.P("Click Test (C-ECHO) at any time while connected to send a C-ECHO to the PACS. The status bar reports success or failure.")

	d.H2("4.5  Disconnecting")
	d.P("Click Disconnect (or select File > Disconnect) to close the session. Any in-progress query is cancelled and the local SCP listener is stopped.")

	// 5. Searching
	d.H1("5  Searching for Studies")

	d.H2("5.1  Opening the Filters Panel")
	d.P("Click Filters ▾ in the server row to open the search criteria panel. The panel floats over the results tree and contains the search fields along with Search, Clear, and Close buttons. Click Filters ▾ again, or click Close inside the panel, to dismiss it. Values typed in the fields are preserved between open and close cycles.")

	d.H2("5.2  Search Fields")
	d.Table([]Row{
		{"Field", "Description"},
		{"Patient Name", "Matches the DICOM Patient Name attribute. Supports DICOM wildcard characters: `*` matches any sequence of characters, `?` matches a single character. Format: FAMILY^GIVEN or a partial name with wildcards (e.g. DOE*). A trailing `*` is appended automatically if the value does not already end with one. Leave blank to match all patients."},
		{"Patient ID", "Matches the DICOM Patient ID (MRN). Supports wildcards. A trailing `*` is appended automatically. Leave blank to match all IDs."},
		{"Accession No", "Matches the DICOM Accession Number. Supports wildcards. A trailing `*` is appended automatically. Leave blank to match all accession numbers."},
		{"Study Date From", "The start of the study date range. Click the calendar icon to open a month-view date picker and select a date, or type directly into the field. Leave blank for no lower bound."},
		{"Study Date To", "The end of the study date range. Click the calendar icon to open a month-view date picker and select a date, or type directly into the field. Leave blank for no upper bound."},
		{"Modality", "Restricts results to one or more imaging modalities. Tick any combination: CT, MR, PT, NM, US, CR, DX, XA, RF. When multiple modalities are ticked, a separate query is sent for each and the results are merged. Leave all checkboxes unticked to include all modalities."},
	})
	d.P("At least one field should be populated before searching. Sending a completely unconstrained query (all fields blank, no modalities ticked) may return a very large result set or be rejected by the PACS.")

	d.H2("5.3  Running a Search")
	d.P("With the Filters panel open, click Search inside the panel, or click the Search button in the server row, or press Ctrl+Enter. The panel closes, the results tree clears, and the query is sent to the PACS. The status bar shows \"Querying…\" during the search and reports the number of studies returned when complete.")
	d.P("Pressing Enter while the cursor is in the Patient Name, Patient ID, or Accession No field also runs the search and closes the panel.")

	d.H2("5.4  Clearing the Search")
	d.P("Click Clear inside the Filters panel to reset all search fields to their defaults and clear the results tree. Alternatively, select Query > Clear results.")

	// 6. Results
	d.H1("6  Query Results")

	d.H2("6.1  Tree Structure")
	d.P("Results are displayed in an expandable tree with three levels:")
	d.P("Patient — one node per unique patient. The label shows the patient name and, where present, the patient ID in parentheses.")
	d.P("Study — one or more studies under each patient. The label shows the study date, study description, accession number, and the set of modalities present in the study.")
	d.P("Series — one or more series under each study. The label shows the series number, modality, series description, and image count.")
	d.P("Results are sorted automatically: patients alphabetically by name, studies within a patient chronologically by date (oldest first), and series within a study numerically by series number.")
	d.P("The tree starts fully collapsed after each search. Click the expand arrow next to a patient node to reveal its studies. Click the expand arrow next to a study node to load its series — dicomqr sends a separate C-FIND query to the PACS at this point to retrieve series-level information. The series list is fetched once per study per session; collapsing and re-expanding a study does not repeat the query.")
	d.P("The Expand All and Collapse All buttons above the tree open or close every branch at once. Expanding all branches also triggers the series C-FIND for any study that has not yet loaded its series.")
	d.P("Large result sets are inserted into the tree in batches so the window stays responsive; the status bar shows a `Loading results… N/total` count while the batch is being added.")

	d.H2("6.2  Filtering Results")
	d.P("Type any text into the filter bar above the results tree. The tree immediately redraws to show only rows whose label contains the typed text (case-insensitive). Parent nodes that contain a matching descendant are always shown. Click Clear at the right of the filter bar to remove the filter and restore the full tree.")
	d.P("The filter acts on the already-loaded results and does not send a new query to the PACS.")

	d.H2("6.3  Selecting Items for Retrieval")
	d.P("Click any row in the results tree to select it. Selected rows are highlighted using the colour and font style configured in Preferences > UI (by default, bold in the theme's primary accent colour — see Section 11.1). Click the same row again to deselect it. Multiple rows at any level (patient, study, or series) may be selected simultaneously.")
	d.P("The Select All button (in the retrieve panel) selects every currently visible row and its loaded descendants; Clear Selection clears the entire selection. Pressing Esc also clears the current selection.")
	d.P("Series nodes are only visible after a study has been expanded. Expand a study first, then select individual series for retrieval.")
	d.P("Selection behaviour during retrieval:")
	d.Bullet("Patient node selected — all studies under that patient are retrieved.")
	d.Bullet("Study node selected — the entire study is retrieved as a single C-MOVE request.")
	d.Bullet("Series node(s) selected — each selected series is retrieved individually.")
	d.Bullet("Mixed selection — if a study and one or more of its series are both selected, the study-level retrieve takes precedence and the series are not sent as duplicate requests.")
	d.P("Press Ctrl+C to copy the full label text of any selected row to the clipboard.")

	d.H2("6.4  Right-Click Context Menu")
	d.P("Right-clicking any row in the results tree opens a context menu:")
	d.Table([]Row{
		{"Option", "Action"},
		{"Retrieve", "Retrieves the right-clicked node directly, regardless of the current selection."},
		{"Copy UID", "Copies the Study Instance UID or Series Instance UID of the row to the clipboard."},
		{"Copy label", "Copies the full display label of the row to the clipboard."},
	})

	d.H2("6.5  Tooltips")
	d.P("Hovering the mouse cursor over a study or series row for approximately 0.6 seconds displays a tooltip showing the Study Instance UID and Accession Number (for study rows) or the Series Instance UID and Modality (for series rows). Moving the cursor off the row dismisses the tooltip immediately.")

	// 7. Retrieving
	d.H1("7  Retrieving Files")

	d.H2("7.1  Prerequisites")
	d.P("The conditions required depend on the Retrieve method configured for the server profile (see Section 4.1).")
	d.P("For C-MOVE (default):")
	d.Bullet("The application must be connected to a PACS server (status bar shows \"Connected\").")
	d.Bullet("The embedded C-STORE listener must be running. It starts automatically when a connection is established.")
	d.Bullet("The PACS must have the local AE Title, IP address, and port registered as a known destination. See Section 2.2.")
	d.Bullet("The download folder must be configured. Click Browse… next to the Download to field in the retrieve panel.")
	d.Bullet("At least one item must be selected in the results tree.")
	d.P("For C-GET:")
	d.Bullet("The application must be connected to a PACS server.")
	d.Bullet("The download folder must be configured.")
	d.Bullet("At least one item must be selected.")
	d.P("No inbound SCP port or PACS-side destination AE registration is required for C-GET. The PACS returns files over the existing outbound connection.")
	d.P("For Auto: dicomqr attempts C-GET first. If the PACS rejects C-GET, it retries each item using C-MOVE. The C-MOVE prerequisites above apply as a fallback.")
	d.P("Regardless of method, dicomqr verifies that the download folder exists and is writable before a retrieve begins. If it is not, an error dialog is shown and no retrieve is started.")

	d.H2("7.2  Starting a Retrieve")
	d.P("Select one or more rows in the results tree, then click Retrieve Selected (or select Query > Retrieve Selected). dicomqr issues a retrieve request for each selected item using the method configured in the server profile:")
	d.Bullet("C-MOVE — dicomqr sends a C-MOVE request; the PACS pushes files to the local C-STORE SCP listener, which writes them to the download folder.")
	d.Bullet("C-GET — dicomqr sends a C-GET request; the PACS streams files back over the same association directly.")
	d.Bullet("Auto — dicomqr attempts C-GET; if the PACS rejects it, the request is retried using C-MOVE.")
	d.P("Selecting a study retrieves all of its series in one request; selecting individual series retrieves each independently. A progress bar appears and advances as each study or series is transferred.")

	d.H2("7.3  Progress")
	d.P("The progress bar tracks completion across all selected studies and series, advancing as each study or series finishes. For C-MOVE the bar also advances within a study as individual files arrive; for C-GET it advances once per completed target. As each file arrives, the status bar briefly shows the path of the received file.")

	d.H2("7.4  Completion")
	d.P("When all files have been received successfully, the progress bar disappears and the status bar shows:")
	d.Code("Retrieved N files successfully")
	d.P("If one or more targets encountered a recoverable DICOM error (for example, a warning status from the PACS indicating that some sub-operations failed), the status bar shows the number of files received alongside the number of targets that had problems:")
	d.Code("Retrieved N files (X/Y targets had errors — see log)")
	d.P("In this case a dialog also appears offering to retry only the failed targets. Accepting re-runs the retrieve loop for just those items, leaving already-retrieved files in place. Details of the errors are written to `dicom.log` in `%USERPROFILE%\\.dicomqr\\`.")

	d.H2("7.5  Cancelling a Retrieve")
	d.P("Click Cancel in the retrieve panel (or select Query > Cancel retrieve) to abort an in-progress retrieval. Files that have already been written to disk are not removed. The status bar shows:")
	d.Code("Retrieve cancelled")

	// 8. Local Browse Tab
	d.H1("8  Local Browse Tab")
	d.P("The Local Browse tab lets you work with DICOM files already in the download folder — browse the tree, preview images, push to a remote PACS, or delete files — without running a query.")

	d.H2("8.1  Scanning the Download Folder")
	d.P("Click Scan (or the folder icon to open the folder first). dicomqr walks the download directory, parses each `.dcm` file (skipping pixel data for speed), and builds a Patient > Study > Series tree. The status label shows progress and a file count. The folder button opens the download directory in Windows Explorer.")

	d.H2("8.2  Filtering and Navigation")
	d.P("Type in the filter bar to narrow the tree. Expand All, Collapse All, and Clear buttons are provided. The filter acts on the already-loaded tree and does not rescan the disk.")

	d.H2("8.3  Previewing Images")
	d.P("Right-click any node and select Preview Images:")
	d.Bullet("Series node — opens the series viewer (see Section 8.3.1).")
	d.Bullet("Study node — opens the study overview grid (see Section 8.3.4).")
	d.Bullet("Patient node — Preview Images is disabled (too many files to be useful at this level).")

	d.H3("8.3.1  Series Viewer")
	d.P("The series viewer displays one image at a time and opens at the middle slice. It supports interactive window/level, zoom and pan, and slice navigation by mouse or keyboard.")
	d.P("The bottom bar contains an instance counter (e.g. `45 / 120`), a navigation slider, a Window preset dropdown (see Section 8.3.2), a Colour map dropdown (see Section 8.3.3), an Annotations checkbox (see Section 8.3.5), a Reset button, and an info label showing pixel dimensions and the current W/L values.")
	d.P("Mouse controls:")
	d.Table([]Row{
		{"Action", "Effect"},
		{"Left-drag", "Adjust window/level — horizontal changes the window width, vertical changes the level (centre). The adjustment is anchored to the point where the drag began."},
		{"Right-drag", "Zoom — drag up to magnify, down to zoom out (up to 16×)."},
		{"Middle-drag", "Pan the image when zoomed in."},
		{"Mouse wheel", "Step to the previous / next slice in the series."},
		{"Double-click", "Reset zoom and pan to fit the window."},
	})
	d.P("Keyboard controls (while the viewer window is focused):")
	d.Table([]Row{
		{"Key", "Effect"},
		{"Up / Left / Page Up", "Previous slice."},
		{"Down / Right / Page Down", "Next slice."},
		{"`+` / `-`", "Zoom in / out."},
		{"Home or F", "Reset zoom and pan to fit."},
		{"R", "Reset the window to the default (clears any preset or manual adjustment)."},
	})
	d.P("The Reset button resets both the view (zoom/pan) and the window to the default. Window/level changes made by dragging or by selecting a preset persist as you scroll through the series.")
	d.P("Compressed pixel data — the built-in viewer decodes JPEG Baseline and uncompressed (native) pixel data. Files stored in JPEG 2000, JPEG-LS, JPEG Lossless, or RLE Lossless formats cannot be decoded and display a message explaining the limitation with a suggestion to use Open in Viewer. To avoid this, enable 'Request uncompressed transfer syntax only' in the server profile before retrieving (see Section 4.1).")

	d.H3("8.3.2  Window/Level Presets")
	d.P("The Window dropdown in the viewer bottom bar offers preset windows tailored to the image's modality. Selecting a preset applies it to the current slice and to subsequent slices until you adjust the window manually. Default restores the image's own window (from the DICOM Window tags, or an automatic 1st–99th percentile window when absent); Full range maps the entire pixel value range.")
	d.Table([]Row{
		{"Modality", "Presets offered"},
		{"CT", "Default, Full range, plus Hounsfield windows: Brain, Subdural, Soft tissue, Liver, Mediastinum, Bone, Lung."},
		{"PET (PT)", "Default, Full range, plus 0 → 75% / 50% / 40% / 30% / 20% windows expressed as a fraction of the peak value (a lower percentage raises contrast in low-uptake regions)."},
		{"MR", "Default, Full range, plus Lower / Higher / Highest contrast windows scaled relative to the image's own window (MR intensities have no absolute scale)."},
		{"Other", "Default, Full range, Lower contrast, Higher contrast."},
	})

	d.H3("8.3.3  Colour Maps")
	d.P("The Colour dropdown in the viewer bottom bar applies a colour lookup table to the windowed image — useful for nuclear-medicine studies (PET and SPECT/NM), which are conventionally read in pseudo-colour rather than grayscale. The colour map is applied on top of the current window/level and persists as you scroll through the series until you change it.")
	d.P("For PET (PT) and nuclear-medicine (NM) studies the viewer selects Hot Iron automatically; all other modalities default to Grayscale. The same default colour map is applied to the study overview thumbnails (Section 8.3.4) so the overview matches the viewer.")
	d.Table([]Row{
		{"Colour map", "Description"},
		{"Grayscale", "Standard grayscale (default for CT, MR, and most modalities)."},
		{"Inverse Grayscale", "Grayscale with the intensity ramp inverted."},
		{"Hot Iron", "Black → red → yellow → white. Default for PET/NM."},
		{"PET", "Black → blue → purple → red → orange → yellow → white."},
		{"Hot Metal Blue", "Like PET but with blue rising earlier (cool shadows, hot highlights)."},
		{"PET 20 Step", "The PET palette quantised into 20 discrete colour bands."},
	})
	d.P("Colour maps apply only to grayscale (monochrome) images; for images already stored in colour the dropdown is disabled. The maps are faithful renditions of the DICOM standard palettes intended for display and triage.")

	d.H3("8.3.4  Study Overview Grid")
	d.P("The overview window shows one thumbnail per series — the middle slice of each series rendered in parallel. Thumbnails are arranged in a three-column grid. Double-click any thumbnail to open that series in the full series viewer.")

	d.H3("8.3.5  DICOM Annotation Overlay")
	d.P("When Annotations is checked in the series viewer, a four-corner overlay is drawn within the actual image area (never in the letterbox bars):")
	d.Table([]Row{
		{"Corner", "Content"},
		{"Top-left", "Patient name, MRN, date of birth, sex and age"},
		{"Top-right (right-aligned)", "Institution, study date/time, accession number, study description, referring physician"},
		{"Bottom-left", "Modality, series number and description, slice thickness, protocol"},
		{"Bottom-right (right-aligned)", "Instance number / total, slice location, pixel spacing, W/L values"},
	})
	d.P("Anatomical orientation markers (R/L, A/P, H/F) are centred on the four image edges and derived from the ImageOrientationPatient direction cosines in DICOM LPS patient coordinates.")
	d.P("The Annotations checkbox state persists between sessions.")

	d.H2("8.4  Opening in External Viewer")
	d.P("The Open in Viewer button in the bottom bar and the right-click menu item open the node's folder in the configured external DICOM viewer. These controls are disabled when no viewer path is configured in Preferences. Open folder opens the folder in Windows Explorer instead.")

	d.H2("8.5  Pushing to a PACS")
	d.P("Right-click any node and select Push to PACS…, or select items and click Push Selected…, to send files to a remote PACS via C-STORE SCU.")
	d.P("A dialog appears with a destination selector (any configured server profile), a progress bar and per-file counter, and a Cancel button. The push creates a new association per operation and does not require the PACS tab to be connected.")

	d.H2("8.6  Deleting Local Files")
	d.P("Right-click any node and select Delete…, or select items and click Delete Selected…, to permanently remove files from disk. A confirmation dialog shows the file count and total size. After deletion, empty directories are pruned and the tree is rescanned automatically.")
	d.P("Warning: Deletion is permanent. Files are not moved to the Recycle Bin.")

	d.H2("8.7  Selection Controls")
	d.Table([]Row{
		{"Control", "Action"},
		{"Select All", "Selects every currently visible (filtered) root node and all its descendants."},
		{"Clear Selection", "Deselects everything."},
		{"Push Selected…", "Pushes all selected files to a chosen server."},
		{"Delete Selected…", "Deletes all selected files after confirmation."},
	})

	// 9. Import Tab
	d.H1("9  Import Tab")
	d.P("The Import tab copies DICOM files from any folder into the organised download folder, applying the same Patient / Study / Series subfolder structure used by retrieval.")

	d.H2("9.1  Scanning a Source Folder")
	d.P("Enter or browse to a source folder and click Scan. dicomqr walks the folder and builds a tree of studies and series found in it. The destination folder (the configured download folder) is shown read-only below the source field.")

	d.H2("9.2  Selecting and Importing")
	d.P("Click rows in the tree to select them. Click Import Selected to copy the selected files. Files already present in the destination (same SOP Instance UID at the same destination path) are skipped; the status label reports imported, already-present, and failed counts.")
	d.P("Select All and Clear Selection buttons are provided. The filter bar narrows the tree in the same way as the other tabs.")

	// 10. Worklist Tab
	d.H1("10  Worklist Tab")
	d.P("The Worklist tab queries a Modality Worklist server for scheduled imaging procedures. It operates independently of the PACS Query tab — it does not require a PACS connection and can target a different server (typically a RIS or MWL broker).")

	d.H2("10.1  Selecting a Worklist Server")
	d.P("Choose any configured server profile from the Worklist server dropdown. The dropdown updates when server profiles are added or removed in Preferences. The query connects to the selected profile's host, port, and AE Title for each query and releases the association immediately after.")
	d.P("Note: the Modality Worklist SOP class (1.2.840.10008.5.1.4.31) must be enabled on the target server. In most environments this is a separate system from the PACS — configure a server profile pointing to that system.")

	d.H2("10.2  Query Fields")
	d.Table([]Row{
		{"Field", "Description"},
		{"Patient Name", "Wildcard-capable patient name match. A trailing `*` is appended automatically. Leave blank to match all patients."},
		{"MRN", "Wildcard-capable Patient ID match."},
		{"Accession", "Wildcard-capable Accession Number match."},
		{"Modality", "Restricts results to one modality. Select (any) to include all modalities."},
		{"Scheduled date", "Today only (checked by default) — restricts to today's scheduled date. Uncheck to select a specific date using the calendar picker. Leave blank (unchecked, no date selected) to return all scheduled dates."},
	})
	d.P("Click Query Worklist or press Enter in any text field to run the query. Click Clear to reset all fields and clear the results.")

	d.H2("10.3  Results Table")
	d.P("Results are shown in a table with columns: Patient, MRN, Accession, Date, Time, Modality, Procedure, and Station. Click any row to select it.")
	d.P("Copy Accession and Copy Patient buttons copy the selected row's values to the clipboard. The status label shows the number of worklist items returned, or any error message.")

	d.H2("10.4  Typical Use Cases")
	d.Bullet("Verify a scheduled procedure — query by patient name or accession to confirm an order reached the worklist server before the patient arrives at the scanner.")
	d.Bullet("Diagnose \"patient not on scanner\" — if a technologist cannot find a patient on the modality's worklist, query here; if the entry appears, the problem is in the scanner's MWL configuration; if it does not, the order was not transmitted to the worklist server.")
	d.Bullet("Look up an accession number — copy the accession and switch to PACS Query to search for the matching study.")

	// 11. Downloaded Files
	d.H1("11  Downloaded Files")
	d.P("Files are written to the folder specified in the Download to field. Within that folder, dicomqr creates a three-level subfolder structure:")
	d.Code("<Download folder>\\\n    <Patient Name> (<Patient ID>)\\\n        <Study Description> (<Study Date>)\\\n            <Series Description> (<Series Number>)\\\n                <SOP Instance UID>.dcm")
	d.P("For example:")
	d.Code("Downloads\\\n    Doe^John (MRN12345)\\\n        Chest CT (20240115)\\\n            Chest W Contrast (2)\\\n                1.2.840.10008.5.1.4.1.1.2.dcm")
	d.P("If a metadata field is absent from the DICOM file, the corresponding folder component falls back to a descriptive placeholder: Unknown Patient, Unknown Study, or Unknown Series. Characters that are not permitted in Windows file or folder names are replaced with underscores.")
	d.P("Each SOP Instance UID is unique, so files from different studies that share the same patient ID and series number are written to separate subfolders and are never overwritten.")

	// 12. Menus
	d.H1("12  Menus")

	d.H2("12.1  File Menu")
	d.Table([]Row{
		{"Item", "Description"},
		{"Connect", "Connects to the currently selected server profile."},
		{"Disconnect", "Ends the current session and stops the local SCP listener."},
		{"Preferences…", "Opens the Preferences dialog. See Section 14."},
		{"Quit", "Exits the application."},
	})

	d.H2("12.2  Query Menu")
	d.Table([]Row{
		{"Item", "Description"},
		{"Search", "Runs the current search."},
		{"Clear results", "Resets all search fields and removes all results from the tree."},
		{"Export…", "Saves the current results tree to CSV or JSON."},
		{"Retrieve Selected", "Starts retrieval of all currently selected tree nodes."},
		{"Cancel retrieve", "Cancels an in-progress retrieval."},
	})

	d.H2("12.3  Help Menu")
	d.Table([]Row{
		{"Item", "Description"},
		{"Activity Log…", "Opens the in-app activity log showing the last 500 lines of the DICOM protocol log. Buttons: Refresh (manual update), Copy All (clipboard), Clear. The log auto-refreshes once per second while the dialog is open."},
		{"About", "Displays the application version, build date, and library credits."},
		{"Client info…", "Displays the local AE Title, SCP port, and detected IP address."},
	})

	// 13. Keyboard Shortcuts
	d.H1("13  Keyboard Shortcuts")
	d.Table([]Row{
		{"Shortcut", "Action"},
		{"Ctrl+Enter", "Run the current search."},
		{"Ctrl+F", "Move focus to the Patient Name field in the Filters panel."},
		{"Ctrl+R", "Retrieve the currently selected items."},
		{"Ctrl+C", "Copy the full label of the currently selected result row to the clipboard."},
		{"Esc", "Clear the current selection in the results tree."},
	})

	// 14. Preferences
	d.H1("14  Preferences")
	d.P("Open the Preferences dialog from File > Preferences…. Changes take effect when Apply is clicked and are written immediately to disk.")

	d.H2("14.1  UI Section")
	d.Table([]Row{
		{"Setting", "Description"},
		{"Theme", "Selects the application colour theme: Light or Dark."},
		{"Tree font", "Selects the font used for results tree rows. Select (default) to use the application's built-in font."},
		{"Selection colour", "The colour applied to selected rows. Click Choose colour… to open a colour picker. If unset, selected rows follow the theme's primary accent colour."},
		{"Selection style", "The font style applied to selected rows: Bold and/or Italic."},
	})

	d.H2("14.2  Connections Section")
	d.P("Lists all saved server profiles. Click Edit to modify, Delete to remove, or Add server… to create a new profile. The Up/Down buttons reorder the list; the first profile is the default selection when the application starts.")
	d.P("Profile editor fields:")
	d.Table([]Row{
		{"Field", "Description"},
		{"Profile name", "A descriptive label shown in the server dropdown and the Worklist tab."},
		{"Remote AE Title", "The AE Title of the PACS or MWL server (case-sensitive, uppercase recommended)."},
		{"Host", "The hostname or IP address of the server."},
		{"Port", "The TCP port of the DICOM service (commonly 104 or 11112)."},
		{"Info model", "`study` — Study Root (default, most common). `patient` — Patient Root. `patient-study-only` — legacy retired model; SERIES-level lazy-load is suppressed automatically."},
		{"Retrieve method", "C-MOVE / C-GET / Auto — see Section 4.1."},
		{"Connect timeout", "Seconds before a connection attempt is considered failed."},
		{"Transfer", "When ticked, negotiates only uncompressed transfer syntaxes — see Section 4.1 for full details and caveats."},
	})

	d.H2("14.3  Retrieve Section")
	d.Table([]Row{
		{"Setting", "Description"},
		{"Local AE Title", "The AE Title this workstation presents during DICOM associations. Default: DICOMQR."},
		{"Local SCP port", "The TCP port on which the embedded C-STORE listener accepts incoming connections. Default: 11112."},
		{"Download folder", "The root folder where retrieved and imported DICOM files are written."},
	})
	d.P("Changes to AE Title or SCP port take effect the next time a connection is established.")

	d.H2("14.4  Image Viewer Section")
	d.Table([]Row{
		{"Setting", "Description"},
		{"External viewer", "Full path to an external DICOM viewer executable. Click Browse… to locate it, or Auto-detect to search for MicroDicom or RadiAnt DICOM Viewer in the standard installation locations. When left empty, the Open in Viewer buttons and menu items are disabled."},
	})

	// 15. Status Bar
	d.H1("15  Status Bar")
	d.P("The status bar at the bottom of the window provides real-time feedback. A coloured LED indicator (gray / amber / green) precedes the status text.")
	d.Table([]Row{
		{"Situation", "Status bar text"},
		{"Application started, not connected", "`v1.4.0`"},
		{"Connecting to server", "`Connecting…`"},
		{"Connected", "`Connected: <AE>@<host>:<port>`"},
		{"Connection cancelled", "`Connection cancelled`"},
		{"Connection failed", "`Connection failed: <reason>`"},
		{"Disconnected", "`Disconnected`"},
		{"Query in progress", "`Querying…`"},
		{"Loading results into the tree", "`Loading results… <N>/<total>`"},
		{"Query complete", "`Query complete — <N> studies`"},
		{"Query error", "`Query error: <reason>`"},
		{"Retrieve starting", "`Starting retrieve of <N> studies…`"},
		{"Retrieve in progress", "`Retrieving study <N>/<total>…`"},
		{"File received", "`Received: <file path>`"},
		{"Retrieve complete", "`Retrieved <N> files successfully`"},
		{"Retrieve complete with warnings", "`Retrieved <N> files (<X>/<total> targets had errors — see log)`"},
		{"Retrieve cancelled", "`Retrieve cancelled`"},
		{"C-ECHO test passed", "`C-ECHO success`"},
		{"C-ECHO test failed", "`C-ECHO failed: <reason>`"},
	})
	d.P("The SCP status indicator row in the connection panel shows:")
	d.Table([]Row{
		{"Situation", "SCP indicator text"},
		{"Not connected", "SCP: not running"},
		{"SCP listening", "SCP: listening on 0.0.0.0:<port> (AE: <title>)"},
		{"SCP failed to start", "SCP: error — <reason>"},
	})

	// Appendix A
	d.PageBreak()
	d.H1("Appendix A  Application Settings")
	d.P("Application settings are persisted to `%USERPROFILE%\\.dicomqr\\settings.json`. This file is created automatically on first launch with the compiled-in defaults shown below.")
	d.Table3([]Row3{
		{"JSON key", "Default", "Description"},
		{"`darkTheme`", "`false`", "Colour theme. false = Light, true = Dark."},
		{"`fontName`", "`\"\"`", "System font for result tree rows. Empty = built-in font."},
		{"`localAETitle`", "`\"DICOMQR\"`", "The AE Title presented during DICOM associations."},
		{"`localSCPPort`", "`11112`", "TCP port for the embedded C-STORE listener."},
		{"`downloadDir`", "`\"\"`", "Absolute path of the download folder. Defaults to ~/DICOM Downloads."},
		{"`viewerPath`", "`\"\"`", "Full path to an external DICOM viewer executable. Empty disables the Open in Viewer controls."},
		{"`selectionColor`", "`\"\"`", "Colour applied to selected tree rows (RRGGBBAA hex). Empty follows the theme primary colour."},
		{"`selectionBold`", "`true`", "Whether selected rows are drawn in bold."},
		{"`selectionItalic`", "`false`", "Whether selected rows are drawn in italic."},
		{"`windowWidth`", "`0`", "Saved window width in pixels. 0 uses the default; updated automatically on close."},
		{"`windowHeight`", "`0`", "Saved window height in pixels."},
		{"`profiles`", "`[]`", "Array of saved server profile objects (see below)."},
	})
	d.P("Each entry in the `profiles` array:")
	d.Table([]Row{
		{"JSON key", "Description"},
		{"`name`", "Display name of the profile."},
		{"`remoteAETitle`", "AE Title of the PACS or MWL server."},
		{"`host`", "Hostname or IP address."},
		{"`port`", "TCP port."},
		{"`infoModel`", "`\"study\"`, `\"patient\"`, or `\"patient-study-only\"`."},
		{"`retrieveMethod`", "`\"MOVE\"`, `\"GET\"`, or `\"AUTO\"`. Omitting defaults to C-MOVE."},
		{"`connectTimeout`", "Connection timeout in seconds. 0 uses the default (10 s)."},
		{"`transferUncompressed`", "When true, the A-ASSOCIATE negotiation for C-GET and C-MOVE offers only uncompressed transfer syntaxes. Default: false."},
	})
	d.P("The Annotations overlay toggle is stored in the application's Fyne preferences (not in settings.json) and persists automatically between sessions.")

	// Appendix B
	d.PageBreak()
	d.H1("Appendix B  PACS Configuration Notes")
	d.P("AE Title registration — The PACS must have a record of the local AE Title (default DICOMQR) associated with the workstation's IP address and SCP port. Look for \"Known Destinations\", \"Remote AE Configuration\", or similar.")
	d.P("C-MOVE destination — For file delivery the PACS must be configured to push files to the local SCP address. The workstation must be reachable at the IP and port shown in Help > Client info…")
	d.P("Windows Firewall — An inbound rule permitting TCP connections on the SCP port (default 11112) is required.")
	d.P("Information model — If queries return no results, try changing the Info model in the server profile. Some PACS require Study Root, others Patient Root. A small number of legacy systems require the Patient/Study Only model (patient-study-only).")
	d.P("Worklist server — The Modality Worklist SOP class is typically served by a RIS or dedicated MWL broker, not the PACS itself. Create a separate server profile pointing to that system and select it in the Worklist tab.")
	d.P("Compressed pixel data — if the PACS stores images in JPEG 2000, JPEG-LS, or other compressed formats that the built-in viewer cannot decode, enable 'Request uncompressed transfer syntax only' in the server profile. The PACS will transcode on the fly if it supports transcoding. If the PACS does not support transcoding, the transfer will fail for those SOP classes; use the external viewer integration instead.")
	d.P("IPv4 connectivity — dicomqr listens on an IPv4 socket only. Ensure the address shown in Help > Client info… is the correct IPv4 address on the same network as the PACS.")

	// Appendix C
	d.PageBreak()
	d.H1("Appendix C  Credits and Acknowledgements")

	d.H2("Developer")
	d.P("Jeffrey Leal")
	d.P("Email: jeffrey.leal@gmail.com")
	d.P("GitHub: https://github.com/jeffrey-leal")

	d.H2("AI Assistance")
	d.P("This application was designed and developed with the assistance of Claude Sonnet 4.6 by Anthropic, accessed through Claude Code (https://claude.ai/code). Architecture planning, code generation, DICOM standard research, and documentation were produced in collaboration with Claude Code.")

	d.H2("DICOM Standard Reference")
	d.P("Protocol implementation follows the DICOM Standard published by NEMA:")
	d.P("DICOM PS3 (2024b) — https://dicom.nema.org/medical/dicom/current")
	d.P("Sections referenced:")
	d.Bullet("PS3.4 — Service Class Specifications (Query/Retrieve C.4; Modality Worklist K.4; Storage B.5)")
	d.Bullet("PS3.7 — Message Exchange (DIMSE-C services: C-ECHO, C-FIND, C-MOVE, C-GET, C-STORE)")
	d.Bullet("PS3.8 — Network Communication / DICOM Upper Layer Protocol")

	d.H2("Open-Source Libraries")
	d.Table4([]Row4{
		{"Library", "Author / Maintainer", "Licence", "Purpose"},
		{"fyne.io/fyne/v2 v2.7.3", "Fyne.io contributors", "BSD 3-Clause", "GUI framework"},
		{"algm/go-netdicom v0.1.0", "Alan Griffin (fork of grailbio)", "BSD 3-Clause", "DICOM networking (C-ECHO, C-FIND, C-MOVE, C-GET, C-STORE SCP/SCU, Worklist)"},
		{"grailbio/go-netdicom", "Yasushi Saito / GRAIL Inc.", "BSD 3-Clause", "Original DICOM networking library (base of go-netdicom fork)"},
		{"grailbio/go-dicom", "GRAIL Inc.", "Apache 2.0", "DICOM dataset encoding / file header writing"},
		{"suyashkumar/dicom v1.1.0", "Suyash Kumar", "MIT", "DICOM file parsing, image rendering, annotation extraction"},
		{"sqweek/dialog", "sqweek", "ISC", "Native Windows file/folder picker dialogs"},
	})
	d.Space()
	d.P("A vendored copy of `algm/go-netdicom` is included under `thirdparty/go-netdicom` with its original BSD 3-Clause licence intact.")
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	// Generate Markdown.
	md := &MDDoc{}
	buildContent(md)
	mdOut := "dicomqr-user-manual.md"
	if err := os.WriteFile(mdOut, []byte(md.b.String()), 0o644); err != nil {
		panic(err)
	}
	fmt.Println("written:", mdOut)

	// Generate DOCX. Load the cover logo first so its display dimensions are
	// known when buildContent emits the cover image paragraph.
	const logoPath = "DICOM App Icon.png"
	logoBytes, err := os.ReadFile(logoPath)
	if err != nil {
		panic(fmt.Sprintf("reading cover logo %q: %v (required build asset)", logoPath, err))
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(logoBytes))
	if err != nil {
		panic(fmt.Sprintf("decoding cover logo %q: %v", logoPath, err))
	}
	const emuPerInch = 914400
	logoCX := int(2.0 * emuPerInch) // 2 in display width
	logoCY := logoCX * cfg.Height / cfg.Width

	d := &Doc{logoCX: logoCX, logoCY: logoCY, hasLogo: true}
	buildContent(d)

	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document ` +
		`xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" ` +
		`xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" ` +
		`xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" ` +
		`xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture">` +
		`<w:body>` +
		d.b.String() +
		`<w:sectPr>` +
		`<w:headerReference w:type="default" r:id="rId4"/>` +
		`<w:footerReference w:type="default" r:id="rId5"/>` +
		`<w:pgSz w:w="12240" w:h="15840"/>` +
		`<w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440"` +
		` w:header="720" w:footer="720" w:gutter="0"/>` +
		`<w:pgNumType w:start="1"/>` +
		`</w:sectPr>` +
		`</w:body></w:document>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, content string) {
		f, err := zw.Create(name)
		if err != nil {
			panic(err)
		}
		if _, err := fmt.Fprint(f, content); err != nil {
			panic(err)
		}
	}
	addBytes := func(name string, content []byte) {
		f, err := zw.Create(name)
		if err != nil {
			panic(err)
		}
		if _, err := f.Write(content); err != nil {
			panic(err)
		}
	}
	add("[Content_Types].xml", contentTypes)
	add("_rels/.rels", rootRels)
	add("word/_rels/document.xml.rels", wordRels)
	add("word/document.xml", docXML)
	add("word/styles.xml", stylesXML)
	add("word/numbering.xml", numbering)
	add("word/settings.xml", settings)
	add("word/header1.xml", header1)
	add("word/footer1.xml", footer1)
	add("docProps/core.xml", coreProps)
	addBytes("word/media/image1.png", logoBytes)
	if err := zw.Close(); err != nil {
		panic(err)
	}
	docxOut := "dicomqr-user-manual.docx"
	if err := os.WriteFile(docxOut, buf.Bytes(), 0o644); err != nil {
		panic(err)
	}
	fmt.Println("written:", docxOut)
}
