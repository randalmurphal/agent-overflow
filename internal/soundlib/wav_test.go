package soundlib

import (
	"encoding/binary"
	"strings"
	"testing"
)

// canonicalWAV builds a conforming cue holding sampleBytes of silence. It is
// the fixture every refusal below mutates exactly one field of, so a test
// that fails is naming its own mutation rather than some second defect it
// introduced by hand-assembling a header.
func canonicalWAV(sampleBytes int) []byte {
	out := make([]byte, headerBytes+sampleBytes)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1)
	binary.LittleEndian.PutUint16(out[22:24], wavChannels)
	binary.LittleEndian.PutUint32(out[24:28], wavSampleRate)
	binary.LittleEndian.PutUint32(out[28:32], wavByteRate)
	binary.LittleEndian.PutUint16(out[32:34], wavBlockAlign)
	binary.LittleEndian.PutUint16(out[34:36], wavBitsPerSample)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(sampleBytes))
	return out
}

func TestValidateWAVAcceptsTheCanonicalShape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes int
	}{
		{"one sample", wavBlockAlign},
		{"a quarter second", wavByteRate / 4},
		{"exactly the duration cap", MaxSoundDataBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateWAV(canonicalWAV(tc.bytes)); err != nil {
				t.Fatalf("ValidateWAV(%d sample bytes) = %v, want nil", tc.bytes, err)
			}
		})
	}
}

