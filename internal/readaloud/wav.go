package readaloud

import (
	"encoding/binary"
	"fmt"
	"math"
)

// WAVHeaderBytes is the length of the header WAV writes.
const WAVHeaderBytes = 44

// WAV wraps 16-bit little-endian mono PCM at rate in a WAV file.
func WAV(pcm []byte, rate int) ([]byte, error) {
	if len(pcm) > math.MaxUint32-WAVHeaderBytes {
		return nil, fmt.Errorf("%d bytes of audio do not fit a WAV file", len(pcm))
	}
	const channels, bits = 1, 16
	out := make([]byte, WAVHeaderBytes, WAVHeaderBytes+len(pcm))
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(pcm)))
	copy(out[8:], "WAVE")
	copy(out[12:], "fmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:], channels)
	binary.LittleEndian.PutUint32(out[24:], uint32(rate))
	binary.LittleEndian.PutUint32(out[28:], uint32(rate*channels*bits/8))
	binary.LittleEndian.PutUint16(out[32:], channels*bits/8)
	binary.LittleEndian.PutUint16(out[34:], bits)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(pcm)))
	return append(out, pcm...), nil
}
