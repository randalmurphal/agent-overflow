package soundlib

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The canonical cue format. ONE shape, not a family of accepted ones.
//
// Nothing a user hands this app is stored or played as handed: the browser
// decodes the upload with the engine's own sandboxed decoder (which yields
// PCM samples or throws), the frontend re-renders those samples to the shape
// below, and Go checks the result structurally before it is written and again
// on every listing. That is only worth anything if "the shape below" is
// exactly one thing — a validator that accepted several chunk layouts, sample
// widths or rates would be a parser, and a parser is the attack surface the
// re-render exists to remove.
//
// The numbers themselves are the frontend's mixdown target
// (lib/audio/renderCue.ts): mono so a cue cannot arrive panned, 44.1 kHz
// because every engine's OfflineAudioContext resamples to it without a
// plugin, and 16-bit because it is the widest integer format the WAV header
// describes without an extension block.
const (
	// headerBytes is the canonical header's exact size: 12 bytes of RIFF
	// container, 24 of `fmt `, 8 of the `data` chunk header. A canonical
	// cue is this header followed by samples and nothing else, so every
	// length in the file is derivable from the file's own size.
	headerBytes = 44

	wavChannels      = 1
	wavSampleRate    = 44100
	wavBitsPerSample = 16
	wavBlockAlign    = wavChannels * wavBitsPerSample / 8 // 2
	wavByteRate      = wavSampleRate * wavBlockAlign      // 88200

	// MaxSoundSeconds is the duration cap. A notification cue is a
	// punctuation mark: past a few seconds it stops being an alert and
	// starts being a clip that plays over whatever the user does next, and
	// the player's cooldown (NOTIFICATION_SOUND_COOLDOWN_MS) is sized for
	// cues that finish long before the next one can start.
	MaxSoundSeconds = 3

	// MaxSoundDataBytes is MaxSoundSeconds of canonical PCM, and therefore
	// the exact byte cap on a cue's sample data.
	MaxSoundDataBytes = MaxSoundSeconds * wavByteRate // 264600

	// MaxSoundBytes is one whole cue file at the duration cap. It is an
	// EXACT bound rather than a generous one: the format is fully
	// determined, so the longest legal file has a computable size.
	MaxSoundBytes = headerBytes + MaxSoundDataBytes // 264644
)

// ValidateWAV reports why data is not a canonical cue, or nil when it is.
//
// Pure and total: it reads the header fields, compares them against the one
// accepted shape, and never allocates proportionally to the audio. The error
// text is user-facing — it becomes a Settings warning beside the file that
// produced it — so each message names the field and what was expected rather
// than saying "invalid".
//
// Checks run header-order EXCEPT that sample geometry (channels, rate, width)
// is checked before the two derived fields (block align, byte rate). A file
// that is internally consistent but stereo, 22 kHz or 8-bit fails all three,
// and "8-bit samples" is the answer a person can act on; "byte rate 44100"
// is not.
func ValidateWAV(data []byte) error {
	if len(data) < headerBytes {
		return fmt.Errorf("truncated: %d bytes, and the canonical header alone is %d", len(data), headerBytes)
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return fmt.Errorf("not a RIFF/WAVE file (found %q and %q)", string(data[0:4]), string(data[8:12]))
	}
	// The RIFF size counts everything after its own four bytes. Checking it
	// against the real file length is what makes every later offset safe to
	// read as a fixed position rather than as a walked chunk list.
	if size := binary.LittleEndian.Uint32(data[4:8]); uint64(size) != uint64(len(data))-8 {
		return fmt.Errorf("the RIFF size field says %d bytes, the file holds %d", size, len(data)-8)
	}
	if string(data[12:16]) != "fmt " {
		return fmt.Errorf("expected a fmt chunk at offset 12, found %q", string(data[12:16]))
	}
	if size := binary.LittleEndian.Uint32(data[16:20]); size != 16 {
		return fmt.Errorf("the fmt chunk is %d bytes, and a canonical cue's is 16 (PCM, no extension block)", size)
	}
	if format := binary.LittleEndian.Uint16(data[20:22]); format != 1 {
		return fmt.Errorf("format tag %d, and a canonical cue is uncompressed PCM (1)", format)
	}
	if channels := binary.LittleEndian.Uint16(data[22:24]); channels != wavChannels {
		return fmt.Errorf("%d channels, and a canonical cue is mono (%d)", channels, wavChannels)
	}
	if rate := binary.LittleEndian.Uint32(data[24:28]); rate != wavSampleRate {
		return fmt.Errorf("%d Hz, and a canonical cue is %d Hz", rate, wavSampleRate)
	}
	if bits := binary.LittleEndian.Uint16(data[34:36]); bits != wavBitsPerSample {
		return fmt.Errorf("%d-bit samples, and a canonical cue is %d-bit", bits, wavBitsPerSample)
	}
	if align := binary.LittleEndian.Uint16(data[32:34]); align != wavBlockAlign {
		return fmt.Errorf("block align %d, and a canonical cue's is %d", align, wavBlockAlign)
	}
	if rate := binary.LittleEndian.Uint32(data[28:32]); rate != wavByteRate {
		return fmt.Errorf("byte rate %d, and a canonical cue's is %d", rate, wavByteRate)
	}
	if string(data[36:40]) != "data" {
		return fmt.Errorf(
			"expected a data chunk at offset %d, found %q; a canonical cue carries no other chunk",
			headerBytes-8, string(data[36:40]))
	}
	size := binary.LittleEndian.Uint32(data[40:44])
	if uint64(size) != uint64(len(data))-headerBytes {
		return fmt.Errorf("the data chunk header says %d bytes, the file holds %d after the header", size, len(data)-headerBytes)
	}
	if size == 0 {
		return errors.New("the data chunk is empty, so there is no audio to play")
	}
	if size%wavBlockAlign != 0 {
		return fmt.Errorf("the data chunk is %d bytes, which is not a whole number of %d-bit mono samples", size, wavBitsPerSample)
	}
	if size > MaxSoundDataBytes {
		return fmt.Errorf(
			"%.2f seconds of audio, and the limit is %d (%d bytes)",
			float64(size)/float64(wavByteRate), MaxSoundSeconds, MaxSoundDataBytes)
	}
	return nil
}