// Every refusal the format has, each from the canonical fixture with one
// field changed. `want` is a substring of the message a user will read in
// Settings, so a message that stops naming its field fails here.
func TestValidateWAVRefusals(t *testing.T) {
	// A zero `samples` means the shared default: a tenth of a second, which
	// is comfortably inside every cap so the mutation is the only thing
	// under test.
	const defaultSamples = wavByteRate / 10
	for _, tc := range []struct {
		name    string
		mutate  func([]byte) []byte
		want    string
		samples int
	}{
		{
			name:   "bad magic",
			mutate: func(b []byte) []byte { copy(b[0:4], "RIFX"); return b },
			want:   "not a RIFF/WAVE file",
		},
		{
			name:   "not WAVE",
			mutate: func(b []byte) []byte { copy(b[8:12], "AVI "); return b },
			want:   "not a RIFF/WAVE file",
		},
		{
			name:   "RIFF size disagrees with the file length",
			mutate: func(b []byte) []byte { binary.LittleEndian.PutUint32(b[4:8], 99); return b },
			want:   "the RIFF size field says 99 bytes",
		},
		{
			name:   "no fmt chunk",
			mutate: func(b []byte) []byte { copy(b[12:16], "JUNK"); return b },
			want:   `expected a fmt chunk at offset 12, found "JUNK"`,
		},
		{
			name:   "extensible fmt chunk",
			mutate: func(b []byte) []byte { binary.LittleEndian.PutUint32(b[16:20], 18); return b },
			want:   "the fmt chunk is 18 bytes",
		},
		{
			name:   "wrong format tag",
			mutate: func(b []byte) []byte { binary.LittleEndian.PutUint16(b[20:22], 3); return b },
			want:   "format tag 3",
		},
		{
			name: "stereo",
			mutate: func(b []byte) []byte {
				binary.LittleEndian.PutUint16(b[22:24], 2)
				binary.LittleEndian.PutUint16(b[32:34], 4)
				binary.LittleEndian.PutUint32(b[28:32], wavSampleRate*4)
				return b
			},
			want: "2 channels",
		},
		{
			name: "wrong sample rate",
			mutate: func(b []byte) []byte {
				binary.LittleEndian.PutUint32(b[24:28], 48000)
				binary.LittleEndian.PutUint32(b[28:32], 48000*wavBlockAlign)
				return b
			},
			want: "48000 Hz",
		},
		{
			name: "8-bit samples",
			mutate: func(b []byte) []byte {
				binary.LittleEndian.PutUint16(b[34:36], 8)
				binary.LittleEndian.PutUint16(b[32:34], 1)
				binary.LittleEndian.PutUint32(b[28:32], wavSampleRate)
				return b
			},
			want: "8-bit samples",
		},
		{
			name: "24-bit samples",
			mutate: func(b []byte) []byte {
				binary.LittleEndian.PutUint16(b[34:36], 24)
				binary.LittleEndian.PutUint16(b[32:34], 3)
				binary.LittleEndian.PutUint32(b[28:32], wavSampleRate*3)
				return b
			},
			want: "24-bit samples",
		},
		{
			name:   "block align disagrees with the geometry",
			mutate: func(b []byte) []byte { binary.LittleEndian.PutUint16(b[32:34], 4); return b },
			want:   "block align 4",
		},
		{
			name:   "byte rate disagrees with the geometry",
			mutate: func(b []byte) []byte { binary.LittleEndian.PutUint32(b[28:32], 176400); return b },
			want:   "byte rate 176400",
		},
		{
			// The failure an ordinary encoder produces: a LIST/INFO chunk
			// between `fmt ` and `data`, which moves the audio off 44.
			name:   "an extra chunk before data",
			mutate: func(b []byte) []byte { copy(b[36:40], "LIST"); return b },
			want:   `expected a data chunk at offset 36, found "LIST"`,
		},
		{
			// A trailing chunk after the samples: the data header no longer
			// accounts for the whole file.
			name: "a trailing chunk after data",
			mutate: func(b []byte) []byte {
				b = append(b, 'L', 'I', 'S', 'T', 0, 0, 0, 0)
				binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
				return b
			},
			want: "the data chunk header says",
		},
		{
			name: "odd data length",
			mutate: func(b []byte) []byte {
				b = append(b, 0)
				binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
				binary.LittleEndian.PutUint32(b[40:44], uint32(len(b)-headerBytes))
				return b
			},
			want: "is not a whole number of 16-bit mono samples",
		},
		{
			name:   "data header disagrees with the file length",
			mutate: func(b []byte) []byte { binary.LittleEndian.PutUint32(b[40:44], 2); return b },
			want:   "the data chunk header says 2 bytes",
		},
		{
			name: "empty data",
			mutate: func(b []byte) []byte {
				b = b[:headerBytes]
				binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
				binary.LittleEndian.PutUint32(b[40:44], 0)
				return b
			},
			want: "the data chunk is empty",
		},
		{
			name:    "longer than the cap",
			samples: MaxSoundDataBytes + wavBlockAlign,
			mutate:  func(b []byte) []byte { return b },
			want:    "seconds of audio, and the limit is 3",
		},
		{
			name:   "truncated",
			mutate: func(b []byte) []byte { return b[:20] },
			want:   "truncated: 20 bytes",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples := tc.samples
			if samples == 0 {
				samples = defaultSamples
			}
			err := ValidateWAV(tc.mutate(canonicalWAV(samples)))
			if err == nil {
				t.Fatalf("ValidateWAV accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateWAV(%s) = %q, want it to contain %q", tc.name, err, tc.want)
			}
		})
	}
}

// The caps are derived from the format, so they must stay derivable. A hand
// edit that moved one of them without the others would let a file through
// that the frontend's renderer can never produce.
func TestCapsFollowFromTheFormat(t *testing.T) {
	if MaxSoundDataBytes != 264600 {
		t.Fatalf("MaxSoundDataBytes = %d, want 264600 (3 s at 44100 Hz, 16-bit mono)", MaxSoundDataBytes)
	}
	if MaxSoundBytes != 264644 {
		t.Fatalf("MaxSoundBytes = %d, want 264644 (the 44-byte header plus the data cap)", MaxSoundBytes)
	}
	if MaxSoundBytes != headerBytes+MaxSoundDataBytes {
		t.Fatal("MaxSoundBytes is no longer the header plus the data cap")
	}
}
