// Package coursecertificate renders a verified completion manifest into one
// self-contained SVG a learner can share.
//
// Why SVG rather than single-file HTML. The artifact has two jobs at once: it is
// opened in a browser, and it is embedded in a README or a showcase issue. SVG is
// the only single file that does both — `![](certificate.svg)` renders where an
// .html attachment is just a download — and its geometry is fixed, so the file a
// reviewer sees is the file the holder saw. HTML would reflow against whatever
// fonts and window width the reader happens to have.
//
// Nothing here is fetched at render time. There is no @font-face, no <image>, no
// <script>, and no stylesheet link: the only absolute URL in the output is the
// SVG namespace declaration, which is an XML identifier the renderer compares as
// a string and never resolves over the network.
package coursecertificate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/courseevidence"
)

// Tier is the capstone tier a holder claims. It is a closed set because a free
// string on the face of a credential is how "Platinum" gets invented; the three
// names are the ones `content/8. Community/8.7. Capstone.md` defines and the ones
// the showcase issue template offers.
type Tier string

const (
	Bronze Tier = "Bronze"
	Silver Tier = "Silver"
	Gold   Tier = "Gold"
)

// ParseTier accepts the tier in whatever case a shell supplied it and returns the
// one spelling that reaches the certificate.
func ParseTier(value string) (Tier, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "bronze":
		return Bronze, nil
	case "silver":
		return Silver, nil
	case "gold":
		return Gold, nil
	default:
		return "", fmt.Errorf("unknown tier %q; the capstone defines bronze, silver, and gold", value)
	}
}

// maxNameRunes bounds the holder name so the layout stays legible. SVG has no
// layout engine to fall back on, so an unbounded name would run off the border
// instead of wrapping; refusing it is better than rendering a broken credential.
const maxNameRunes = 48

// Input is everything the certificate states. Deliberately no clock and no
// filesystem: the date comes from the manifest, so re-running the same command
// against the same manifest reproduces the file byte for byte, which is the
// property the certificate claims about itself on its own face.
type Input struct {
	Name           string
	Tier           Tier
	CourseVersion  string
	ManifestDigest string
	Manifest       courseevidence.Manifest
}

// Digest is the SHA-256 of the manifest bytes, formatted the way `sha256sum`
// prints it so a reader can compare the two strings without transforming either.
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// Render returns the complete SVG document, or the first reason it cannot make an
// honest one.
func Render(input Input) (string, error) {
	name, err := validateName(input.Name)
	if err != nil {
		return "", err
	}
	if input.Tier != Bronze && input.Tier != Silver && input.Tier != Gold {
		return "", fmt.Errorf("tier %q is not one of the capstone tiers", string(input.Tier))
	}
	if strings.TrimSpace(input.CourseVersion) == "" {
		return "", errors.New("the certificate needs the course version from the repository VERSION file")
	}
	if len(input.ManifestDigest) != 2*sha256.Size {
		return "", errors.New("the manifest digest must be the hex SHA-256 of the manifest file")
	}
	generated, err := manifestDate(input.Manifest.GeneratedAt)
	if err != nil {
		return "", err
	}
	if input.Manifest.Revision == "" || input.Manifest.TreeDigest == "" {
		return "", errors.New("the manifest carries no source revision or tree digest to certify")
	}
	if len(input.Manifest.Gates) == 0 || len(input.Manifest.Artifacts) == 0 {
		return "", errors.New("the manifest records no gates or artifacts, so there is nothing to certify")
	}
	page := draw(input, name, generated)
	// A fixed canvas cannot reflow, so a manifest carrying a gate command longer
	// than the column would silently print past the border. Refusing to render is
	// the honest outcome: a credential with a truncated command on it is worse
	// than no credential.
	if page.right > canvasWidth-marginX {
		return "", fmt.Errorf(
			"a manifest row needs %.0f points and the certificate column is %.0f; shorten the gate commands or widen the layout",
			page.right-marginX, contentWidth,
		)
	}
	return document(input, name, page.body.String(), page.y+marginBottom), nil
}

