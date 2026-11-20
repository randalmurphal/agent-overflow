package attachment

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// The bounds AVIFSize reads within, besides the caller's reader.
const (
	// avifMetaMax is the largest meta box AVIFSize buffers. An encoder
	// writes a few hundred bytes of it; a larger one answers unknown.
	avifMetaMax = 256 << 10
	// avifTopBoxesMax is how many top-level boxes AVIFSize reads before
	// meta. An encoder writes meta second, after ftyp.
	avifTopBoxesMax = 16
	// avifFtypMax is the largest ftyp box AVIFSize reads the brand of.
	avifFtypMax = 4 << 10
)

// AVIFSize answers the pixel size of the AVIF still image r reads: the
// ispe property of its primary item (ISO/IEC 23008-12: meta's pitm names
// the item, iprp's ipma associates it with properties in ipco), width and
// height swapped by an irot of an odd quarter turn. It reads r only as far
// as the end of the meta box, never past what r yields, so the caller's
// reader is the byte bound; a top-level box before meta is skipped by
// discarding, never buffered. Unknown (the zero DisplaySize) for anything
// it cannot answer exactly: an image sequence (major brand avis), a
// primary item with a clap (its crop), with no ispe or more than one, a
// size outside the pixel budget, meta past avifMetaMax or after
// avifTopBoxesMax other boxes, and every malformed or truncated structure.
// Unknown is no error: the caller serves the image as it would without a
// size. Every count and size read from the bytes is checked against what
// remains before it drives a loop or an allocation.
func AVIFSize(r io.Reader) DisplaySize {
	meta, ok := avifMeta(r)
	if !ok {
		return DisplaySize{}
	}
	width, height, ok := avifPrimarySize(meta)
	if !ok || width == 0 || height == 0 || width > thumbPixelBudget || height > thumbPixelBudget || !WithinPixelBudget(int(width), int(height)) {
		return DisplaySize{}
	}
	return DisplaySize{Width: int(width), Height: int(height)}
}

var errAVIFBox = errors.New("malformed box")

// avifMeta reads r's top-level boxes up to meta and answers meta's
// payload.
func avifMeta(r io.Reader) ([]byte, bool) {
	var header [16]byte
	for range avifTopBoxesMax {
		if _, err := io.ReadFull(r, header[:8]); err != nil {
			return nil, false
		}
		size := uint64(binary.BigEndian.Uint32(header[:4]))
		kind := string(header[4:8])
		headerLen := uint64(8)
		toEnd := false
		switch size {
		case 0:
			toEnd = true
		case 1:
			if _, err := io.ReadFull(r, header[8:16]); err != nil {
				return nil, false
			}
			size, headerLen = binary.BigEndian.Uint64(header[8:16]), 16
		}
		if !toEnd && (size < headerLen || size > math.MaxInt64) {
			return nil, false
		}
		payload := int64(size - headerLen)
		switch kind {
		case "meta":
			limit := payload
			if toEnd || payload > avifMetaMax {
				// One byte past the cap tells a box over it from one at it.
				limit = avifMetaMax + 1
			}
			meta, err := io.ReadAll(io.LimitReader(r, limit))
			switch {
			case err != nil, len(meta) > avifMetaMax:
				return nil, false
			case !toEnd && int64(len(meta)) != payload:
				return nil, false
			}
			return meta, true
		case "ftyp":
			// An image sequence shows its track, whose size this does not read.
			if toEnd || payload < 4 || payload > avifFtypMax {
				return nil, false
			}
			if _, err := io.ReadFull(r, header[:4]); err != nil || string(header[:4]) == "avis" {
				return nil, false
			}
			payload -= 4
		}
		if toEnd {
			return nil, false
		}
		if n, err := io.CopyN(io.Discard, r, payload); err != nil || n != payload {
			return nil, false
		}
	}
	return nil, false
}

// isoBox is one box inside a buffered box: its type and payload.
type isoBox struct {
	kind []byte
	body []byte
}

// nextBox splits b's first box from the rest: a 32-bit size, a 64-bit
// largesize (size 1) or the rest of b (size 0), each within b.
func nextBox(b []byte) (isoBox, []byte, error) {
	if len(b) < 8 {
		return isoBox{}, nil, errAVIFBox
	}
	size := uint64(binary.BigEndian.Uint32(b[:4]))
	kind := b[4:8]
	headerLen := uint64(8)
	switch size {
	case 0:
		size = uint64(len(b))
	case 1:
		if len(b) < 16 {
			return isoBox{}, nil, errAVIFBox
		}
		size, headerLen = binary.BigEndian.Uint64(b[8:16]), 16
	}
	if size < headerLen || size > uint64(len(b)) {
		return isoBox{}, nil, errAVIFBox
	}
	return isoBox{kind: kind, body: b[headerLen:size]}, b[size:], nil
}

