package attachment

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"strings"
	"testing"

	"golang.org/x/image/bmp"
)

func TestDetectDisplayImageMIME(t *testing.T) {
	var pngBuf, bmpBuf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	if err := bmp.Encode(&bmpBuf, img); err != nil {
		t.Fatal(err)
	}
	avif := append([]byte{0, 0, 0, 0x1c}, []byte("ftypavif")...)
	avif = append(avif, make([]byte, 16)...)
	ico := append([]byte{0, 0, 1, 0, 1, 0}, make([]byte, 16)...)

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png", pngBuf.Bytes(), "image/png"},
		{"bmp", bmpBuf.Bytes(), "image/bmp"},
		{"avif", avif, "image/avif"},
		{"ico", ico, "image/x-icon"},
		{"svg bare", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), "image/svg+xml"},
		{"svg with prolog, comment and doctype", []byte("\uFEFF<?xml version=\"1.0\"?>\n<!-- made by hand -->\n<!DOCTYPE svg>\n<svg>\n</svg>"), "image/svg+xml"},
	}
	for _, tc := range cases {
		got, err := DetectDisplayImageMIME(tc.data)
		if err != nil || got != tc.want {
			t.Errorf("%s: DetectDisplayImageMIME = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}

	for name, data := range map[string][]byte{
		"text":            []byte("not an image"),
		"html":            []byte("<html><body><svg></svg></body></html>"),
		"svg-named other": []byte("<svgfoo>"),
		"pdf":             []byte("%PDF-1.4"),
		"empty":           nil,
	} {
		if got, err := DetectDisplayImageMIME(data); err == nil {
			t.Errorf("%s: DetectDisplayImageMIME = %q, want error", name, got)
		}
	}
}

func TestValidateDisplayImage(t *testing.T) {
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 6, 4))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mime string
		data       []byte
		want       DisplaySize
	}{
		{"png", "image/png", pngBuf.Bytes(), DisplaySize{Width: 6, Height: 4}},
		{"svg with no size", "image/svg+xml", []byte("<svg></svg>"), DisplaySize{Width: 300, Height: 150}},
		{"fluid svg", "image/svg+xml", []byte(`<svg viewBox="0 0 64 32"></svg>`), DisplaySize{Width: 2, Height: 1, Fluid: true}},
		{"svg sized by its font", "image/svg+xml", []byte(`<svg width="2em" height="1em"></svg>`), DisplaySize{}},
		{"avif", "image/avif", fixture(t, "solid-8x3.avif"), DisplaySize{Width: 8, Height: 3}},
		// A size the reader cannot read is shown as it is, with no size.
		{"avif with a clap", "image/avif", avifFile(avifMetaBox(1, avifItemProperties(1, avifIspe(60000, 60000), avifClap()))), DisplaySize{}},
		{"ico", "image/x-icon", append([]byte{0, 0, 1, 0, 1, 0}, make([]byte, 16)...), DisplaySize{}},
	} {
		got, err := ValidateDisplayImage(tc.data, tc.mime)
		if err != nil || got != tc.want {
			t.Errorf("%s: ValidateDisplayImage = %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
	// A header declaring an absurd size trips the pixel budget, whatever
	// the format: the product, or one side past what the product allows.
	for _, tc := range []struct {
		name, mime string
		data       []byte
	}{
		{"png", "image/png", hugePNGHeader()},
		{"avif", "image/avif", avifFile(avifMetaBox(1, avifItemProperties(1, avifIspe(60000, 60000))))},
		{"avif one side", "image/avif", avifFile(avifMetaBox(1, avifItemProperties(1, avifIspe(1, 0xffffffff))))},
	} {
		got, err := ValidateDisplayImage(tc.data, tc.mime)
		if err == nil || !strings.Contains(err.Error(), tc.mime) || !errors.Is(err, ErrPixelBudget) {
			t.Errorf("oversized %s: ValidateDisplayImage = %+v, %v; want ErrPixelBudget naming the format", tc.name, got, err)
		}
	}
}

// hugePNGHeader is a well-formed PNG signature and IHDR declaring
// 60000x60000 pixels, with the CRC the decoder verifies before it reads
// the size.
func hugePNGHeader() []byte {
	ihdr := make([]byte, 0, 25)
	ihdr = append(ihdr, 'I', 'H', 'D', 'R')
	ihdr = binary.BigEndian.AppendUint32(ihdr, 60000)
	ihdr = binary.BigEndian.AppendUint32(ihdr, 60000)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)

	out := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	out = binary.BigEndian.AppendUint32(out, uint32(len(ihdr)-4))
	out = append(out, ihdr...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(ihdr))
}

// TestSVGDisplaySize is the sizing matrix: the CSS default sizing of an
// <img> with no authored size, which the Chromium test
// imageDisplaySize.browser.test.ts (frontend/src/lib/utils) checks row by row
// against the same answers.
func TestSVGDisplaySize(t *testing.T) {
	const ns = `xmlns="http://www.w3.org/2000/svg"`
	size := func(w, h int) DisplaySize { return DisplaySize{Width: w, Height: h} }
	fluid := func(w, h int) DisplaySize { return DisplaySize{Width: w, Height: h, Fluid: true} }
	unknown := DisplaySize{}
	for _, tc := range []struct {
		name string
		doc  string
		want DisplaySize
	}{
		// Both sides definite.
		{"numbers", `<svg ` + ns + ` width="120" height="80"></svg>`, size(120, 80)},
		{"px", `<svg width="120px" height="80px"/>`, size(120, 80)},
		{"units in any case", `<svg width="5PX" height="6Px"></svg>`, size(5, 6)},
		{"decimals round", `<svg width="10.4" height="10.5"></svg>`, size(10, 11)},
		{"exponent and padding", `<svg width=" 1.2e2 " height="8E1"></svg>`, size(120, 80)},
		{"in", `<svg width="1in" height="0.5IN"></svg>`, size(96, 48)},
		{"cm and mm", `<svg width="2.54cm" height="25.4mm"></svg>`, size(96, 96)},
		{"Q", `<svg width="40Q" height="10q"></svg>`, size(38, 9)},
		{"pt and pc", `<svg width="72pt" height="6pc"></svg>`, size(96, 96)},
		{"single quotes", `<svg width='7' height='9'></svg>`, size(7, 9)},
		{"mixed quotes and spacing", "<svg\n\twidth = '7'\r\n height=\"9\">", size(7, 9)},
		{"a viewBox does not change both definite sides", `<svg ` + ns + ` viewBox="0 0 1 1" width="3" height="4"><rect width="1" height="1"/></svg>`, size(3, 4)},
		{"prolog, comment and doctype", "\uFEFF<?xml version=\"1.0\"?>\n<!-- <svg width=\"1\" height=\"1\"> -->\n<!DOCTYPE svg>\n<svg width=\"5\" height=\"6\"></svg>", size(5, 6)},
		// One side definite.
		{"only width", `<svg width="100"></svg>`, size(100, 150)},
		{"only height", `<svg height="80"></svg>`, size(300, 80)},
		{"percent width", `<svg width="50%" height="50"></svg>`, size(300, 50)},
		{"auto width", `<svg width="auto" height="50"></svg>`, size(300, 50)},
		{"width and a ratio", `<svg width="100" viewBox="0 0 200 100"></svg>`, size(100, 50)},
		{"height and a ratio", `<svg height="100" viewBox="0 0 200 100"></svg>`, size(200, 100)},
		{"width and a ratio rounds", `<svg width="10" viewBox="0 0 3 1"></svg>`, size(10, 3)},
		// Neither side definite.
		{"no size", `<svg ` + ns + `></svg>`, size(300, 150)},
		{"percent both", `<svg width="100%" height="50%"></svg>`, size(300, 150)},
		{"uppercase attribute names are other attributes", `<svg WIDTH="5" HEIGHT="6"></svg>`, size(300, 150)},
		{"a later element's size is its own", `<svg ` + ns + `><rect width="5" height="6"/></svg>`, size(300, 150)},
		{"zero viewBox width", `<svg viewBox="0 0 0 100"></svg>`, size(300, 150)},
		{"negative viewBox width", `<svg viewBox="0 0 -200 100"></svg>`, size(300, 150)},
		{"only viewBox", `<svg viewBox="0 0 200 100"></svg>`, fluid(2, 1)},
		{"viewBox with commas", `<svg viewBox="0,0,200,100"></svg>`, fluid(2, 1)},
		{"viewBox with commas and spaces", `<svg viewBox=" 0, 0 ,200 , 100 "></svg>`, fluid(2, 1)},
		{"percent both and a viewBox", `<svg width="100%" height="100%" viewBox="0 0 64 48"></svg>`, fluid(4, 3)},
		{"square viewBox", `<svg viewBox="0 0 24 24"></svg>`, fluid(1, 1)},
		{"fractional viewBox", `<svg viewBox="0 0 16.5 9"></svg>`, fluid(11, 6)},
		{"irrational ratio", `<svg viewBox="0 0 3.14159265 1"></svg>`, fluid(333, 106)},
		{"both sides and preserveAspectRatio none", `<svg width="100" height="40" viewBox="0 0 200 100" preserveAspectRatio="none"></svg>`, size(100, 40)},
		{"preserveAspectRatio keeps the ratio", `<svg viewBox="0 0 200 100" preserveAspectRatio="xMinYMax slice"></svg>`, fluid(2, 1)},
		// Unknown.
		{"em", `<svg width="2em" height="2em"></svg>`, unknown},
		{"ex", `<svg width="2ex" height="10"></svg>`, unknown},
		{"rem", `<svg width="2rem" height="10"></svg>`, unknown},
		{"viewport unit", `<svg width="10vw" height="10"></svg>`, unknown},
		{"ch", `<svg width="10ch" height="10"></svg>`, unknown},
		{"calc", `<svg width="calc(10px + 5px)" height="20"></svg>`, unknown},
		{"empty", `<svg width="" height="50"></svg>`, unknown},
		{"malformed viewBox", `<svg viewBox="0 0 200"></svg>`, unknown},
		{"viewBox with a fifth number", `<svg viewBox="0 0 200 100 5"></svg>`, unknown},
		{"viewBox of words", `<svg viewBox="a b c d"></svg>`, unknown},
		{"viewBox with doubled commas", `<svg viewBox="0,,0,200,100"></svg>`, unknown},
		// Chromium sizes these 300x150 whatever the one side says.
		{"preserveAspectRatio none", `<svg viewBox="0 0 200 100" preserveAspectRatio="none"></svg>`, unknown},
		{"width, a ratio and preserveAspectRatio none", `<svg width="100" viewBox="0 0 200 100" preserveAspectRatio="none"></svg>`, unknown},
		{"width and preserveAspectRatio none", `<svg width="100" preserveAspectRatio="none"></svg>`, unknown},
		{"unknown preserveAspectRatio", `<svg viewBox="0 0 200 100" preserveAspectRatio="stretch"></svg>`, unknown},
		{"ratio past the pixel budget", `<svg viewBox="0 0 1 1e-9"></svg>`, unknown},
		{"huge side", `<svg width="100000000" height="1"></svg>`, unknown},
		{"over the pixel budget", `<svg width="10000" height="10000"></svg>`, unknown},
		{"over the pixel budget from the default", `<svg width="400000"></svg>`, unknown},
		{"infinite", `<svg width="1e999" height="5"></svg>`, unknown},
		{"zero", `<svg width="0" height="5"></svg>`, unknown},
		{"zero with a ratio", `<svg width="0" viewBox="0 0 2 1"></svg>`, unknown},
		{"negative", `<svg width="-5" height="5"></svg>`, unknown},
		{"rounds to zero", `<svg width="0.4" height="5"></svg>`, unknown},
		{"entity", `<svg width="&w;" height="5"></svg>`, unknown},
		{"hex float", `<svg width="0x10" height="5"></svg>`, unknown},
		{"inf word", `<svg width="Inf" height="5"></svg>`, unknown},
		{"repeated attribute", `<svg width="5" width="6" height="5"></svg>`, unknown},
		{"repeated viewBox", `<svg viewBox="0 0 1 1" viewBox="0 0 2 1"></svg>`, unknown},
		{"no space between attributes", `<svg width="5"height="6"></svg>`, unknown},
		{"unquoted", `<svg width=5 height=6></svg>`, unknown},
		{"root tag past the prefix", `<svg ` + strings.Repeat(`a="b" `, 800) + `width="5" height="6">`, unknown},
		{"not svg", `<html width="5" height="6">`, unknown},
	} {
		if got := SVGDisplaySize([]byte(tc.doc)); got != tc.want {
			t.Errorf("%s: SVGDisplaySize = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Every fluid answer keeps the viewBox ratio within the tolerance, in
// terms within the pixel budget.
func TestRatioTermsStayWithinTheTolerance(t *testing.T) {
	for _, ratio := range []float64{1, 2, 0.5, 16.0 / 9, 1.6180339887, 1e-4, 1234.5678, 7.0 / 3000, 0.999999} {
		w, h, ok := ratioTerms(ratio)
		if !ok || w < 1 || h < 1 || !WithinPixelBudget(w, h) {
			t.Fatalf("ratioTerms(%g) = %d:%d, %v", ratio, w, h, ok)
		}
		if diff := float64(w)/float64(h)/ratio - 1; diff > fluidRatioTolerance || diff < -fluidRatioTolerance {
			t.Errorf("ratioTerms(%g) = %d:%d, off by %g", ratio, w, h, diff)
		}
	}
}

// The svg reader's work is bounded by its sniffed prefix and allocates
// nothing: a 25 MiB document costs what its first svgSniffBytes do, and
// a prefix that is all attributes is answered without an allocation.
func TestSVGDisplaySizeWorkload(t *testing.T) {
	root := `<svg width="120" height="80"` + strings.Repeat(` a="b"`, (svgSniffBytes-64)/6) + `>`
	huge := append([]byte(root), bytes.Repeat([]byte(`<rect width="1" height="1"/>`), int(DisplayImageMaxBytes/28))...)
	if got := SVGDisplaySize(huge); got != (DisplaySize{Width: 120, Height: 80}) {
		t.Fatalf("SVGDisplaySize = %+v, want 120x80", got)
	}
	if allocs := testing.AllocsPerRun(20, func() { SVGDisplaySize(huge) }); allocs != 0 {
		t.Fatalf("SVGDisplaySize allocated %v times per run, want none", allocs)
	}
	fluidDoc := []byte(`<svg viewBox="0 0 3.14159265 1"` + strings.Repeat(` a="b"`, (svgSniffBytes-64)/6) + `>`)
	if allocs := testing.AllocsPerRun(20, func() { SVGDisplaySize(fluidDoc) }); allocs != 0 {
		t.Fatalf("a fluid answer allocated %v times per run, want none", allocs)
	}
	// A root past the prefix is not looked for.
	late := append(bytes.Repeat([]byte(" "), svgSniffBytes), []byte(`<svg width="5" height="6"></svg>`)...)
	if got := SVGDisplaySize(late); got != (DisplaySize{}) {
		t.Fatalf("a root past the prefix = %+v, want unknown", got)
	}
}
