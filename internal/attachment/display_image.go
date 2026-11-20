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
// the declared pixel size the check read. Formats Go can read the header of
// get the pixel budget check; formats it cannot (svg, ico, avif) are
// accepted on the byte cap alone, which the caller has already applied, and
// report a zero size, except an svg whose root declares a plain size
// (SVGSize).
func ValidateDisplayImage(data []byte, mime string) (width, height int, err error) {
	switch mime {
	case "image/svg+xml":
		width, height = SVGSize(data)
		return width, height, nil
	case "image/x-icon", "image/vnd.microsoft.icon", "image/avif":
		return 0, 0, nil
	}
	width, height, err = ImageDimensions(data)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", mime, err)
	}
	return width, height, nil
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

// SVGSize answers the pixel size an svg's root element declares, read
// from its first svgSniffBytes bytes without an XML decoder: both width
// and height as plain numbers or px lengths, positive and within the
// pixel budget (WithinPixelBudget), each rounded to an integer. Any other
// form (a percentage, another unit, an entity, a missing or repeated
// attribute, only a viewBox, a root tag that does not end in the prefix)
// answers 0x0: the browser sizes such an svg from its container, so no
// box can be reserved for it.
func SVGSize(data []byte) (width, height int) {
	root, ok := svgRoot(data)
	if !ok {
		return 0, 0
	}
	var widthValue, heightValue []byte
	p := root[len("<svg"):]
	for {
		n := len(p)
		p = bytes.TrimLeft(p, xmlSpace)
		if len(p) == 0 {
			return 0, 0
		}
		if p[0] == '>' || p[0] == '/' {
			break
		}
		// XML separates attributes with whitespace.
		if len(p) == n {
			return 0, 0
		}
		eq := bytes.IndexByte(p, '=')
		if eq <= 0 {
			return 0, 0
		}
		name := bytes.TrimRight(p[:eq], xmlSpace)
		if len(name) == 0 || bytes.ContainsAny(name, xmlSpace+"<>/\"'") {
			return 0, 0
		}
		p = bytes.TrimLeft(p[eq+1:], xmlSpace)
		if len(p) == 0 || (p[0] != '"' && p[0] != '\'') {
			return 0, 0
		}
		end := bytes.IndexByte(p[1:], p[0])
		if end < 0 {
			return 0, 0
		}
		value := p[1 : 1+end]
		p = p[2+end:]
		if bytes.IndexByte(value, '<') >= 0 {
			return 0, 0
		}
		var slot *[]byte
		switch string(name) {
		case "width":
			slot = &widthValue
		case "height":
			slot = &heightValue
		default:
			continue
		}
		if *slot != nil {
			return 0, 0
		}
		*slot = value
	}
	w, okW := svgPixels(widthValue)
	h, okH := svgPixels(heightValue)
	if !okW || !okH || !WithinPixelBudget(w, h) {
		return 0, 0
	}
	return w, h
}

// svgPixels reads an svg length that is a plain number or a px length,
// rounded to a positive integer.
func svgPixels(value []byte) (int, bool) {
	if value == nil {
		return 0, false
	}
	number := bytes.TrimSuffix(bytes.Trim(value, xmlSpace), []byte("px"))
	if !svgNumber(number) {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(number), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	rounded := math.Round(f)
	if rounded < 1 || rounded > thumbPixelBudget {
		return 0, false
	}
	return int(rounded), true
}

// svgNumber is whether s is an SVG number: an optional sign, digits with
// an optional fraction (or only a fraction) and an optional exponent.
func svgNumber(s []byte) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := func() int {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		return i - start
	}
	whole := digits()
	fraction := 0
	if i < len(s) && s[i] == '.' {
		i++
		if fraction = digits(); fraction == 0 {
			return false
		}
	}
	if whole == 0 && fraction == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if digits() == 0 {
			return false
		}
	}
	return i == len(s)
}
