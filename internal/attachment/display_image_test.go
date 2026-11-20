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

func TestValidateDisplayImageReportsTheDeclaredSize(t *testing.T) {
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 6, 4))); err != nil {
		t.Fatal(err)
	}
	width, height, err := ValidateDisplayImage(pngBuf.Bytes(), "image/png")
	if err != nil {
		t.Fatalf("png: %v", err)
	}
	if width != 6 || height != 4 {
		t.Fatalf("size = %dx%d, want 6x4", width, height)
	}
}

func TestValidateDisplayImageSkipsFormatsGoCannotDecode(t *testing.T) {
	width, height, err := ValidateDisplayImage([]byte("<svg></svg>"), "image/svg+xml")
	if err != nil {
		t.Fatalf("svg: %v", err)
	}
	if width != 0 || height != 0 {
		t.Fatalf("svg size = %dx%d, want unknown (0x0)", width, height)
	}
	// A PNG header declaring an absurd size trips the pixel budget.
	_, _, err = ValidateDisplayImage(hugePNGHeader(), "image/png")
	if err == nil || !strings.Contains(err.Error(), "image/png") || !errors.Is(err, ErrPixelBudget) {
		t.Fatalf("oversized png: err = %v, want ErrPixelBudget naming the format", err)
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

// SVGSize answers only a root's plain or px width and height; every form
// whose size depends on the container is 0x0.
func TestSVGSize(t *testing.T) {
	const ns = `xmlns="http://www.w3.org/2000/svg"`
	for _, tc := range []struct {
		name string
		doc  string
		w, h int
	}{
		{"numbers", `<svg ` + ns + ` width="120" height="80"></svg>`, 120, 80},
		{"px", `<svg width="120px" height="80px"/>`, 120, 80},
		{"decimals round", `<svg width="10.4" height="10.5"></svg>`, 10, 11},
		{"exponent and padding", `<svg width=" 1.2e2 " height="8E1"></svg>`, 120, 80},
		{"single quotes", `<svg width='7' height='9'></svg>`, 7, 9},
		{"mixed quotes and spacing", "<svg\n\twidth = '7'\r\n height=\"9\">", 7, 9},
		{"after other attributes", `<svg ` + ns + ` viewBox="0 0 1 1" width="3" height="4"><rect width="1" height="1"/></svg>`, 3, 4},
		{"prolog, comment and doctype", "\uFEFF<?xml version=\"1.0\"?>\n<!-- <svg width=\"1\" height=\"1\"> -->\n<!DOCTYPE svg>\n<svg width=\"5\" height=\"6\"></svg>", 5, 6},
		{"uppercase attribute names", `<svg WIDTH="5" HEIGHT="6"></svg>`, 0, 0},
		{"uppercase unit", `<svg width="5PX" height="6PX"></svg>`, 0, 0},
		{"percent", `<svg width="100%" height="50"></svg>`, 0, 0},
		{"em", `<svg width="2em" height="2em"></svg>`, 0, 0},
		{"only width", `<svg width="5"></svg>`, 0, 0},
		{"only viewBox", `<svg viewBox="0 0 100 50"></svg>`, 0, 0},
		{"size on a later element", `<svg ` + ns + `><rect width="5" height="6"/></svg>`, 0, 0},
		{"huge side", `<svg width="100000000" height="1"></svg>`, 0, 0},
		{"over the pixel budget", `<svg width="10000" height="10000"></svg>`, 0, 0},
		{"infinite", `<svg width="1e999" height="5"></svg>`, 0, 0},
		{"zero", `<svg width="0" height="5"></svg>`, 0, 0},
		{"negative", `<svg width="-5" height="5"></svg>`, 0, 0},
		{"rounds to zero", `<svg width="0.4" height="5"></svg>`, 0, 0},
		{"entity", `<svg width="&w;" height="5"></svg>`, 0, 0},
		{"hex float", `<svg width="0x10" height="5"></svg>`, 0, 0},
		{"inf word", `<svg width="Inf" height="5"></svg>`, 0, 0},
		{"repeated attribute", `<svg width="5" width="6" height="5"></svg>`, 0, 0},
		{"no space between attributes", `<svg width="5"height="6"></svg>`, 0, 0},
		{"unquoted", `<svg width=5 height=6></svg>`, 0, 0},
		{"root tag past the prefix", `<svg ` + strings.Repeat(`a="b" `, 800) + `width="5" height="6">`, 0, 0},
		{"not svg", `<html width="5" height="6">`, 0, 0},
	} {
		if w, h := SVGSize([]byte(tc.doc)); w != tc.w || h != tc.h {
			t.Errorf("%s: SVGSize = %dx%d, want %dx%d", tc.name, w, h, tc.w, tc.h)
		}
	}
}
