package attachment

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"testing"
)

// The AVIF fixtures are solid-color images this repository made with
// sharp (libavif): solid-8x3.avif, solid-5x7.avif and solid-64x1.avif,
// their names their pixel sizes. The Chromium test
// imageDisplaySize.browser.test.ts (frontend/src/lib/utils) asserts the same
// sizes as the browser decodes them.
var avifFixtures = []struct {
	name          string
	width, height int
}{
	{"solid-8x3.avif", 8, 3},
	{"solid-5x7.avif", 5, 7},
	{"solid-64x1.avif", 64, 1},
}

// readCounter counts the bytes its reader yields.
type readCounter struct {
	r io.Reader
	n int64
}

func (c *readCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// metaEnd is where a file's meta box ends: AVIFSize needs no byte past it.
func metaEnd(t *testing.T, data []byte) int64 {
	t.Helper()
	for at := int64(0); at+8 <= int64(len(data)); {
		size := int64(binary.BigEndian.Uint32(data[at:]))
		if string(data[at+4:at+8]) == "meta" {
			return at + size
		}
		if size < 8 {
			t.Fatal("fixture box with no size")
		}
		at += size
	}
	t.Fatal("fixture has no meta box")
	return 0
}

func TestAVIFSizeOfRealFiles(t *testing.T) {
	for _, f := range avifFixtures {
		data := fixture(t, f.name)
		// Behind a megabyte of picture data, it still reads only to meta's end.
		padded := append(append([]byte(nil), data...), make([]byte, 1<<20)...)
		counted := &readCounter{r: bytes.NewReader(padded)}
		got := avifSizeOrUnknown(t, counted)
		if got != (DisplaySize{Width: f.width, Height: f.height}) {
			t.Errorf("%s: AVIFSize = %+v, want %dx%d", f.name, got, f.width, f.height)
		}
		if end := metaEnd(t, data); counted.n != end {
			t.Errorf("%s: read %d bytes, want %d (the end of meta)", f.name, counted.n, end)
		}
		if mime, err := DetectDisplayImageMIME(data); err != nil || mime != "image/avif" {
			t.Errorf("%s: DetectDisplayImageMIME = %q, %v", f.name, mime, err)
		}
	}
}

// Cut at every byte, a real file is unknown until its meta box is whole,
// then exact, and nothing past the cut is read or panics.
func TestAVIFSizeTruncatedAtEveryByte(t *testing.T) {
	for _, f := range avifFixtures {
		data := fixture(t, f.name)
		end := metaEnd(t, data)
		for cut := 0; cut <= len(data); cut++ {
			counted := &readCounter{r: bytes.NewReader(data[:cut])}
			got := avifSizeOrUnknown(t, counted)
			want := DisplaySize{}
			if int64(cut) >= end {
				want = DisplaySize{Width: f.width, Height: f.height}
			}
			if got != want {
				t.Errorf("%s cut at %d: AVIFSize = %+v, want %+v", f.name, cut, got, want)
			}
			if counted.n > int64(cut) {
				t.Errorf("%s cut at %d: read %d bytes", f.name, cut, counted.n)
			}
		}
	}
}

// bytesPerCall is the heap bytes one call of f allocates, averaged over
// many calls so the measurement's own cost vanishes.
func bytesPerCall(f func()) uint64 {
	const runs = 200
	f()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / runs
}

// Box builders for structures an encoder does not write.

func avifBox(kind string, body ...[]byte) []byte {
	payload := bytes.Join(body, nil)
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(payload)))
	return append(append(out, kind...), payload...)
}

// avifLargeBox is a box whose size is a 64-bit largesize.
func avifLargeBox(kind string, body ...[]byte) []byte {
	payload := bytes.Join(body, nil)
	out := binary.BigEndian.AppendUint32(nil, 1)
	out = append(out, kind...)
	out = binary.BigEndian.AppendUint64(out, uint64(16+len(payload)))
	return append(out, payload...)
}

// avifToEndBox is a box of size 0, which runs to the end of its container.
func avifToEndBox(kind string, body ...[]byte) []byte {
	return append(append([]byte{0, 0, 0, 0}, kind...), bytes.Join(body, nil)...)
}

