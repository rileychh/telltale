package tableimg

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

//go:embed NotoSansTC.ttf
var cjkData []byte

//go:embed NotoSansTC-Bold.ttf
var cjkBoldData []byte

//go:embed NotoSans-Regular.ttf
var latinData []byte

//go:embed NotoSans-Bold.ttf
var latinBoldData []byte

//go:embed NotoSans-Italic.ttf
var latinItalicData []byte

//go:embed NotoSans-BoldItalic.ttf
var latinBoldItalicData []byte

//go:embed MonaspaceNeon.otf
var monoData []byte

//go:embed NotoEmoji.ttf
var emojiData []byte

// Table holds parsed markdown table data.
type Table struct {
	Header []string
	Rows   [][]string
}

// span is a segment of cell text with formatting.
type span struct {
	text   string
	code   bool
	bold   bool
	italic bool
}

var (
	bgColor     = color.White
	headerBg    = color.RGBA{R: 0xF0, G: 0xF0, B: 0xF0, A: 0xFF}
	borderColor = color.RGBA{R: 0xDD, G: 0xDD, B: 0xDD, A: 0xFF}
	textColor   = image.NewUniform(color.Black)
)

const (
	fontSize   = 16
	cellPadX   = 12
	cellPadY   = 8
	lineHeight = 24
)

type renderer struct {
	cjk            font.Face
	cjkBold        font.Face
	latin          font.Face
	latinBold      font.Face
	latinItalic    font.Face
	latinBoldItalic font.Face
	mono           font.Face
	emoji          font.Face
}