// avifPrimarySize reads meta's payload (a full box: version and flags,
// then boxes) for the primary item's ispe, swapped by an odd irot.
func avifPrimarySize(meta []byte) (width, height uint32, ok bool) {
	if len(meta) < 4 {
		return 0, 0, false
	}
	var pitm, iprp []byte
	var pitms, iprps int
	for rest := meta[4:]; len(rest) > 0; {
		box, next, err := nextBox(rest)
		if err != nil {
			return 0, 0, false
		}
		rest = next
		switch string(box.kind) {
		case "pitm":
			pitm, pitms = box.body, pitms+1
		case "iprp":
			iprp, iprps = box.body, iprps+1
		}
	}
	if pitms != 1 || iprps != 1 {
		return 0, 0, false
	}
	primary, ok := avifPrimaryItem(pitm)
	if !ok {
		return 0, 0, false
	}
	var ipco []byte
	var ipcos int
	// Bit i: property index i is associated with the primary item.
	var wanted [1 << 15 / 64]uint64
	maxIndex, entries := 0, 0
	for rest := iprp; len(rest) > 0; {
		box, next, err := nextBox(rest)
		if err != nil {
			return 0, 0, false
		}
		rest = next
		switch string(box.kind) {
		case "ipco":
			ipco, ipcos = box.body, ipcos+1
		case "ipma":
			found, ok := avifAssociations(box.body, primary, &wanted, &maxIndex)
			if !ok {
				return 0, 0, false
			}
			entries += found
		}
	}
	// An item is listed once; twice is no answer to trust.
	if ipcos != 1 || entries != 1 {
		return 0, 0, false
	}
	index, ispes := 0, 0
	rotated := false
	for rest := ipco; len(rest) > 0; {
		box, next, err := nextBox(rest)
		if err != nil {
			return 0, 0, false
		}
		rest = next
		index++
		if index >= len(wanted)*64 || wanted[index/64]&(1<<(index%64)) == 0 {
			continue
		}
		switch string(box.kind) {
		case "ispe":
			// A full box: version and flags, then width and height.
			if len(box.body) < 12 {
				return 0, 0, false
			}
			width, height = binary.BigEndian.Uint32(box.body[4:8]), binary.BigEndian.Uint32(box.body[8:12])
			ispes++
		case "irot":
			if len(box.body) < 1 {
				return 0, 0, false
			}
			rotated = box.body[0]&1 == 1
		case "clap":
			// The displayed image is the crop, which this does not compute.
			return 0, 0, false
		}
	}
	// An association to a property ipco does not hold is malformed.
	if ispes != 1 || maxIndex > index {
		return 0, 0, false
	}
	if rotated {
		width, height = height, width
	}
	return width, height, true
}

// avifPrimaryItem reads pitm's payload: the item id, 16 bits in version 0
// and 32 in version 1.
func avifPrimaryItem(pitm []byte) (uint32, bool) {
	if len(pitm) < 4 {
		return 0, false
	}
	switch pitm[0] {
	case 0:
		if len(pitm) < 6 {
			return 0, false
		}
		return uint32(binary.BigEndian.Uint16(pitm[4:6])), true
	case 1:
		if len(pitm) < 8 {
			return 0, false
		}
		return binary.BigEndian.Uint32(pitm[4:8]), true
	}
	return 0, false
}

// avifAssociations reads ipma's payload and marks in wanted the property
// indexes of item: item ids are 16 bits in version 0 and 32 in version 1,
// indexes 7 bits, or 15 when flag 1 is set, after an essential bit. It
// answers how many entries list item, and raises maxIndex to the largest
// index marked.
func avifAssociations(ipma []byte, item uint32, wanted *[1 << 15 / 64]uint64, maxIndex *int) (int, bool) {
	if len(ipma) < 8 {
		return 0, false
	}
	version, wide := ipma[0], ipma[3]&1 == 1
	if version > 1 {
		return 0, false
	}
	idLen, indexLen := 2, 1
	if version == 1 {
		idLen = 4
	}
	if wide {
		indexLen = 2
	}
	count := uint64(binary.BigEndian.Uint32(ipma[4:8]))
	p := ipma[8:]
	found := 0
	// Each entry is checked against what remains before it is read, so a
	// count past the data ends the loop within len(p)/(idLen+1) entries.
	for range count {
		if len(p) < idLen+1 {
			return 0, false
		}
		var id uint32
		if idLen == 2 {
			id = uint32(binary.BigEndian.Uint16(p))
		} else {
			id = binary.BigEndian.Uint32(p)
		}
		associations := int(p[idLen])
		p = p[idLen+1:]
		if associations*indexLen > len(p) {
			return 0, false
		}
		if id == item {
			found++
			for i := range associations {
				var index int
				if wide {
					index = int(binary.BigEndian.Uint16(p[2*i:]) & 0x7fff)
				} else {
					index = int(p[i] & 0x7f)
				}
				// Index 0 is no property.
				if index == 0 {
					continue
				}
				wanted[index/64] |= 1 << (index % 64)
				*maxIndex = max(*maxIndex, index)
			}
		}
		p = p[associations*indexLen:]
	}
	return found, true
}