func validateName(value string) (string, error) {
	name := strings.TrimSpace(value)
	if name == "" {
		return "", errors.New("the certificate needs a name to print; pass --name")
	}
	// Rejected rather than stripped. A tab or a newline in a name is either a
	// paste accident or an attempt to smuggle markup past the escaper, and both
	// deserve to stop the command instead of quietly changing what is printed.
	for _, character := range name {
		if !unicode.IsPrint(character) {
			return "", errors.New("the name contains a control character; pass printable text")
		}
	}
	if count := utf8.RuneCountInString(name); count > maxNameRunes {
		return "", fmt.Errorf("the name is %d characters; the certificate has room for %d", count, maxNameRunes)
	}
	return name, nil
}

// manifestDate keeps the certificate's date and the manifest's timestamp the same
// fact. An unparsable timestamp is an error rather than a fallback to today,
// because a certificate that quietly dates itself to the render is exactly the
// kind of unverifiable claim this artifact exists to avoid.
func manifestDate(value string) (string, error) {
	generated, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "", fmt.Errorf("the manifest timestamp %q is not RFC 3339: %w", value, err)
	}
	return generated.UTC().Format("2006-01-02"), nil
}

// Canvas geometry. One column, because a two-column layout would need measured
// text to avoid collisions and measuring is exactly what a font-free renderer
// cannot do. The height is computed from the content instead of pinned here: the
// gate list comes from the manifest, so adding a gate has to lengthen the page
// rather than push a line off the bottom of it.
const (
	canvasWidth  = 1000.0
	marginX      = 56.0
	marginBottom = 30.0
	contentWidth = canvasWidth - 2*marginX
)

// monoAdvance is the advance width of one monospace glyph as a fraction of the
// font size. Every monospace family a browser resolves sits within a few percent
// of 0.6em, and the hex rows below pin their width with `textLength` anyway, so
// this only has to be close enough to choose a sensible size.
const monoAdvance = 0.6

// proseColumns is the wrap budget for the fine print, counted in characters
// rather than measured pixels. At 13px in any of the sans-serif families a
// browser resolves, the widest realistic line of 96 characters stays inside the
// content column with room to spare.
const proseColumns = 96

type canvas struct {
	body strings.Builder
	y    float64
	// right is the widest right edge any pinned run reached. Only monospace runs
	// contribute, because they are the only ones whose width this renderer knows;
	// the proportional runs are bounded by the wrap budget and the name scaling.
	right float64
}

func (c *canvas) advance(delta float64) { c.y += delta }

// line emits one left-aligned run of text at the current baseline.
func (c *canvas) line(class, value string) {
	fmt.Fprintf(&c.body, "  <text class=%q x=\"%.0f\" y=\"%.0f\">%s</text>\n", class, marginX, c.y, escape(value))
}

// fixed emits a monospace run whose rendered width is pinned with `textLength`,
// so a revision or a digest occupies the same box in every renderer regardless of
// which monospace family the reader's machine actually has.
func (c *canvas) fixed(class, value string, fontSize float64) {
	c.fixedAt(class, value, marginX, fontSize)
}

// fixedAt is the same, at an explicit x. Two-column rows are built from two runs
// rather than from one run padded with spaces: SVG collapses a run of whitespace
// to a single space by default, so padding survives the file and then disappears
// in the renderer — and with `textLength` still set for the padded length, the
// surviving glyphs are stretched to fill the gap.
func (c *canvas) fixedAt(class, value string, x, fontSize float64) {
	width := monoWidth(value, fontSize)
	c.right = max(c.right, x+width)
	fmt.Fprintf(&c.body,
		"  <text class=%q x=\"%.1f\" y=\"%.0f\" textLength=\"%.1f\" lengthAdjust=\"spacingAndGlyphs\">%s</text>\n",
		class, x, c.y, width, escape(value))
}

// monoWidth is the width one monospace run occupies at a font size, which is also
// the unit every column offset on this page is expressed in.
func monoWidth(value string, fontSize float64) float64 {
	return float64(utf8.RuneCountInString(value)) * fontSize * monoAdvance
}

func (c *canvas) rule() {
	fmt.Fprintf(&c.body, "  <line class=\"rule\" x1=\"%.0f\" y1=\"%.0f\" x2=\"%.0f\" y2=\"%.0f\"/>\n",
		marginX, c.y, canvasWidth-marginX, c.y)
}

