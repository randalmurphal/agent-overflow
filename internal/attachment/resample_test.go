package attachment

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"math"
	"math/rand/v2"
	"runtime"
	"testing"

	"golang.org/x/image/draw"
)

func randomNRGBA(rng *rand.Rand, width, height int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.IntN(256))
	}
	return img
}

// textLike is an opaque gradient crossed by one-pixel black and white strokes,
// the hard edges where Catmull-Rom overshoots and the clamps matter.
func textLike(width, height int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			c := color.NRGBA{R: uint8(x * 255 / max(width-1, 1)), G: uint8(y * 255 / max(height-1, 1)), B: 0x80, A: 0xff}
			switch {
			case x%7 == 3 || y%11 == 5:
				c = color.NRGBA{A: 0xff}
			case x%7 == 4 || y%11 == 6:
				c = color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func randomRGBA(rng *rand.Rand, width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < len(img.Pix); i += 4 {
		a := rng.IntN(256)
		img.Pix[i+0] = uint8(rng.IntN(a + 1))
		img.Pix[i+1] = uint8(rng.IntN(a + 1))
		img.Pix[i+2] = uint8(rng.IntN(a + 1))
		img.Pix[i+3] = uint8(a)
	}
	return img
}

func randomGray(rng *rand.Rand, width, height int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, width, height))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.IntN(256))
	}
	return img
}

func randomPaletted(rng *rand.Rand, width, height int) *image.Paletted {
	palette := color.Palette{
		color.NRGBA{R: 0xff, A: 0xff}, color.NRGBA{G: 0xff, A: 0x80},
		color.NRGBA{B: 0xff, A: 0x10}, color.Transparent, color.White, color.Black,
	}
	img := image.NewPaletted(image.Rect(0, 0, width, height), palette)
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.IntN(len(palette)))
	}
	return img
}

func randomYCbCr(rng *rand.Rand, width, height int) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio420)
	for _, plane := range [][]uint8{img.Y, img.Cb, img.Cr} {
		for i := range plane {
			plane[i] = uint8(rng.IntN(256))
		}
	}
	return img
}

func randomNRGBA64(rng *rand.Rand, width, height int) *image.NRGBA64 {
	img := image.NewNRGBA64(image.Rect(0, 0, width, height))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.IntN(256))
	}
	return img
}

// opaqueImage hides every method but image.Image's, so a source reaches the
// At fallback in both scalers.
type opaqueImage struct{ image.Image }

// xdrawResample is the reference resample must reproduce.
func xdrawResample(dst draw.Image, src image.Image) {
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
}

// requireMatchesXDraw resamples src into an NRGBA and an RGBA destination of
// dw x dh, each placed at a non-zero origin inside a larger buffer, and
// requires every byte of each buffer, including the margin neither scaler
// may touch, to equal x/draw's.
//
// The comparison is exact on every architecture: resample keeps x/draw's
// explicit float64 conversions around each product, which forbid fusing a
// multiply and add, so both round identically even where FMA is available.
func requireMatchesXDraw(t *testing.T, name string, src image.Image, dw, dh int) {
	t.Helper()
	outer := image.Rect(0, 0, dw+3, dh+2)
	inner := image.Rect(2, 1, 2+dw, 1+dh)

	gotNRGBA, wantNRGBA := image.NewNRGBA(outer), image.NewNRGBA(outer)
	resample(gotNRGBA.SubImage(inner).(*image.NRGBA), src)
	xdrawResample(wantNRGBA.SubImage(inner).(*image.NRGBA), src)
	requireSamePix(t, name+" into NRGBA", gotNRGBA.Pix, wantNRGBA.Pix, outer.Dx())

	gotRGBA, wantRGBA := image.NewRGBA(outer), image.NewRGBA(outer)
	resample(gotRGBA.SubImage(inner).(*image.RGBA), src)
	xdrawResample(wantRGBA.SubImage(inner).(*image.RGBA), src)
	requireSamePix(t, name+" into RGBA", gotRGBA.Pix, wantRGBA.Pix, outer.Dx())
}

func requireSamePix(t *testing.T, name string, got, want []uint8, width int) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	for i := range got {
		if got[i] != want[i] {
			pixel := i / 4
			t.Fatalf("%s: pixel (%d, %d) channel %d = %d, x/draw wrote %d", name, pixel%width, pixel/width, i%4, got[i], want[i])
		}
	}
}

