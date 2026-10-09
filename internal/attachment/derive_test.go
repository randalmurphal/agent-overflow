package attachment

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
	"golang.org/x/sync/semaphore"
)

// The three WebP fixtures are copied from golang.org/x/image/testdata
// (BSD-3-Clause, The Go Authors): tux.lossless.webp (386x395, VP8L),
// yellow_rose.lossy.webp (400x301, VP8) and
// yellow_rose.lossy-with-alpha.webp (400x301, VP8X with ALPH).
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func gradient(width, height int, translucent bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			alpha := uint8(0xff)
			if translucent {
				alpha = uint8((x + y) % 256)
			}
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: alpha})
		}
	}
	return img
}

func encodeAs(t *testing.T, format string, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
	case "gif":
		err = gif.Encode(&buf, img, nil)
	case "bmp":
		err = bmp.Encode(&buf, img)
	case "tiff":
		err = tiff.Encode(&buf, img, nil)
	default:
		t.Fatalf("no encoder for %s", format)
	}
	if err != nil {
		t.Fatalf("encode %s: %v", format, err)
	}
	return buf.Bytes()
}

func animatedGIF(t *testing.T, width, height, frames int) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White}
	anim := &gif.GIF{}
	for i := range frames {
		frame := image.NewPaletted(image.Rect(0, 0, width, height), palette)
		frame.SetColorIndex(i%width, 0, 1)
		anim.Image = append(anim.Image, frame)
		anim.Delay = append(anim.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		t.Fatalf("encode animated gif: %v", err)
	}
	return buf.Bytes()
}

// jpegQuantTables returns the payload of every DQT segment, which depends on
// the encoder's quality setting and nothing else.
func jpegQuantTables(t *testing.T, data []byte) []byte {
	t.Helper()
	var tables []byte
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			t.Fatalf("no JPEG marker at byte %d", i)
		}
		marker, length := data[i+1], int(data[i+2])<<8|int(data[i+3])
		if marker == 0xDB {
			tables = append(tables, data[i+4:i+2+length]...)
		}
		if marker == 0xDA {
			break
		}
		i += 2 + length
	}
	if len(tables) == 0 {
		t.Fatal("JPEG carries no quantization tables")
	}
	return tables
}

func requireJPEGQuality(t *testing.T, data []byte, quality int) {
	t.Helper()
	var reference bytes.Buffer
	if err := jpeg.Encode(&reference, gradient(8, 8, false), &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("encode reference: %v", err)
	}
	if !bytes.Equal(jpegQuantTables(t, data), jpegQuantTables(t, reference.Bytes())) {
		t.Fatalf("JPEG was not encoded at quality %d", quality)
	}
}

// The frontend mirrors this list, so a change here is a wire change.
func TestDeriveWidthsArePinned(t *testing.T) {
	want := []int{320, 480, 720, 1080, 1440, 2160, 2880, 3840, 5120}
	if !reflect.DeepEqual(DeriveWidths, want) {
		t.Fatalf("DeriveWidths = %v, want %v", DeriveWidths, want)
	}
}

func TestDeriveTierRoundsUpToTheLadder(t *testing.T) {
	cases := map[int]int{
		-1: 0, 0: 0, // the original
		1: 320, 320: 320, 321: 480, 1000: 1080, 1081: 1440, 5119: 5120, 5120: 5120,
		5121: 0, // above the ladder: the original
	}
	for maxWidth, want := range cases {
		if got := DeriveTier(maxWidth); got != want {
			t.Errorf("DeriveTier(%d) = %d, want %d", maxWidth, got, want)
		}
	}
}