func avifFullBox(kind string, version byte, flags uint32, body ...[]byte) []byte {
	head := binary.BigEndian.AppendUint32(nil, uint32(version)<<24|flags&0xffffff)
	return avifBox(kind, append([][]byte{head}, body...)...)
}

func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func avifIspe(width, height uint32) []byte {
	return avifFullBox("ispe", 0, 0, u32(width), u32(height))
}
func avifIrot(angle byte) []byte { return avifBox("irot", []byte{angle}) }
func avifClap() []byte           { return avifBox("clap", make([]byte, 32)) }

// avifEntry is one ipma entry: an item and its property indexes.
type avifEntry struct {
	item    uint32
	indexes []uint16
}

// avifIpma is an ipma of version (16 or 32-bit item ids) and flags (bit 1:
// 15-bit indexes).
func avifIpma(version byte, flags uint32, entries ...avifEntry) []byte {
	body := u32(uint32(len(entries)))
	for _, e := range entries {
		if version == 0 {
			body = append(body, u16(uint16(e.item))...)
		} else {
			body = append(body, u32(e.item)...)
		}
		body = append(body, byte(len(e.indexes)))
		for _, index := range e.indexes {
			if flags&1 == 1 {
				body = append(body, u16(0x8000|index)...)
			} else {
				body = append(body, byte(0x80|index))
			}
		}
	}
	return avifFullBox("ipma", version, flags, body)
}

func avifIprp(properties [][]byte, ipmas ...[]byte) []byte {
	return avifBox("iprp", append([][]byte{avifBox("ipco", properties...)}, ipmas...)...)
}

// avifItemProperties associates item with every one of properties.
func avifItemProperties(item uint16, properties ...[]byte) []byte {
	indexes := make([]uint16, len(properties))
	for i := range indexes {
		indexes[i] = uint16(i + 1)
	}
	return avifIprp(properties, avifIpma(0, 0, avifEntry{uint32(item), indexes}))
}

// frees is n empty free boxes, properties that take an index.
func frees(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = avifBox("free")
	}
	return out
}

func avifPitm(item uint16) []byte { return avifFullBox("pitm", 0, 0, u16(item)) }

func avifHdlr() []byte {
	return avifFullBox("hdlr", 0, 0, make([]byte, 4), []byte("pict"), make([]byte, 13))
}

// avifMetaBox is a meta box naming primary as the primary item.
func avifMetaBox(primary uint16, children ...[]byte) []byte {
	return avifFullBox("meta", 0, 0, append([][]byte{avifHdlr(), avifPitm(primary)}, children...)...)
}

func avifFtyp(brand string) []byte {
	return avifBox("ftyp", []byte(brand), make([]byte, 4), []byte("mif1miaf"))
}

// avifFile is an ftyp, the boxes given and a little picture data.
func avifFile(boxes ...[]byte) []byte {
	out := avifFtyp("avif")
	for _, b := range boxes {
		out = append(out, b...)
	}
	return append(out, avifBox("mdat", make([]byte, 16))...)
}

