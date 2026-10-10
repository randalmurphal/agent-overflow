package attachment

import (
	"image"
	"image/color"
	"math"
)

// resample scales src to fill dst with the Catmull-Rom kernel. CatmullRom's
// slight sharpening keeps screenshot text legible where bilinear filtering
// looks soft.
//
// The output is byte for byte what draw.CatmullRom.Scale(dst, dst.Bounds(),
// src, src.Bounds(), draw.Src, nil) from golang.org/x/image/draw writes: the
// same contribution tables, the same 16-bit source values, the same
// summation order and the same rounding. The difference is memory. x/draw
// resamples every source row horizontally before the vertical pass, into
// [4]float64 scratch of destination width times SOURCE height. This keeps
// only the rows the kernel's vertical support spans, in a ring, and resamples
// each source row once when the first destination row needs it.
// resampleScratchBytes is everything it allocates.
//
// The float64 conversions around every product are x/draw's: they forbid the
// compiler from fusing a multiply and add, so the rounding, and the output,
// is the same on every architecture.
func resample[D *image.NRGBA | *image.RGBA](dst D, src image.Image) {
	var (
		pix           []uint8
		stride        int
		dr            image.Rectangle
		unpremultiply bool
	)
	switch d := any(dst).(type) {
	case *image.NRGBA:
		pix, stride, dr, unpremultiply = d.Pix, d.Stride, d.Rect, true
	case *image.RGBA:
		pix, stride, dr = d.Pix, d.Stride, d.Rect
	}
	sr := src.Bounds()
	dw, dh, sw, sh := dr.Dx(), dr.Dy(), sr.Dx(), sr.Dy()
	if dw <= 0 || dh <= 0 || sw <= 0 || sh <= 0 {
		return
	}

	horizontal, vertical := newResampleAxis(dw, sw), newResampleAxis(dh, sh)
	entries, _ := horizontal.windows(dw)
	_, ringRows := vertical.windows(dh)

	// The horizontal table serves every source row, so it is built once.
	spans := make([]resampleSpan, dw)
	table := make([]resampleContrib, 0, entries)
	for x := range spans {
		start := len(table)
		var invTotalWeight float64
		table, invTotalWeight = horizontal.contributions(x, table)
		spans[x] = resampleSpan{start: int32(start), end: int32(len(table)), invTotalWeightFFFF: invTotalWeight / 0xffff}
	}

	row := make([]color.RGBA64, sw)
	ring := make([][4]float64, ringRows*dw)
	column := make([]resampleContrib, 0, ringRows)
	offsets := make([]int, 0, ringRows)
	loaded := 0
	for dy := range dh {
		var invTotalWeight float64
		column, invTotalWeight = vertical.contributions(dy, column[:0])

		// Source rows load in order. Windows only move down, and the ring
		// holds the widest one, so the row a load overwrites is above
		// every window from this one on.
		for last := int(column[len(column)-1].coord); loaded <= last; loaded++ {
			readResampleRow(row, src, sr.Min.Y+loaded)
			out := ring[loaded%ringRows*dw:][:dw]
			for x, s := range spans {
				var pr, pg, pb, pa float64
				for _, c := range table[s.start:s.end] {
					p := row[c.coord]
					pr += float64(float64(p.R) * c.weight)
					pg += float64(float64(p.G) * c.weight)
					pb += float64(float64(p.B) * c.weight)
					pa += float64(float64(p.A) * c.weight)
				}
				out[x] = [4]float64{
					pr * s.invTotalWeightFFFF,
					pg * s.invTotalWeightFFFF,
					pb * s.invTotalWeightFFFF,
					pa * s.invTotalWeightFFFF,
				}
			}
		}

		offsets = offsets[:0]
		for _, c := range column {
			offsets = append(offsets, int(c.coord)%ringRows*dw)
		}
		d := pix[dy*stride:]
		for x := range dw {
			var pr, pg, pb, pa float64
			for k, c := range column {
				p := &ring[offsets[k]+x]
				pr += float64(p[0] * c.weight)
				pg += float64(p[1] * c.weight)
				pb += float64(p[2] * c.weight)
				pa += float64(p[3] * c.weight)
			}
			if pr > pa {
				pr = pa
			}
			if pg > pa {
				pg = pa
			}
			if pb > pa {
				pb = pa
			}
			r := uint32(ftou(pr * invTotalWeight))
			g := uint32(ftou(pg * invTotalWeight))
			b := uint32(ftou(pb * invTotalWeight))
			a := uint32(ftou(pa * invTotalWeight))
			// image.NRGBA.SetRGBA64, which x/draw writes an NRGBA
			// destination through.
			if unpremultiply && a != 0 && a != 0xffff {
				r = r * 0xffff / a
				g = g * 0xffff / a
				b = b * 0xffff / a
			}
			p := d[x*4 : x*4+4 : x*4+4]
			p[0], p[1], p[2], p[3] = uint8(r>>8), uint8(g>>8), uint8(b>>8), uint8(a>>8)
		}
	}
}

// resampleScratchBytes is what resample allocates to scale a sw x sh source
// to dw x dh: the horizontal contribution table, one source row of 16-bit
// pixels, the ring of horizontally resampled rows, and one destination row's
// vertical contributions.
func resampleScratchBytes(dw, dh, sw, sh int) int64 {
	entries, _ := newResampleAxis(dw, sw).windows(dw)
	_, ringRows := newResampleAxis(dh, sh).windows(dh)
	const (
		spanBytes    = 16 // resampleSpan
		contribBytes = 16 // resampleContrib
		rowPixel     = 8  // color.RGBA64
		ringPixel    = 32 // [4]float64
		offsetBytes  = 8  // int
	)
	return int64(dw)*spanBytes + int64(entries)*contribBytes +
		int64(sw)*rowPixel +
		int64(ringRows)*int64(dw)*ringPixel +
		int64(ringRows)*(contribBytes+offsetBytes)
}