func TestResampleMatchesXDraw(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	sources := []struct {
		name string
		img  image.Image
	}{
		{"random NRGBA", randomNRGBA(rng, 180, 126)},
		{"text-like NRGBA", textLike(180, 126)},
		{"RGBA", randomRGBA(rng, 180, 126)},
		{"Gray", randomGray(rng, 180, 126)},
		{"Paletted", randomPaletted(rng, 180, 126)},
		{"YCbCr 4:2:0", randomYCbCr(rng, 180, 126)},
		{"NRGBA64", randomNRGBA64(rng, 180, 126)},
		{"At only", opaqueImage{randomNRGBA(rng, 180, 126)}},
		{"NRGBA sub-image", randomNRGBA(rng, 200, 140).SubImage(image.Rect(13, 9, 193, 135))},
		{"RGBA sub-image", randomRGBA(rng, 200, 140).SubImage(image.Rect(13, 9, 193, 135))},
		{"YCbCr sub-image", randomYCbCr(rng, 200, 140).SubImage(image.Rect(13, 9, 193, 135))},
	}
	sizes := []struct {
		name   string
		dw, dh int
	}{
		{"shrink 1.8x", 100, 70},
		{"shrink 12x", 15, 11},
		{"enlarge 1.5x", 270, 189},
		{"to one pixel", 1, 1},
		{"one pixel wide", 1, 63},
		{"one pixel tall", 90, 1},
	}
	for _, src := range sources {
		for _, size := range sizes {
			requireMatchesXDraw(t, src.name+", "+size.name, src.img, size.dw, size.dh)
		}
	}

	shapes := []struct {
		name           string
		sw, sh, dw, dh int
	}{
		{"odd sizes", 37, 53, 11, 17},
		{"one pixel wide source", 1, 40, 1, 23},
		{"one pixel tall source", 40, 1, 23, 1},
		{"one pixel wide source enlarged", 1, 7, 3, 12},
		{"one pixel source enlarged", 1, 1, 5, 4},
		{"shrink across, enlarge down", 64, 9, 20, 30},
	}
	for _, shape := range shapes {
		requireMatchesXDraw(t, shape.name, randomNRGBA(rng, shape.sw, shape.sh), shape.dw, shape.dh)
	}
}

// Every pairing of small sizes in both directions, which walks the ring
// through every alignment of window and slot.
func TestResampleMatchesXDrawAcrossSizes(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for range 300 {
		sw, sh := 1+rng.IntN(48), 1+rng.IntN(48)
		dw, dh := 1+rng.IntN(48), 1+rng.IntN(48)
		requireMatchesXDraw(t, fmt.Sprintf("%dx%d to %dx%d", sw, sh, dw, dh), randomNRGBA(rng, sw, sh), dw, dh)
	}
}

func TestCatmullRomIsXDrawsKernel(t *testing.T) {
	if catmullRomSupport != draw.CatmullRom.Support {
		t.Fatalf("support %v, x/draw's is %v", float64(catmullRomSupport), draw.CatmullRom.Support)
	}
	rng := rand.New(rand.NewPCG(5, 6))
	for i := range 1 << 16 {
		ts := []float64{float64(i) / (1 << 15), rng.Float64() * catmullRomSupport}
		for _, t0 := range ts {
			if got, want := catmullRom(t0), draw.CatmullRom.At(t0); math.Float64bits(got) != math.Float64bits(want) {
				t.Fatalf("catmullRom(%v) = %v, x/draw's is %v", t0, got, want)
			}
		}
	}
}

// The ring holds the rows one destination row's kernel spans, not the
// source's height of them: a 2000x2000 source to 500x500 allocates under
// half a megabyte where x/draw's scratch alone is 32 MB. resampleScratchBytes,
// which the decode budget charges, covers what was allocated.
func TestResampleScratchIsBoundedByTheKernelSupport(t *testing.T) {
	const sw, sh, dw, dh = 2000, 2000, 500, 500
	src := textLike(sw, sh)
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	_, ringRows := newResampleAxis(dh, sh).windows(dh)
	bound := uint64(ringRows*dw*32 + sw*8 + 256<<10)
	estimate := uint64(resampleScratchBytes(dw, dh, sw, sh))

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	resample(dst, src)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	if allocated > bound {
		t.Fatalf("resample allocated %d bytes, over the %d-byte bound for %d ring rows", allocated, bound, ringRows)
	}
	// The allocator rounds each of resample's six buffers up to its size
	// class or page.
	const rounding = 6 * 8 << 10
	if allocated > estimate+rounding {
		t.Fatalf("resample allocated %d bytes, over its %d-byte estimate", allocated, estimate)
	}
	t.Logf("allocated %d bytes, estimate %d, %d ring rows", allocated, estimate, ringRows)
}

// A screenshot to the 2160 tier, through this resampler and through x/draw.
func BenchmarkResample(b *testing.B) {
	src := textLike(3840, 2160)
	for _, scaler := range []struct {
		name string
		run  func(dst *image.NRGBA)
	}{
		{"stream", func(dst *image.NRGBA) { resample(dst, src) }},
		{"xdraw", func(dst *image.NRGBA) { xdrawResample(dst, src) }},
	} {
		b.Run(scaler.name, func(b *testing.B) {
			dst := image.NewNRGBA(image.Rect(0, 0, 2160, 1215))
			b.ReportAllocs()
			for b.Loop() {
				scaler.run(dst)
			}
		})
	}
}
