package attachment

import (
	"bytes"
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

func TestValidateDisplayImageSkipsFormatsGoCannotDecode(t *testing.T) {
	if err := ValidateDisplayImage([]byte("<svg></svg>"), "image/svg+xml"); err != nil {
		t.Fatalf("svg: %v", err)
	}
	// A PNG header declaring an absurd size trips the pixel budget.
	huge := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x01\x00\x00\x00\x01\x00\x00\x08\x06\x00\x00\x00")
	if err := ValidateDisplayImage(huge, "image/png"); err == nil || !strings.Contains(err.Error(), "image/png") {
		t.Fatalf("oversized png: err = %v, want pixel budget error naming the format", err)
	}
}