func draw(input Input, name, generated string) *canvas {
	canvas := &canvas{y: 66}
	fmt.Fprintf(&canvas.body, "  <text class=\"eyebrow\" x=\"%.0f\" y=\"%.0f\">%s</text>\n",
		canvasWidth-marginX, canvas.y, escape("SELF-ATTESTED · REPRODUCIBLE"))
	canvas.line("eyebrow-left", "AGENTOPS OPEN COURSE · CAPSTONE")
	canvas.advance(18)
	canvas.rule()

	canvas.advance(64)
	// The name is the one variable-width string that must not be pinned with
	// textLength: stretching "Ada" across the column would look like a defect.
	// It is scaled down instead, against a deliberately pessimistic advance so a
	// wide font still lands inside the border.
	fmt.Fprintf(&canvas.body, "  <text class=\"holder\" x=\"%.0f\" y=\"%.0f\" font-size=\"%.1f\">%s</text>\n",
		marginX, canvas.y, nameFontSize(name), escape(name))
	canvas.advance(34)
	canvas.line("statement", "completed the capstone at the "+string(input.Tier)+" tier.")

	canvas.advance(46)
	canvas.line("label", "WHAT A MACHINE CHECKED, AT THE REVISION BELOW")
	const gateFontSize = 15
	commands := make([]string, 0, len(input.Manifest.Gates))
	for _, gate := range input.Manifest.Gates {
		commands = append(commands, gate.Command)
	}
	results := marginX + columnAfter(gateFontSize, commands)
	for _, gate := range input.Manifest.Gates {
		canvas.advance(24)
		canvas.fixed("mono", gate.Command, gateFontSize)
		canvas.fixedAt("mono", gate.Result, results, gateFontSize)
	}
	canvas.advance(24)
	canvas.fixed("mono", strconv.Itoa(len(input.Manifest.Artifacts))+" evidence artifacts hashed", gateFontSize)

	for _, fact := range []struct{ label, value string }{
		{"COURSE", input.Manifest.Course + " v" + input.CourseVersion},
		{"SOURCE REVISION", input.Manifest.Revision},
		{"TREE DIGEST", input.Manifest.TreeDigest},
		{"MANIFEST DIGEST (SHA-256)", input.ManifestDigest},
		{"MANIFEST GENERATED", generated + " UTC"},
	} {
		canvas.advance(38)
		canvas.line("label", fact.label)
		canvas.advance(21)
		canvas.fixed("mono", fact.value, 14)
	}

	canvas.advance(34)
	canvas.rule()
	canvas.advance(28)
	canvas.line("label", "WHAT NOBODY CHECKED")
	for _, line := range wrap(disclosure(input.Tier), proseColumns) {
		canvas.advance(20)
		canvas.line("fine", line)
	}
	canvas.advance(28)
	// ASCII only on the monospace rows. A missing glyph in whatever monospace
	// family the reader's machine resolves would render as a box, and these two
	// lines are the ones a reader is meant to retype.
	const recipeFontSize = 13
	recipes := [...]struct{ command, effect string }{
		{"sha256sum .agents/tmp/course-completion.json", "-> the manifest digest above"},
		{"mise run course:evidence:verify", "-> re-runs the gates at that revision"},
	}
	effects := marginX + columnAfter(recipeFontSize, []string{recipes[0].command, recipes[1].command})
	for _, recipe := range recipes {
		canvas.fixed("mono-fine", recipe.command, recipeFontSize)
		canvas.fixedAt("mono-fine", recipe.effect, effects, recipeFontSize)
		canvas.advance(20)
	}
	return canvas
}

// columnAfter is the offset where a second monospace column starts, given the
// runs in the first one. The three-character gutter is the smallest gap that
// still reads as two columns at these sizes.
func columnAfter(fontSize float64, values []string) float64 {
	widest := 0
	for _, value := range values {
		widest = max(widest, utf8.RuneCountInString(value))
	}
	return float64(widest+3) * fontSize * monoAdvance
}

// disclosure is the sentence the artifact exists for. It is assembled here rather
// than spelled out per tier so the honest part cannot be edited out of one branch
// and left in another.
func disclosure(tier Tier) string {
	return "Nobody issued, graded, signed, or registered this certificate; the holder rendered it. " +
		"The gates above ran on the holder's own machine, and the " + string(tier) + " tier is the holder's " +
		"own claim against the capstone milestones, which no command measures. What is checkable is the " +
		"pair below: the digest identifies one manifest exactly, and the verify command re-runs those " +
		"gates at that revision and refuses a changed tree. Rendering is deterministic, so the same " +
		"manifest, name, and tier produce this file byte for byte."
}