func TestAVIFSizeOfBuiltStructures(t *testing.T) {
	size := func(w, h int) DisplaySize { return DisplaySize{Width: w, Height: h} }
	unknown := DisplaySize{}
	meta := func(children ...[]byte) []byte { return avifFile(avifMetaBox(1, children...)) }
	ispe := avifIspe(40, 10)
	for _, tc := range []struct {
		name string
		data []byte
		want DisplaySize
	}{
		{"ispe", meta(avifItemProperties(1, ispe)), size(40, 10)},
		{"irot 90", meta(avifItemProperties(1, ispe, avifIrot(1))), size(10, 40)},
		{"irot 180", meta(avifItemProperties(1, ispe, avifIrot(2))), size(40, 10)},
		{"irot 270, reserved bits set", meta(avifItemProperties(1, ispe, avifIrot(0xfc|3))), size(10, 40)},
		{"clap", meta(avifItemProperties(1, ispe, avifClap())), unknown},
		{"a grid's own ispe, not its tiles'", meta(avifIprp(
			[][]byte{avifIspe(100, 50), avifIspe(50, 50)},
			avifIpma(0, 0, avifEntry{1, []uint16{1}}, avifEntry{2, []uint16{2}}, avifEntry{3, []uint16{2}}),
		)), size(100, 50)},
		{"of two ispe, the primary item's", meta(avifIprp(
			[][]byte{avifIspe(10, 20), avifIspe(30, 40)},
			avifIpma(0, 0, avifEntry{2, []uint16{1}}, avifEntry{1, []uint16{2}}),
		)), size(30, 40)},
		{"a tile's irot is not the primary item's", meta(avifIprp(
			[][]byte{ispe, avifIrot(1)},
			avifIpma(0, 0, avifEntry{1, []uint16{1}}, avifEntry{2, []uint16{1, 2}}),
		)), size(40, 10)},
		{"index 0 is no property", meta(avifIprp([][]byte{ispe}, avifIpma(0, 0, avifEntry{1, []uint16{0, 1}}))), size(40, 10)},
		{"ipma version 1 and pitm version 1", avifFile(avifFullBox("meta", 0, 0, avifHdlr(), avifFullBox("pitm", 1, 0, u32(70000)),
			avifIprp([][]byte{ispe}, avifIpma(1, 0, avifEntry{70000, []uint16{1}})))), size(40, 10)},
		{"15-bit indexes", meta(avifIprp(append(frees(199), ispe), avifIpma(0, 1, avifEntry{1, []uint16{200}}))), size(40, 10)},
		{"meta with a largesize", avifFile(avifLargeBox("meta", u32(0), avifHdlr(), avifPitm(1), avifItemProperties(1, ispe))), size(40, 10)},
		{"a property with a largesize", meta(avifIprp([][]byte{avifLargeBox("free"), avifLargeBox("ispe", u32(0), u32(40), u32(10))}, avifIpma(0, 0, avifEntry{1, []uint16{2}}))), size(40, 10)},
		{"meta to the end of the file", append(avifFtyp("avif"), avifToEndBox("meta", u32(0), avifHdlr(), avifPitm(1), avifItemProperties(1, ispe))...), size(40, 10)},
		{"a last property to the end of ipco", meta(avifBox("iprp", avifBox("ipco", avifBox("free"), avifToEndBox("ispe", u32(0), u32(40), u32(10))), avifIpma(0, 0, avifEntry{1, []uint16{2}}))), size(40, 10)},
		{"picture data before meta", append(append(avifFtyp("avif"), avifBox("mdat", make([]byte, 4096))...), avifMetaBox(1, avifItemProperties(1, ispe))...), size(40, 10)},
		{"an image sequence", append(avifFtyp("avis"), avifMetaBox(1, avifItemProperties(1, ispe))...), unknown},
		{"no pitm", avifFile(avifFullBox("meta", 0, 0, avifHdlr(), avifItemProperties(1, ispe))), unknown},
		{"two pitm", avifFile(avifFullBox("meta", 0, 0, avifHdlr(), avifPitm(1), avifPitm(1), avifItemProperties(1, ispe))), unknown},
		{"pitm version 2", avifFile(avifFullBox("meta", 0, 0, avifHdlr(), avifFullBox("pitm", 2, 0, u32(1)), avifItemProperties(1, ispe))), unknown},
		{"ipma version 2", meta(avifIprp([][]byte{ispe}, avifFullBox("ipma", 2, 0, u32(1), u16(1), []byte{1, 0x81}))), unknown},
		{"no iprp", meta(), unknown},
		{"no association", meta(avifIprp([][]byte{ispe}, avifIpma(0, 0, avifEntry{2, []uint16{1}}))), unknown},
		{"the item listed twice", meta(avifIprp([][]byte{ispe}, avifIpma(0, 0, avifEntry{1, []uint16{1}}), avifIpma(0, 0, avifEntry{1, []uint16{1}}))), unknown},
		{"no ispe", meta(avifItemProperties(1, avifIrot(1))), unknown},
		{"two ispe on the item", meta(avifItemProperties(1, ispe, avifIspe(40, 10))), unknown},
		{"an index past ipco", meta(avifIprp([][]byte{ispe}, avifIpma(0, 0, avifEntry{1, []uint16{1, 2}}))), unknown},
		{"zero width", meta(avifItemProperties(1, avifIspe(0, 10))), unknown},
		{"a short ispe", meta(avifItemProperties(1, avifFullBox("ispe", 0, 0, u32(40)))), unknown},
		{"too many boxes before meta", append(append(avifFtyp("avif"), bytes.Repeat(avifBox("free"), avifTopBoxesMax)...), avifMetaBox(1, avifItemProperties(1, ispe))...), unknown},
		{"a box to the end before meta", append(avifFtyp("avif"), avifToEndBox("free")...), unknown},
		{"empty", nil, unknown},
		// A size past the budget it cannot read stays unknown.
		{"over the pixel budget with a clap", meta(avifItemProperties(1, avifIspe(60000, 60000), avifClap())), unknown},
		{"over the pixel budget in a sequence", append(avifFtyp("avis"), avifMetaBox(1, avifItemProperties(1, avifIspe(60000, 60000)))...), unknown},
	} {
		if got := avifSizeOrUnknown(t, bytes.NewReader(tc.data)); got != tc.want {
			t.Errorf("%s: AVIFSize = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	// A size it reads past the budget is refused as an oversized raster
	// header is: the product, or one side past what the product allows.
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"over the pixel budget", meta(avifItemProperties(1, avifIspe(60000, 60000)))},
		{"over the pixel budget rotated", meta(avifItemProperties(1, avifIspe(60000, 60000), avifIrot(1)))},
		{"one side past the budget", meta(avifItemProperties(1, avifIspe(1, thumbPixelBudget+1)))},
		{"both sides at 2^32-1, past int64 multiplied", meta(avifItemProperties(1, avifIspe(0xffffffff, 0xffffffff)))},
	} {
		if got, err := AVIFSize(bytes.NewReader(tc.data)); !errors.Is(err, ErrPixelBudget) || got != unknown {
			t.Errorf("%s: AVIFSize = %+v, %v; want ErrPixelBudget", tc.name, got, err)
		}
	}
}

// avifSizeOrUnknown is AVIFSize where no error is expected.
func avifSizeOrUnknown(t *testing.T, r io.Reader) DisplaySize {
	t.Helper()
	size, err := AVIFSize(r)
	if err != nil {
		t.Errorf("AVIFSize: %v", err)
	}
	return size
}

// Sizes and counts declared far past the data are refused before they
// drive a read, a loop or an allocation: each answers unknown having read
// no more than the data and allocated far less than it declares.
func TestAVIFSizeRefusesDeclaredSizesPastTheData(t *testing.T) {
	ispe := avifIspe(40, 10)
	declared := func(kind string, size uint32, body ...[]byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, size)
		return append(append(out, kind...), bytes.Join(body, nil)...)
	}
	largeDeclared := func(kind string, size uint64) []byte {
		out := append(binary.BigEndian.AppendUint32(nil, 1), kind...)
		return binary.BigEndian.AppendUint64(out, size)
	}
	// Each over the cap by far, and each parses whole: only the cap
	// refuses it. The to-end meta's last child runs to the end too, so a
	// read cut at the cap still parses.
	overCap := avifFullBox("meta", 0, 0, avifHdlr(), avifPitm(1), avifItemProperties(1, ispe), avifBox("free", make([]byte, 4*avifMetaMax)))
	overCapToEnd := avifToEndBox("meta", u32(0), avifHdlr(), avifPitm(1), avifItemProperties(1, ispe), avifToEndBox("free", make([]byte, 4*avifMetaMax)))
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"meta of 4 GiB", append(avifFtyp("avif"), declared("meta", 0xfffffff0, u32(0), avifHdlr())...)},
		{"meta of 2^62 bytes", append(avifFtyp("avif"), largeDeclared("meta", 1<<62)...)},
		{"a largesize past int64", append(avifFtyp("avif"), largeDeclared("meta", 1<<63+8)...)},
		{"a largesize under its header", append(avifFtyp("avif"), largeDeclared("free", 8)...)},
		{"a size under its header", append(avifFtyp("avif"), declared("free", 4)...)},
		{"a 1 TiB box before meta", append(avifFtyp("avif"), largeDeclared("mdat", 1<<40)...)},
		{"ftyp of 4 GiB", declared("ftyp", 0xfffffff0, []byte("avif"))},
		{"meta over the cap", append(avifFtyp("avif"), overCap...)},
		{"meta to the end over the cap", append(avifFtyp("avif"), overCapToEnd...)},
		{"a child past meta", avifFile(avifFullBox("meta", 0, 0, avifHdlr(), declared("iprp", 0xfffffff0)))},
		{"a property past ipco", avifFile(avifMetaBox(1, avifBox("iprp", avifBox("ipco", declared("ispe", 0x7fffffff)), avifIpma(0, 0, avifEntry{1, []uint16{1}}))))},
		{"4 billion ipma entries", avifFile(avifMetaBox(1, avifIprp([][]byte{ispe}, avifFullBox("ipma", 0, 0, u32(0xffffffff), u16(1), []byte{1, 0x81}))))},
		{"255 associations in 2 bytes", avifFile(avifMetaBox(1, avifIprp([][]byte{ispe}, avifFullBox("ipma", 0, 0, u32(1), u16(1), []byte{255, 0x81, 0x81}))))},
	} {
		counted := &readCounter{r: bytes.NewReader(tc.data)}
		if got := avifSizeOrUnknown(t, counted); got != (DisplaySize{}) {
			t.Errorf("%s: AVIFSize = %+v, want unknown", tc.name, got)
		}
		// Never past the data, nor past ftyp, a header and the cap and a byte.
		if bound := min(int64(len(tc.data)), int64(len(avifFtyp("avif"))+16+avifMetaMax+1)); counted.n > bound {
			t.Errorf("%s: read %d of %d bytes, want at most %d", tc.name, counted.n, len(tc.data), bound)
		}
		// The over-cap cases buffer at most the cap and a byte, in the
		// doublings a growing read makes; the rest a few KiB at most.
		limit := uint64(4 << 10)
		if len(tc.data) > avifMetaMax {
			limit = 3 * avifMetaMax
		}
		if allocated := bytesPerCall(func() { AVIFSize(bytes.NewReader(tc.data)) }); allocated > limit {
			t.Errorf("%s: allocated %d bytes a call, want at most %d", tc.name, allocated, limit)
		}
	}
}