func TestDeriveOutputPerSourceFamily(t *testing.T) {
	cases := []struct {
		name       string
		src        []byte
		mime       string
		wantMIME   string
		wantHeight int
		quality    int // JPEG output only
	}{
		{"png", encodeAs(t, "png", gradient(641, 480, false)), "image/png", "image/png", 240, 0},
		{"png with alpha", encodeAs(t, "png", gradient(641, 480, true)), "image/png", "image/png", 240, 0},
		{"jpeg", encodeAs(t, "jpeg", gradient(641, 480, false)), "image/jpeg", "image/jpeg", 240, 90},
		{"static gif", encodeAs(t, "gif", gradient(641, 480, false)), "image/gif", "image/png", 240, 0},
		{"bmp", encodeAs(t, "bmp", gradient(641, 480, false)), "image/bmp", "image/png", 240, 0},
		{"tiff", encodeAs(t, "tiff", gradient(641, 480, false)), "image/tiff", "image/png", 240, 0},
		// 395*320/386 = 327.5 rounds to 327; 301*320/400 = 240.8 to 241.
		{"lossless webp", fixture(t, "tux.lossless.webp"), "image/webp", "image/png", 327, 0},
		{"lossy webp", fixture(t, "yellow_rose.lossy.webp"), "image/webp", "image/jpeg", 241, 90},
		{"lossy webp with alpha", fixture(t, "yellow_rose.lossy-with-alpha.webp"), "image/webp", "image/png", 241, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Derive("family/"+tc.name, tc.src, tc.mime, 300)
			if err != nil {
				t.Fatalf("Derive: %v", err)
			}
			if !got.Derived || got.MimeType != tc.wantMIME || got.Width != 320 || got.Height != tc.wantHeight {
				t.Fatalf("Derive = (derived %v, %s, %dx%d), want (derived, %s, 320x%d)",
					got.Derived, got.MimeType, got.Width, got.Height, tc.wantMIME, tc.wantHeight)
			}
			decoded, format, err := image.Decode(bytes.NewReader(got.Data))
			if err != nil {
				t.Fatalf("decode derivative: %v", err)
			}
			if "image/"+format != tc.wantMIME || decoded.Bounds().Dx() != 320 || decoded.Bounds().Dy() != tc.wantHeight {
				t.Fatalf("derivative decodes as %s %v, want %s 320x%d", format, decoded.Bounds(), tc.wantMIME, tc.wantHeight)
			}
			if tc.quality != 0 {
				requireJPEGQuality(t, got.Data, tc.quality)
			}
		})
	}
}

// Unpremultiplied output keeps a translucent pixel translucent rather than
// flattening it.
func TestDeriveKeepsAlpha(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 640, 2))
	for x := range 640 {
		for y := range 2 {
			src.SetNRGBA(x, y, color.NRGBA{R: 200, G: 40, B: 10, A: 30})
		}
	}
	got, err := Derive("alpha", encodeAs(t, "png", src), "image/png", 320)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	pixel := color.NRGBAModel.Convert(decoded.At(100, 0)).(color.NRGBA)
	if pixel != (color.NRGBA{R: 200, G: 40, B: 10, A: 30}) {
		t.Fatalf("pixel = %+v, want the source's unpremultiplied color and alpha", pixel)
	}
}