// nameFontSize shrinks a long name instead of letting it overflow. The 0.62em
// average advance is above what any common bold sans-serif needs for mixed-case
// Latin text, which trades a little unused margin for never clipping a name.
func nameFontSize(name string) float64 {
	const (
		preferred = 42.0
		smallest  = 20.0
		average   = 0.62
	)
	size := contentWidth / (average * float64(utf8.RuneCountInString(name)))
	return max(smallest, min(preferred, size))
}

// wrap breaks prose on word boundaries at a fixed column budget. Character
// counting rather than measurement is the point: the renderer must not depend on
// font metrics it cannot read, and a conservative budget is what makes the same
// file legible on a machine with different fonts installed.
func wrap(text string, columns int) []string {
	words := strings.Fields(text)
	lines := make([]string, 0, len(text)/columns+1)
	var current strings.Builder
	for _, word := range words {
		if current.Len() > 0 && current.Len()+1+len(word) > columns {
			lines = append(lines, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteByte(' ')
		}
		current.WriteString(word)
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}
	return lines
}

func document(input Input, name, body string, height float64) string {
	title := name + " — AgentOps Open Course capstone, " + string(input.Tier) + " tier"
	description := "Self-attested completion certificate for manifest " + input.ManifestDigest +
		" at source revision " + input.Manifest.Revision + "."
	var out strings.Builder
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	// The xmlns value is the SVG namespace identifier. It is compared as a string
	// by every renderer and never dereferenced, so it is not a network dependency
	// — and it cannot be omitted, or a browser treats the file as plain XML.
	fmt.Fprintf(&out,
		"<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"%.0f\" height=\"%.0f\" "+
			"viewBox=\"0 0 %.0f %.0f\" role=\"img\" aria-labelledby=\"title desc\">\n",
		canvasWidth, height, canvasWidth, height)
	fmt.Fprintf(&out, "  <title id=\"title\">%s</title>\n", escape(title))
	fmt.Fprintf(&out, "  <desc id=\"desc\">%s</desc>\n", escape(description))
	out.WriteString(style)
	// An explicit background: an SVG opened straight from disk is painted over
	// whatever the browser's page background is, and dark ink on a dark default
	// is unreadable.
	fmt.Fprintf(&out, "  <rect class=\"page\" width=\"%.0f\" height=\"%.0f\"/>\n", canvasWidth, height)
	fmt.Fprintf(&out, "  <rect class=\"frame\" x=\"14\" y=\"14\" width=\"%.0f\" height=\"%.0f\" rx=\"6\"/>\n",
		canvasWidth-28, height-28)
	out.WriteString(body)
	out.WriteString("</svg>\n")
	return out.String()
}

// Only generic and widely preinstalled families are named, in that order, so the
// document resolves a font from the reader's machine and never asks for one.
const style = `  <style>
    .page { fill: #fdfdfb; }
    .frame { fill: none; stroke: #1d1d1b; stroke-width: 2; }
    text { font-family: ui-sans-serif, system-ui, "Helvetica Neue", Arial, sans-serif; fill: #1d1d1b; }
    .mono, .mono-fine { font-family: ui-monospace, "DejaVu Sans Mono", "Courier New", monospace; }
    .eyebrow, .eyebrow-left { font-size: 13px; letter-spacing: 2px; fill: #55554e; }
    .eyebrow { text-anchor: end; }
    .holder { font-weight: 700; }
    .statement { font-size: 20px; }
    .label { font-size: 11px; letter-spacing: 1.6px; fill: #6b6b62; }
    .mono { font-size: 15px; }
    .mono-fine { font-size: 13px; fill: #3a3a35; }
    .fine { font-size: 13px; fill: #3a3a35; }
    .rule { stroke: #c9c9c0; stroke-width: 1; }
  </style>
`

// escape runs every interpolated value through the XML escaper. The holder name
// is learner input, and it is the only place on this page where text from outside
// the repository reaches markup.
func escape(value string) string {
	var out bytes.Buffer
	if err := xml.EscapeText(&out, []byte(value)); err != nil {
		// xml.EscapeText only fails when the writer fails, and bytes.Buffer does
		// not fail. Returning the escaped prefix keeps the function total without
		// inventing an error path no caller could act on.
		return out.String()
	}
	return out.String()
}
