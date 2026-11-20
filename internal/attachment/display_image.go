package attachment

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	_ "golang.org/x/image/bmp"  // register bmp for image.DecodeConfig
	_ "golang.org/x/image/tiff" // register tiff for image.DecodeConfig
)

// DisplayImageMaxBytes caps a local image read for display in rendered
// markdown. Larger than DefaultMaxSize because these bytes are shown, not
// re-encoded into a model's context: a full-resolution screenshot of a 4K
// display is routinely over 10 MiB as PNG.
const DisplayImageMaxBytes int64 = 25 * 1024 * 1024

// DetectDisplayImageMIME sniffs the MIME type of an image that will be
// handed to a browser <img> for display. Wider than DetectImageMIME, whose
// set is what providers ingest: anything a browser paints is accepted here,
// which is the provider set plus bmp, ico, avif, tiff and svg.
//
// SVG is text, so its sniff is by structure (an <svg root after optional
// BOM, XML prolog, comments and doctype) rather than by magic bytes. In an
// <img> element the browser runs no script from it.
func DetectDisplayImageMIME(data []byte) (string, error) {
	if mime, err := DetectImageMIME(data); err == nil {
		return mime, nil
	}
	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		switch string(data[8:12]) {
		case "avif", "avis":
			return "image/avif", nil
		}
	}
	switch sniffed := http.DetectContentType(data); sniffed {
	case "image/bmp", "image/x-icon", "image/vnd.microsoft.icon", "image/tiff":
		return sniffed, nil
	}
	if looksLikeSVG(data) {
		return "image/svg+xml", nil
	}
	return "", errors.New("attachment: payload is not an image a browser displays")
}

// ValidateDisplayImage bounds the decode a browser will perform and returns
// how an <img> of the image is sized (DisplaySize). Formats Go can read the
// header of, and an avif whose size AVIFSize reads, get the pixel budget
// check; an avif of unknown size, an svg and an ico are accepted on the
// byte cap alone, which the caller has already applied: an svg answers
// SVGDisplaySize, an ico unknown.
func ValidateDisplayImage(data []byte, mime string) (DisplaySize, error) {
	var size DisplaySize
	var err error
	switch mime {
	case "image/svg+xml":
		return SVGDisplaySize(data), nil
	case "image/avif":
		size, err = AVIFSize(bytes.NewReader(data))
	case "image/x-icon", "image/vnd.microsoft.icon":
		return DisplaySize{}, nil
	default:
		size.Width, size.Height, err = ImageDimensions(data)
	}
	if err != nil {
		return DisplaySize{}, fmt.Errorf("%s: %w", mime, err)
	}
	return size, nil
}

func looksLikeSVG(data []byte) bool {
	_, ok := svgRoot(data)
	return ok
}

// svgSniffBytes is the prefix an svg's root is looked for in.
const svgSniffBytes = 4096

// svgRoot answers data from its root element's "<svg" when its first
// svgSniffBytes bytes open with one after an optional BOM, XML
// declaration, processing instructions, comments and doctype.
func svgRoot(data []byte) ([]byte, bool) {
	head := data
	if len(head) > svgSniffBytes {
		head = head[:svgSniffBytes]
	}
	head = bytes.TrimPrefix(head, []byte{0xEF, 0xBB, 0xBF})
	for {
		head = bytes.TrimLeft(head, xmlSpace)
		switch {
		case bytes.HasPrefix(head, []byte("<?")):
			end := bytes.Index(head, []byte("?>"))
			if end < 0 {
				return nil, false
			}
			head = head[end+2:]
		case bytes.HasPrefix(head, []byte("<!--")):
			end := bytes.Index(head, []byte("-->"))
			if end < 0 {
				return nil, false
			}
			head = head[end+3:]
		case bytes.HasPrefix(head, []byte("<!")):
			end := bytes.IndexByte(head, '>')
			if end < 0 {
				return nil, false
			}
			head = head[end+1:]
		case bytes.HasPrefix(head, []byte("<svg")):
			if len(head) == 4 || bytes.IndexByte([]byte(" >\t\r\n"), head[4]) >= 0 {
				return head, true
			}
			return nil, false
		default:
			return nil, false
		}
	}
}