func TestDeriveServesTheOriginal(t *testing.T) {
	png641 := encodeAs(t, "png", gradient(641, 480, false))
	png400 := encodeAs(t, "png", gradient(400, 300, false))
	cases := []struct {
		name          string
		src           []byte
		mime          string
		maxWidth      int
		width, height int
	}{
		{"no width asked", png641, "image/png", 0, 641, 480},
		{"wider than the ladder", png641, "image/png", 5121, 641, 480},
		{"no wider than the tier", png400, "image/png", 401, 400, 300},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="900" height="9"/>`), "image/svg+xml", 320, 0, 0},
		{"ico", []byte("\x00\x00\x01\x00not decoded"), "image/x-icon", 320, 0, 0},
		{"avif", []byte("\x00\x00\x00\x1cftypavifnot decoded"), "image/avif", 320, 0, 0},
		{"animated gif", animatedGIF(t, 641, 480, 3), "image/gif", 320, 641, 480},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Derive("original/"+tc.name, tc.src, tc.mime, tc.maxWidth)
			if err != nil {
				t.Fatalf("Derive: %v", err)
			}
			if got.Derived || &got.Data[0] != &tc.src[0] || len(got.Data) != len(tc.src) || got.MimeType != tc.mime {
				t.Fatalf("Derive = (derived %v, %s, %d bytes), want the source itself", got.Derived, got.MimeType, len(got.Data))
			}
			if got.Width != tc.width || got.Height != tc.height {
				t.Fatalf("size = %dx%d, want %dx%d", got.Width, got.Height, tc.width, tc.height)
			}
		})
	}
}

func TestDeriveRefusesAPixelBombBeforeDecoding(t *testing.T) {
	var acquired atomic.Int32
	replaceDecodeAcquired(t, func(int64) { acquired.Add(1) })
	bomb := pngWithDeclaredDimensions(t, 100_000, 100_000)
	if _, err := Derive("bomb", bomb, "image/png", 320); !errors.Is(err, ErrPixelBudget) {
		t.Fatalf("Derive(bomb) = %v, want ErrPixelBudget", err)
	}
	if acquired.Load() != 0 {
		t.Fatal("the bomb reached the decode")
	}
}

func TestDeriveRoundsTheHeight(t *testing.T) {
	cases := []struct{ width, height, to, want int }{
		{1000, 333, 320, 107}, // 106.56: truncation would answer 106
		{1000, 331, 320, 106}, // 105.92
		{1000, 1, 320, 1},     // 0.32: never zero
		{641, 480, 320, 240},  // 239.6
	}
	for _, tc := range cases {
		if got := scaledHeight(tc.width, tc.height, tc.to); got != tc.want {
			t.Errorf("scaledHeight(%d, %d, %d) = %d, want %d", tc.width, tc.height, tc.to, got, tc.want)
		}
	}
	got, err := Derive("rounding", encodeAs(t, "png", gradient(1000, 333, false)), "image/png", 320)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if got.Width != 320 || got.Height != 107 {
		t.Fatalf("Derive size = %dx%d, want 320x107", got.Width, got.Height)
	}
}

func TestGIFAnimationIsCountedWithoutDecoding(t *testing.T) {
	static := encodeAs(t, "gif", gradient(40, 20, false))
	animated := animatedGIF(t, 40, 20, 2)
	cases := map[string]struct {
		src  []byte
		want bool
	}{
		"static":            {static, false},
		"two frames":        {animated, true},
		"truncated":         {animated[:len(animated)/2], false},
		"header only":       {static[:13], false},
		"not a gif":         {[]byte("hello"), false},
		"trailer after one": {append(append([]byte{}, static[:len(static)-1]...), 0x3B), false},
		"garbage after one": {append(append([]byte{}, static[:len(static)-1]...), 0x99, 0x2C), false},
	}
	for name, tc := range cases {
		if got := gifIsAnimated(tc.src); got != tc.want {
			t.Errorf("%s: gifIsAnimated = %v, want %v", name, got, tc.want)
		}
	}
}

// replaceDecodeAcquired installs a hook for the window between acquiring the
// decode budget and decoding. Tests using it stay serial: the hook is
// package state.
func replaceDecodeAcquired(t *testing.T, hook func(cost int64)) {
	t.Helper()
	previous := decodeAcquired
	decodeAcquired = hook
	t.Cleanup(func() { decodeAcquired = previous })
}

func replaceDecodeBudget(t *testing.T, limit int64) {
	t.Helper()
	previousSem, previousLimit := decodeMemory, decodeMemoryLimit
	decodeMemory, decodeMemoryLimit = semaphore.NewWeighted(limit), limit
	t.Cleanup(func() { decodeMemory, decodeMemoryLimit = previousSem, previousLimit })
}

// Two panes asking for the same tier of the same image decode it once.
func TestDeriveDedupesConcurrentCallsForOneTier(t *testing.T) {
	src := encodeAs(t, "png", gradient(641, 480, false))
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		var decodes atomic.Int32
		replaceDecodeAcquired(t, func(int64) {
			decodes.Add(1)
			<-gate
		})
		const callers = 4
		results := make([]Derived, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() {
				var err error
				if results[i], err = Derive("same-file", src, "image/png", 320); err != nil {
					t.Errorf("Derive: %v", err)
				}
			})
		}
		// Every caller is now either holding the decode open or waiting on
		// its flight.
		synctest.Wait()
		close(gate)
		wg.Wait()
		if got := decodes.Load(); got != 1 {
			t.Fatalf("%d decodes for one tier, want 1", got)
		}
		for i := 1; i < callers; i++ {
			if &results[i].Data[0] != &results[0].Data[0] {
				t.Fatalf("caller %d got its own derivative", i)
			}
		}
		// Another tier of the same file is its own derivation.
		if _, err := Derive("same-file", src, "image/png", 480); err != nil {
			t.Fatalf("Derive: %v", err)
		}
		if got := decodes.Load(); got != 2 {
			t.Fatalf("%d decodes after a second tier, want 2", got)
		}
	})
}

// Each job holds its estimated cost, not a slot, while it decodes.
func TestDeriveHoldsItsEstimatedMemory(t *testing.T) {
	src := encodeAs(t, "png", gradient(641, 480, false))
	cfg, err := imageConfig(src)
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	var held int64
	replaceDecodeAcquired(t, func(int64) { held = decodeMemoryHeld.Load() })
	if _, err := Derive("estimate", src, "image/png", 320); err != nil {
		t.Fatalf("Derive: %v", err)
	}
	// 641x480 decoded at 4 B/px, a 320x480 float scratch, and the 320x240
	// destination plus its encoding.
	want := int64(641*480*4 + 320*480*32 + 2*320*240*4)
	if resampleCost(cfg, 320, 240) != want || held != want {
		t.Fatalf("held %d bytes while decoding, want %d", held, want)
	}
	if got := decodeMemoryHeld.Load(); got != 0 {
		t.Fatalf("%d bytes still held after the job", got)
	}
}

// A second job that does not fit beside the first waits for it.
func TestDecodeMemoryBoundsConcurrentJobs(t *testing.T) {
	src := encodeAs(t, "png", gradient(641, 480, false))
	cfg, err := imageConfig(src)
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	cost := resampleCost(cfg, 320, 240)
	synctest.Test(t, func(t *testing.T) {
		replaceDecodeBudget(t, cost+cost/2)
		gate := make(chan struct{})
		var inside atomic.Int32
		replaceDecodeAcquired(t, func(int64) {
			inside.Add(1)
			<-gate
		})
		var wg sync.WaitGroup
		for _, key := range []string{"first", "second"} {
			wg.Go(func() {
				if _, err := Derive(key, src, "image/png", 320); err != nil {
					t.Errorf("Derive: %v", err)
				}
			})
		}
		synctest.Wait()
		if got := inside.Load(); got != 1 {
			t.Fatalf("%d jobs decoding inside a budget that fits one", got)
		}
		if held := decodeMemoryHeld.Load(); held != cost {
			t.Fatalf("held %d, want %d", held, cost)
		}
		gate <- struct{}{}
		synctest.Wait()
		if got := inside.Load(); got != 2 {
			t.Fatalf("the waiting job did not start after the first released: %d", got)
		}
		close(gate)
		wg.Wait()
	})
}

// A job costlier than the whole budget takes all of it and runs alone,
// rather than waiting forever for room that can never exist.
func TestDecodeMemoryAdmitsAJobLargerThanTheBudget(t *testing.T) {
	src := encodeAs(t, "png", gradient(641, 480, false))
	synctest.Test(t, func(t *testing.T) {
		replaceDecodeBudget(t, 1000)
		var held int64
		replaceDecodeAcquired(t, func(int64) { held = decodeMemoryHeld.Load() })
		if _, err := Derive("too-big", src, "image/png", 320); err != nil {
			t.Fatalf("Derive: %v", err)
		}
		if held != 1000 {
			t.Fatalf("held %d, want the whole 1000-byte budget", held)
		}
	})
}