// Render draws the table as a PNG image and returns the bytes.
func Render(t Table) ([]byte, error) {
	r, err := newRenderer()
	if err != nil {
		return nil, err
	}

	ncols := len(t.Header)

	// Parse all cells into spans
	headerSpans := make([][]span, ncols)
	for i, h := range t.Header {
		headerSpans[i] = parseSpans(h)
	}
	rowSpans := make([][][]span, len(t.Rows))
	for ri, row := range t.Rows {
		rowSpans[ri] = make([][]span, ncols)
		for i := 0; i < ncols && i < len(row); i++ {
			rowSpans[ri][i] = parseSpans(row[i])
		}
	}

	// Measure column widths
	widths := make([]int, ncols)
	for i, spans := range headerSpans {
		w := r.measureSpans(spans)
		if w > widths[i] {
			widths[i] = w
		}
	}
	for _, row := range rowSpans {
		for i, spans := range row {
			w := r.measureSpans(spans)
			if w > widths[i] {
				widths[i] = w
			}
		}
	}

	// Calculate image dimensions
	totalW := 1
	for _, w := range widths {
		totalW += cellPadX + w + cellPadX + 1
	}
	rowH := cellPadY + lineHeight + cellPadY
	totalH := 1 + rowH + 1
	totalH += len(t.Rows) * (rowH + 1)

	// Create image
	img := image.NewRGBA(image.Rect(0, 0, totalW, totalH))
	draw.Draw(img, img.Bounds(), image.NewUniform(bgColor), image.Point{}, draw.Src)

	// Draw header background
	draw.Draw(img, image.Rect(0, 0, totalW, 1+rowH+1), image.NewUniform(headerBg), image.Point{}, draw.Src)

	// Draw horizontal lines
	drawHLine(img, 0, totalW, 0)
	drawHLine(img, 0, totalW, 1+rowH)
	for i := range t.Rows {
		y := 1 + rowH + 1 + (i+1)*(rowH+1) - 1
		drawHLine(img, 0, totalW, y)
	}

	// Draw vertical lines
	x := 0
	drawVLine(img, x, 0, totalH)
	for _, w := range widths {
		x += cellPadX + w + cellPadX + 1
		drawVLine(img, x, 0, totalH)
	}

	// Draw header text
	x = 1
	for i, spans := range headerSpans {
		textY := 1 + cellPadY + lineHeight - 4
		r.drawSpans(img, spans, x+cellPadX, textY)
		x += cellPadX + widths[i] + cellPadX + 1
	}

	// Draw row text
	for ri, row := range rowSpans {
		x = 1
		baseY := 1 + rowH + 1 + ri*(rowH+1)
		for i := 0; i < ncols; i++ {
			var spans []span
			if i < len(row) {
				spans = row[i]
			}
			textY := baseY + cellPadY + lineHeight - 4
			r.drawSpans(img, spans, x+cellPadX, textY)
			x += cellPadX + widths[i] + cellPadX + 1
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var reMarkdownImage = regexp.MustCompile(`!\[([^\]]*)\]\([^)]+\)`)

// parseSpans splits cell text into spans with code, bold, and italic formatting.
func parseSpans(s string) []span {
	// Replace markdown images with italic placeholder
	s = reMarkdownImage.ReplaceAllStringFunc(s, func(match string) string {
		alt := reMarkdownImage.FindStringSubmatch(match)[1]
		if alt != "" {
			return "*[Image: " + alt + "]*"
		}
		return "*[Image]*"
	})

	// First pass: split on backticks for code spans
	var raw []span
	for {
		idx := strings.IndexByte(s, '`')
		if idx < 0 {
			if s != "" {
				raw = append(raw, span{text: s})
			}
			break
		}
		if idx > 0 {
			raw = append(raw, span{text: s[:idx]})
		}
		s = s[idx+1:]
		end := strings.IndexByte(s, '`')
		if end < 0 {
			raw = append(raw, span{text: "`" + s})
			break
		}
		raw = append(raw, span{text: s[:end], code: true})
		s = s[end+1:]
	}

	// Second pass: parse bold/italic markers in non-code spans
	var result []span
	for _, sp := range raw {
		if sp.code {
			result = append(result, sp)
			continue
		}
		result = append(result, parseFormatting(sp.text)...)
	}
	return result
}

// parseFormatting parses ***, **, and * markers for bold/italic.
func parseFormatting(s string) []span {
	var spans []span
	for len(s) > 0 {
		// Find the first * or _ marker
		markerIdx := -1
		for i, ch := range s {
			if ch == '*' || ch == '_' {
				markerIdx = i
				break
			}
		}
		if markerIdx < 0 {
			spans = append(spans, span{text: s})
			break
		}

		// Emit text before marker
		if markerIdx > 0 {
			spans = append(spans, span{text: s[:markerIdx]})
		}

		marker := rune(s[markerIdx])
		rest := s[markerIdx:]

		// Count consecutive marker chars
		markerLen := 0
		for _, ch := range rest {
			if ch == marker {
				markerLen++
			} else {
				break
			}
		}

		// Find closing marker of same length
		content := rest[markerLen:]
		closeMarker := strings.Repeat(string(marker), markerLen)
		closeIdx := strings.Index(content, closeMarker)

		if closeIdx < 0 {
			// No closing marker — treat as literal text
			spans = append(spans, span{text: rest[:markerLen]})
			s = content
			continue
		}

		inner := content[:closeIdx]
		bold := markerLen >= 2
		italic := markerLen == 1 || markerLen == 3

		spans = append(spans, span{text: inner, bold: bold, italic: italic})
		s = content[closeIdx+markerLen:]
	}
	return spans
}

// isEmoji returns true if the rune likely needs the emoji font.
func isEmoji(r rune) bool {
	if r < 0x200D {
		return false
	}
	switch {
	case r >= 0x1F300 && r <= 0x1F9FF:
		return true
	case r >= 0x2600 && r <= 0x27BF:
		return true
	case r == 0x200D || r == 0xFE0F:
		return true
	case r >= 0x1FA00 && r <= 0x1FAFF:
		return true
	case r >= 0x2300 && r <= 0x23FF:
		return true
	case r >= 0x25A0 && r <= 0x25FF:
		return true
	case r >= 0x2B00 && r <= 0x2BFF:
		return true
	case unicode.Is(unicode.So, r):
		return true
	}
	return false
}

// runKind classifies a text run for font selection.
type runKind int

const (
	runLatin runKind = iota
	runCJK
	runEmoji
)

type textRun struct {
	text string
	kind runKind
}

// isCJK returns true if the rune is a CJK character that needs the TC font.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Bopomofo, r) ||
		(r >= 0x3000 && r <= 0x303F) || // CJK Symbols and Punctuation
		(r >= 0xFF00 && r <= 0xFFEF) // Fullwidth Forms
}

func classifyRune(r rune) runKind {
	if isEmoji(r) {
		return runEmoji
	}
	if isCJK(r) {
		return runCJK
	}
	return runLatin
}

// splitTextRuns splits text into runs of Latin, CJK, and emoji characters.
func splitTextRuns(s string) []textRun {
	var runs []textRun
	runes := []rune(s)
	i := 0
	for i < len(runes) {
		kind := classifyRune(runes[i])
		j := i + 1
		for j < len(runes) && classifyRune(runes[j]) == kind {
			j++
		}
		runs = append(runs, textRun{text: string(runes[i:j]), kind: kind})
		i = j
	}
	return runs
}

// faceForRun returns the font face for a span + run kind combination.
// CJK characters use the TC fonts (no italic variant available).
// Latin characters use the full Noto Sans set with italic support.
func (r *renderer) faceForRun(sp span, kind runKind) font.Face {
	if kind == runEmoji {
		return r.emoji
	}
	if sp.code {
		return r.mono
	}
	if kind == runCJK {
		if sp.bold {
			return r.cjkBold
		}
		return r.cjk
	}
	switch {
	case sp.bold && sp.italic:
		return r.latinBoldItalic
	case sp.bold:
		return r.latinBold
	case sp.italic:
		return r.latinItalic
	}
	return r.latin
}

func (r *renderer) measureSpans(spans []span) int {
	total := 0
	for _, sp := range spans {
		for _, run := range splitTextRuns(sp.text) {
			f := r.faceForRun(sp, run.kind)
			d := &font.Drawer{Face: f}
			total += d.MeasureString(run.text).Ceil()
		}
	}
	return total
}

func (r *renderer) drawSpans(dst *image.RGBA, spans []span, x, y int) {
	for _, sp := range spans {
		for _, run := range splitTextRuns(sp.text) {
			f := r.faceForRun(sp, run.kind)
			d := &font.Drawer{
				Dst:  dst,
				Src:  textColor,
				Face: f,
				Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
			}
			d.DrawString(run.text)
			x = d.Dot.X.Ceil()
		}
	}
}

func newRenderer() (*renderer, error) {
	parseFace := func(data []byte) (font.Face, error) {
		f, err := opentype.Parse(data)
		if err != nil {
			return nil, err
		}
		return opentype.NewFace(f, &opentype.FaceOptions{
			Size:    fontSize,
			DPI:     144,
			Hinting: font.HintingFull,
		})
	}

	cjkFace, err := parseFace(cjkData)
	if err != nil {
		return nil, err
	}
	cjkBoldFace, err := parseFace(cjkBoldData)
	if err != nil {
		return nil, err
	}
	latinFace, err := parseFace(latinData)
	if err != nil {
		return nil, err
	}
	latinBoldFace, err := parseFace(latinBoldData)
	if err != nil {
		return nil, err
	}
	latinItalicFace, err := parseFace(latinItalicData)
	if err != nil {
		return nil, err
	}
	latinBoldItalicFace, err := parseFace(latinBoldItalicData)
	if err != nil {
		return nil, err
	}
	monoFace, err := parseFace(monoData)
	if err != nil {
		return nil, err
	}
	emojiFace, err := parseFace(emojiData)
	if err != nil {
		return nil, err
	}
	return &renderer{
		cjk:             cjkFace,
		cjkBold:         cjkBoldFace,
		latin:           latinFace,
		latinBold:       latinBoldFace,
		latinItalic:     latinItalicFace,
		latinBoldItalic: latinBoldItalicFace,
		mono:            monoFace,
		emoji:           emojiFace,
	}, nil
}

func drawHLine(img *image.RGBA, x0, x1, y int) {
	for x := x0; x < x1; x++ {
		img.Set(x, y, borderColor)
	}
}

func drawVLine(img *image.RGBA, x, y0, y1 int) {
	for y := y0; y < y1; y++ {
		img.Set(x, y, borderColor)
	}
}