// readResampleRow reads source row y into row as premultiplied 16-bit RGBA,
// the values x/draw's kernel scaler reads. The NRGBA and RGBA cases are
// x/draw's fast paths; every standard library image type implements
// RGBA64Image, so At is reached only by other implementations.
func readResampleRow(row []color.RGBA64, src image.Image, y int) {
	switch s := src.(type) {
	case *image.NRGBA:
		i := s.PixOffset(s.Rect.Min.X, y)
		pix := s.Pix[i : i+4*len(row)]
		for x := range row {
			p := pix[x*4 : x*4+4 : x*4+4]
			pa := uint32(p[3]) * 0x101
			row[x] = color.RGBA64{
				R: uint16(uint32(p[0]) * pa / 0xff),
				G: uint16(uint32(p[1]) * pa / 0xff),
				B: uint16(uint32(p[2]) * pa / 0xff),
				A: uint16(pa),
			}
		}
	case *image.RGBA:
		i := s.PixOffset(s.Rect.Min.X, y)
		pix := s.Pix[i : i+4*len(row)]
		for x := range row {
			p := pix[x*4 : x*4+4 : x*4+4]
			row[x] = color.RGBA64{
				R: uint16(p[0]) * 0x101,
				G: uint16(p[1]) * 0x101,
				B: uint16(p[2]) * 0x101,
				A: uint16(p[3]) * 0x101,
			}
		}
	case image.RGBA64Image:
		minX := s.Bounds().Min.X
		for x := range row {
			row[x] = s.RGBA64At(minX+x, y)
		}
	default:
		minX := s.Bounds().Min.X
		for x := range row {
			r, g, b, a := s.At(minX+x, y).RGBA()
			row[x] = color.RGBA64{R: uint16(r), G: uint16(g), B: uint16(b), A: uint16(a)}
		}
	}
}

// resampleAxis spreads src source columns or rows over destination ones, as
// x/draw's newDistrib does for its Kernel.
type resampleAxis struct {
	src                        int32
	scale, halfWidth, argScale float64
}

// resampleContrib is one source column or row's weight in a destination one.
type resampleContrib struct {
	coord  int32
	weight float64
}

// resampleSpan is one destination column's run of the horizontal table and
// its inverse total weight over 0xffff.
type resampleSpan struct {
	start, end         int32
	invTotalWeightFFFF float64
}

// catmullRomSupport is the kernel's half width at unit scale.
const catmullRomSupport = 2

func newResampleAxis(dst, src int) resampleAxis {
	scale := float64(src) / float64(dst)
	halfWidth, argScale := float64(catmullRomSupport), 1.0
	// Shrinking broadens the kernel so every source pixel is visited.
	if scale > 1 {
		halfWidth *= scale
		argScale = 1 / scale
	}
	return resampleAxis{src: int32(src), scale: scale, halfWidth: halfWidth, argScale: argScale}
}

// window is the source range [i, j) the kernel spans for destination
// position x, around the source coordinate center. Both ends are
// non-decreasing in x.
func (a resampleAxis) window(x int) (center float64, i, j int32) {
	center = float64((float64(x)+0.5)*a.scale) - 0.5
	i = int32(math.Floor(center - a.halfWidth))
	if i < 0 {
		i = 0
	}
	j = int32(math.Ceil(center + a.halfWidth))
	if j > a.src {
		j = a.src
		if j < i {
			j = i
		}
	}
	return center, i, j
}

// windows reports the total and the widest window over n destination
// positions: the horizontal table's capacity and the ring's row count.
func (a resampleAxis) windows(n int) (total, widest int) {
	for x := range n {
		_, i, j := a.window(x)
		total += int(j - i)
		widest = max(widest, int(j-i))
	}
	return total, widest
}

// contributions appends destination position x's weights to out in source
// order, skipping those outside the support or exactly zero, and returns
// them with the inverse of their total. Every position has one: the nearest
// source coordinate is at most half a pixel from the center.
func (a resampleAxis) contributions(x int, out []resampleContrib) ([]resampleContrib, float64) {
	center, i, j := a.window(x)
	total := 0.0
	for coord := i; coord < j; coord++ {
		t := (center - float64(coord)) * a.argScale
		if t < 0 {
			t = -t
		}
		if t >= catmullRomSupport {
			continue
		}
		weight := catmullRom(t)
		if weight == 0 {
			continue
		}
		total += weight
		out = append(out, resampleContrib{coord: coord, weight: weight})
	}
	return out, 1 / total
}

// catmullRom is draw.CatmullRom.At: the cubic BC-spline with B=0, C=0.5.
func catmullRom(t float64) float64 {
	if t < 1 {
		return float64((float64(1.5*t)-2.5)*t*t) + 1
	}
	return float64((float64(float64(float64(-0.5*t)+2.5)*t)-4)*t) + 2
}

// ftou is x/draw's: [0, 1] to [0, 0xffff], rounding half up and clamping.
func ftou(f float64) uint16 {
	i := int32(float64(0xffff*f) + 0.5)
	if i > 0xffff {
		return 0xffff
	}
	if i > 0 {
		return uint16(i)
	}
	return 0
}