const xmlSpace = " \t\r\n"

// DisplaySize is how an <img> of an image is sized when nothing else
// sizes it: Width x Height natural pixels, or, when Fluid, no natural
// size and a ratio Width:Height, which the browser fits to the image's
// container. The zero value is unknown.
type DisplaySize struct {
	Width  int
	Height int
	Fluid  bool
}

// The default object size CSS gives a replaced element per side it has no
// natural size for and cannot take from a ratio.
const (
	svgDefaultWidth  = 300
	svgDefaultHeight = 150
)

// fluidRatioTolerance is how far a fluid answer's integer terms may stray
// from the viewBox ratio, relative to it.
const fluidRatioTolerance = 1e-4

// SVGDisplaySize answers how an <img> of an svg is sized, from its root
// element's width, height, viewBox and preserveAspectRatio within its
// first svgSniffBytes bytes, read without an XML decoder or entity
// expansion:
//
//   - width and height: a number, or a number with a CSS absolute unit
//     (px, in, cm, mm, Q, pt, pc; any case), is definite; a percentage, the
//     keyword auto or no attribute is auto. Any other value (a font or
//     viewport relative unit, calc, an entity, an empty value, a repeated
//     attribute) answers unknown: its size depends on more than the file.
//   - viewBox: four numbers separated by whitespace and/or commas gives a
//     ratio when its width and height are positive, none when either is
//     zero or negative; any other form answers unknown. preserveAspectRatio
//     "none" answers unknown unless both sides are definite (Chromium then
//     sizes the image 300x150 whatever one side says), as does a value that
//     is not an align with an optional meet or slice.
//   - Both sides definite: that size. One definite and a ratio: the other
//     from the ratio. One definite and no ratio: the other is the default
//     (300 wide, 150 high). Neither and no ratio: 300x150. Neither and a
//     ratio: Fluid, the ratio as the smallest integer terms within
//     fluidRatioTolerance whose product is within the pixel budget.
//
// A definite answer is rounded to integers, each side at least 1 and the
// whole within the pixel budget (WithinPixelBudget), else unknown. The
// work is linear in the sniffed prefix and allocates nothing.
func SVGDisplaySize(data []byte) DisplaySize {
	root, ok := svgRoot(data)
	if !ok {
		return DisplaySize{}
	}
	attrs, ok := svgRootAttributes(root)
	if !ok {
		return DisplaySize{}
	}
	width, widthDefinite, widthOK := svgLength(attrs[svgWidth])
	height, heightDefinite, heightOK := svgLength(attrs[svgHeight])
	ratioW, ratioH, ratioOK := svgViewBoxRatio(attrs[svgViewBox])
	keepsRatio, aspectOK := svgKeepsRatio(attrs[svgPreserveAspectRatio])
	if !widthOK || !heightOK || !ratioOK || !aspectOK {
		return DisplaySize{}
	}
	if !keepsRatio && !(widthDefinite && heightDefinite) {
		return DisplaySize{}
	}
	hasRatio := ratioW > 0
	var w, h float64
	switch {
	case widthDefinite && heightDefinite:
		w, h = width, height
	case widthDefinite && hasRatio:
		w, h = width, width*ratioH/ratioW
	case heightDefinite && hasRatio:
		w, h = height*ratioW/ratioH, height
	case widthDefinite:
		w, h = width, svgDefaultHeight
	case heightDefinite:
		w, h = svgDefaultWidth, height
	case hasRatio:
		termW, termH, ok := ratioTerms(ratioW / ratioH)
		if !ok {
			return DisplaySize{}
		}
		return DisplaySize{Width: termW, Height: termH, Fluid: true}
	default:
		w, h = svgDefaultWidth, svgDefaultHeight
	}
	return pixelSize(w, h)
}

