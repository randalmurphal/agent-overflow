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