// The reader's cost is the bytes to the end of meta, whatever follows,
// and a meta at the cap still answers.
func TestAVIFSizeWorkload(t *testing.T) {
	ispe := avifIspe(40, 10)
	// A meta box exactly at the cap: its payload is avifMetaMax bytes.
	inner := [][]byte{avifHdlr(), avifPitm(1), avifItemProperties(1, ispe)}
	used := 4
	for _, b := range inner {
		used += len(b)
	}
	atCap := avifFullBox("meta", 0, 0, append(inner, avifBox("free", make([]byte, avifMetaMax-used-8)))...)
	if got := len(atCap) - 8; got != avifMetaMax {
		t.Fatalf("built a meta payload of %d bytes", got)
	}
	data := append(append(avifFtyp("avif"), atCap...), avifBox("mdat", make([]byte, 4<<20))...)
	counted := &readCounter{r: bytes.NewReader(data)}
	if got := avifSizeOrUnknown(t, counted); got != (DisplaySize{Width: 40, Height: 10}) {
		t.Fatalf("meta at the cap: AVIFSize = %+v, want 40x10", got)
	}
	if want := int64(len(avifFtyp("avif")) + len(atCap)); counted.n != want {
		t.Fatalf("read %d bytes, want %d", counted.n, want)
	}
	real := fixture(t, "solid-8x3.avif")
	// The meta buffer (a few hundred bytes) and the readers around it.
	if allocated := bytesPerCall(func() { AVIFSize(bytes.NewReader(real)) }); allocated > 1<<10 {
		t.Fatalf("a real file costs %d bytes a call, want at most 1 KiB", allocated)
	}
}