// pixelSize rounds w x h to integers, each at least 1 and the whole within
// the pixel budget, else unknown.
func pixelSize(w, h float64) DisplaySize {
	w, h = math.Round(w), math.Round(h)
	if !(w >= 1 && h >= 1 && w <= thumbPixelBudget && h <= thumbPixelBudget) || !WithinPixelBudget(int(w), int(h)) {
		return DisplaySize{}
	}
	return DisplaySize{Width: int(w), Height: int(h)}
}

// The root attributes SVGDisplaySize reads.
const (
	svgWidth = iota
	svgHeight
	svgViewBox
	svgPreserveAspectRatio
	svgAttributeCount
)

// svgRootAttributes answers the values of the attributes SVGDisplaySize
// reads off root, an svg root from its "<svg", nil for one absent. A root
// tag that does not end within root, an attribute that is not name="value"
// or name='value' after whitespace, a value holding '<' and a repeated
// attribute it reads answer false.
func svgRootAttributes(root []byte) ([svgAttributeCount][]byte, bool) {
	var attrs [svgAttributeCount][]byte
	p := root[len("<svg"):]
	for {
		n := len(p)
		p = bytes.TrimLeft(p, xmlSpace)
		if len(p) == 0 {
			return attrs, false
		}
		if p[0] == '>' || p[0] == '/' {
			return attrs, true
		}
		// XML separates attributes with whitespace.
		if len(p) == n {
			return attrs, false
		}
		eq := bytes.IndexByte(p, '=')
		if eq <= 0 {
			return attrs, false
		}
		name := bytes.TrimRight(p[:eq], xmlSpace)
		if len(name) == 0 || bytes.ContainsAny(name, xmlSpace+"<>/\"'") {
			return attrs, false
		}
		p = bytes.TrimLeft(p[eq+1:], xmlSpace)
		if len(p) == 0 || (p[0] != '"' && p[0] != '\'') {
			return attrs, false
		}
		end := bytes.IndexByte(p[1:], p[0])
		if end < 0 {
			return attrs, false
		}
		value := p[1 : 1+end]
		p = p[2+end:]
		if bytes.IndexByte(value, '<') >= 0 {
			return attrs, false
		}
		var slot int
		switch string(name) {
		case "width":
			slot = svgWidth
		case "height":
			slot = svgHeight
		case "viewBox":
			slot = svgViewBox
		case "preserveAspectRatio":
			slot = svgPreserveAspectRatio
		default:
			continue
		}
		if attrs[slot] != nil {
			return attrs, false
		}
		// An empty value is a non-nil empty slice: present, and refused by
		// its reader.
		attrs[slot] = value
	}
}

// svgAbsoluteUnits are the CSS absolute length units in CSS px.
var svgAbsoluteUnits = [...]struct {
	unit string
	px   float64
}{
	{"px", 1},
	{"in", 96},
	{"cm", 96 / 2.54},
	{"mm", 96 / 25.4},
	{"q", 96 / 101.6},
	{"pt", 96.0 / 72},
	{"pc", 16},
}

// svgLength reads a root width or height: CSS px and definite for a
// number or an absolute length, not definite for auto (absent, a
// percentage or the keyword auto), and not ok for any other value.
func svgLength(value []byte) (px float64, definite, ok bool) {
	if value == nil {
		return 0, false, true
	}
	value = bytes.Trim(value, xmlSpace)
	if bytes.EqualFold(value, []byte("auto")) {
		return 0, false, true
	}
	n := svgNumberPrefix(value)
	if n == 0 {
		return 0, false, false
	}
	f, err := strconv.ParseFloat(string(value[:n]), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false, false
	}
	unit := value[n:]
	switch {
	case len(unit) == 0:
		return f, true, true
	case len(unit) == 1 && unit[0] == '%':
		return 0, false, true
	}
	for _, u := range svgAbsoluteUnits {
		if bytes.EqualFold(unit, []byte(u.unit)) {
			return f * u.px, true, true
		}
	}
	return 0, false, false
}

