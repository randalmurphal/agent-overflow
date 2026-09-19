package forgeattach

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestClassifyImage(t *testing.T) {
	mimeType, kind, err := Classify(tinyPNG(t), "whatever-the-reference-said")
	if err != nil {
		t.Fatalf("Classify returned error: %v", err)
	}
	if mimeType != "image/png" || kind != KindImage {
		t.Fatalf("Classify = (%q, %q), want (image/png, image)", mimeType, kind)
	}
}

// TestClassifyRefusesADecodeBomb: a payload that IS an image but decodes
// to an absurd number of pixels reaches the same <img> a safe one does,
// so it has to fail the call rather than fall through to a download the
// caller would then render anyway.
func TestClassifyRefusesADecodeBomb(t *testing.T) {
	if _, _, err := Classify(hugePNGHeader(), "bomb.png"); err == nil {
		t.Fatal("Classify accepted a PNG declaring 60000x60000 pixels")
	}
}

func TestClassifyMedia(t *testing.T) {
	cases := []struct {
		name     string
		data     []byte
		filename string
		mimeType string
		kind     string
	}{
		{name: "mp4 isom", data: ftypBox("isom", "isomiso2avc1mp41"), mimeType: "video/mp4", kind: KindVideo},
		{name: "mp4 by compatible brand only", data: ftypBox("3gp4", "3gp4mp42"), mimeType: "video/mp4", kind: KindVideo},
		{name: "quicktime", data: ftypBox("qt  ", "qt  "), mimeType: "video/quicktime", kind: KindVideo},
		{name: "m4a", data: ftypBox("M4A ", "M4A mp42isom"), mimeType: "audio/mp4", kind: KindAudio},
		{name: "webm", data: append([]byte{0x1A, 0x45, 0xDF, 0xA3}, bytes.Repeat([]byte{0}, 64)...), mimeType: "video/webm", kind: KindVideo},
		{name: "avi", data: riff("AVI ", 64), mimeType: "video/avi", kind: KindVideo},
		{name: "wav", data: riff("WAVE", 64), mimeType: "audio/wav", kind: KindAudio},
		{name: "mp3 with an id3 tag", data: append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{0}, 64)...), mimeType: "audio/mpeg", kind: KindAudio},
		{name: "flac", data: append([]byte("fLaC"), bytes.Repeat([]byte{0}, 64)...), mimeType: "audio/flac", kind: KindAudio},
		{name: "adts aac", data: append([]byte{0xFF, 0xF1, 0x50, 0x80}, bytes.Repeat([]byte{0}, 64)...), mimeType: "audio/aac", kind: KindAudio},
		{name: "ogg theora", data: oggPage("\x80theora"), mimeType: "video/ogg", kind: KindVideo},
		{name: "ogg vorbis", data: oggPage("\x01vorbis"), mimeType: "audio/ogg", kind: KindAudio},
		{name: "ogg opus", data: oggPage("OpusHead"), mimeType: "audio/ogg", kind: KindAudio},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mimeType, kind, err := Classify(tc.data, tc.filename)
			if err != nil {
				t.Fatalf("Classify returned error: %v", err)
			}
			if mimeType != tc.mimeType || kind != tc.kind {
				t.Fatalf("Classify = (%q, %q), want (%q, %q)", mimeType, kind, tc.mimeType, tc.kind)
			}
		})
	}
}

// TestClassifyFallsBackToTheExtension covers the only case the filename
// is consulted for: bytes no signature claimed. The mime is display
// only, which is why a .html payload landing on text/html here is safe —
// the route serves every KindFile as application/octet-stream.
func TestClassifyFallsBackToTheExtension(t *testing.T) {
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0}, 64)...)
	mimeType, kind, err := Classify(pdf, "report.pdf")
	if err != nil {
		t.Fatalf("Classify returned error: %v", err)
	}
	if kind != KindFile {
		t.Fatalf("kind = %q, want file", kind)
	}
	if !strings.HasPrefix(mimeType, "application/pdf") {
		t.Fatalf("mime = %q, want application/pdf", mimeType)
	}

	// A GitHub asset reference carries no extension at all, and nothing
	// may be invented for it.
	mimeType, kind, err = Classify([]byte("\x00\x01\x02 not a known container"), "1f0c2c2e-1111")
	if err != nil {
		t.Fatalf("Classify returned error: %v", err)
	}
	if kind != KindFile || mimeType != "application/octet-stream" {
		t.Fatalf("Classify = (%q, %q), want (application/octet-stream, file)", mimeType, kind)
	}
}

func TestClassifyRefusesAnEmptyBody(t *testing.T) {
	if _, _, err := Classify(nil, "a.png"); err == nil {
		t.Fatal("Classify accepted an empty body")
	}
}

// TestIsoBrandsIgnoresADeclaredBoxSize pins the clamp: the box length is
// a number from the payload, and a crafted one must not decide how long
// the brand scan runs.
func TestIsoBrandsIgnoresADeclaredBoxSize(t *testing.T) {
	data := ftypBox("isom", "isom")
	binary.BigEndian.PutUint32(data[:4], 0xFFFFFFFF)
	brands, ok := isoBrands(data)
	if !ok {
		t.Fatal("isoBrands did not recognize the ftyp box")
	}
	if len(brands) > 64 {
		t.Fatalf("isoBrands returned %d brands from a %d byte payload", len(brands), len(data))
	}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// hugePNGHeader is a real PNG signature plus an IHDR declaring more
// pixels than the decode budget allows. Only the header is parsed, so
// nothing beyond it is needed.
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

func ftypBox(major, brands string) []byte {
	body := make([]byte, 0, 16+len(brands))
	body = append(body, 'f', 't', 'y', 'p')
	body = append(body, major...)
	body = append(body, 0, 0, 2, 0)
	body = append(body, brands...)
	out := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	out = append(out, body...)
	return append(out, bytes.Repeat([]byte{0}, 64)...)
}

func riff(form string, payload int) []byte {
	out := []byte("RIFF")
	out = binary.LittleEndian.AppendUint32(out, uint32(payload+4))
	out = append(out, form...)
	return append(out, bytes.Repeat([]byte{0}, payload)...)
}

func oggPage(codec string) []byte {
	out := []byte("OggS")
	out = append(out, bytes.Repeat([]byte{0}, 24)...)
	out = append(out, codec...)
	return append(out, bytes.Repeat([]byte{0}, 64)...)
}