// svgViewBoxRatio reads a viewBox: its width and height when both are
// positive, zeros for no viewBox or one with a zero or negative side, and
// false for anything but four numbers separated by whitespace and/or a
// comma.
func svgViewBoxRatio(value []byte) (width, height float64, ok bool) {
	if value == nil {
		return 0, 0, true
	}
	var terms [4]float64
	p := value
	for i := range terms {
		p = bytes.TrimLeft(p, xmlSpace)
		if i > 0 && len(p) > 0 && p[0] == ',' {
			p = bytes.TrimLeft(p[1:], xmlSpace)
		}
		n := svgNumberPrefix(p)
		if n == 0 {
			return 0, 0, false
		}
		f, err := strconv.ParseFloat(string(p[:n]), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, 0, false
		}
		terms[i] = f
		p = p[n:]
		// A number ends at a separator or the value's end.
		if len(p) > 0 && p[0] != ',' && bytes.IndexByte([]byte(xmlSpace), p[0]) < 0 {
			return 0, 0, false
		}
	}
	if len(bytes.Trim(p, xmlSpace)) != 0 {
		return 0, 0, false
	}
	if !(terms[2] > 0 && terms[3] > 0) {
		return 0, 0, true
	}
	return terms[2], terms[3], true
}

// svgAlignments are preserveAspectRatio's align values other than none.
var svgAlignments = [...]string{
	"xMinYMin", "xMidYMin", "xMaxYMin",
	"xMinYMid", "xMidYMid", "xMaxYMid",
	"xMinYMax", "xMidYMax", "xMaxYMax",
}

// svgKeepsRatio reads preserveAspectRatio: whether the viewBox ratio
// stands (not for "none"), and false as its second answer for a value that
// is not an align with an optional meet or slice.
func svgKeepsRatio(value []byte) (keeps, ok bool) {
	if value == nil {
		return true, true
	}
	fields := bytes.Fields(value)
	if len(fields) == 0 || len(fields) > 2 {
		return false, false
	}
	if len(fields) == 2 && string(fields[1]) != "meet" && string(fields[1]) != "slice" {
		return false, false
	}
	if string(fields[0]) == "none" {
		return false, true
	}
	for _, align := range svgAlignments {
		if string(fields[0]) == align {
			return true, true
		}
	}
	return false, false
}

// svgNumberPrefix is the length of the SVG number s opens with: an
// optional sign, digits with an optional fraction (or only a fraction) and
// an optional exponent; 0 when it opens with none. An 'e' not followed by
// digits ends the number, so "2em" is 2 and a unit.
func svgNumberPrefix(s []byte) int {
	digits := func(from int) int {
		for from < len(s) && s[from] >= '0' && s[from] <= '9' {
			from++
		}
		return from
	}
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	end := digits(i)
	whole := end - i
	i = end
	fraction := 0
	if i < len(s) && s[i] == '.' {
		end = digits(i + 1)
		if fraction = end - (i + 1); fraction > 0 {
			i = end
		}
	}
	if whole == 0 && fraction == 0 {
		return 0
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if end := digits(j); end > j {
			i = end
		}
	}
	return i
}

// ratioTerms answers the smallest integer terms w:h within
// fluidRatioTolerance of ratio (the first convergent of its continued
// fraction that close) whose product is within the pixel budget, or false.
func ratioTerms(ratio float64) (int, int, bool) {
	if !(ratio > 0) || math.IsInf(ratio, 0) {
		return 0, 0, false
	}
	// Convergents h/k: h(n) = a*h(n-1) + h(n-2), and k likewise.
	h1, h2 := 1.0, 0.0
	k1, k2 := 0.0, 1.0
	x := ratio
	// A float64's continued fraction ends within a few dozen terms.
	for range 64 {
		a := math.Floor(x)
		h, k := a*h1+h2, a*k1+k2
		if h > thumbPixelBudget || k > thumbPixelBudget || h*k > thumbPixelBudget {
			return 0, 0, false
		}
		if h >= 1 && k >= 1 && math.Abs(h/k-ratio) <= fluidRatioTolerance*ratio {
			return int(h), int(k), true
		}
		frac := x - a
		if frac <= 0 {
			return 0, 0, false
		}
		h1, h2, k1, k2 = h, h1, k, k1
		x = 1 / frac
	}
	return 0, 0, false
}
