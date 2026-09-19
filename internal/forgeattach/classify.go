package forgeattach

import (
	"bytes"
	"encoding/binary"
	"errors"
	"mime"
	"net/http"
	"path/filepath"

	"agent-overflow/internal/attachment"
)

// Kinds. The frontend chooses an element from these and nothing else:
// <img>, <video>, <audio>, or a download link.
const (
	KindImage = "image"
	KindVideo = "video"
	KindAudio = "audio"
	KindFile  = "file"
)

// isoVideoBrands are the ISO base-media brands a browser plays in a
// <video>. Read from the ftyp box's major brand and its compatible-brand
// list, because a file whose major brand is "isom" is routinely the same
// H.264 both forges accept from a drag-and-drop.
var isoVideoBrands = map[string]string{
	"isom": "video/mp4",
	"iso2": "video/mp4",
	"iso4": "video/mp4",
	"iso5": "video/mp4",
	"iso6": "video/mp4",
	"mp41": "video/mp4",
	"mp42": "video/mp4",
	"avc1": "video/mp4",
	"dash": "video/mp4",
	"M4V ": "video/mp4",
	"MSNV": "video/mp4",
	"qt  ": "video/quicktime",
}

// isoAudioBrands is the audio half of the same box. Small on purpose:
// the point is that an <audio> element renders something sensible, not
// that this becomes a format database.
var isoAudioBrands = map[string]string{
	"M4A ": "audio/mp4",
	"M4B ": "audio/mp4",
}

// Classify decides what one attachment body is from its BYTES, with the
// filename consulted only where the bytes are genuinely ambiguous (an
// Ogg container) or say nothing at all (the file fallback).
//
// A reference's own spelling is never trusted: GitHub's asset URLs carry
// no extension, and a forge will happily serve a .png that is a video.
//
// The returned mime is what the byte route writes as Content-Type for
// image, video and audio. For KindFile it is DISPLAY ONLY — the route
// serves every file under application/octet-stream — so a text or svg
// type here can never become something an engine parses at the SPA
// origin.
func Classify(data []byte, filename string) (mimeType, kind string, err error) {
	if len(data) == 0 {
		return "", "", errors.New("attachment body is empty")
	}
	if detected, imageErr := attachment.DetectDisplayImageMIME(data); imageErr == nil {
		// A payload that IS an image but decodes to an unreasonable
		// number of pixels is an error rather than a download: it would
		// be handed to the same <img> a safe one is, and the failure has
		// to reach the person who clicked rather than the renderer.
		if err := attachment.ValidateDisplayImage(data, detected); err != nil {
			return "", "", err
		}
		return detected, KindImage, nil
	}
	if brands, ok := isoBrands(data); ok {
		for _, brand := range brands {
			if mimeType, ok := isoVideoBrands[brand]; ok {
				return mimeType, KindVideo, nil
			}
			if mimeType, ok := isoAudioBrands[brand]; ok {
				return mimeType, KindAudio, nil
			}
		}
	}
	if mimeType, kind, ok := sniffContainer(data); ok {
		return mimeType, kind, nil
	}
	return fileMIME(filename), KindFile, nil
}

// sniffContainer covers the non-ISO media containers, leaning on the
// stdlib table where it already answers and adding only what it does not
// carry (flac, adts aac, and which half of an Ogg stream this is).
func sniffContainer(data []byte) (mimeType, kind string, ok bool) {
	switch http.DetectContentType(data) {
	case "video/webm":
		return "video/webm", KindVideo, true
	case "video/avi":
		return "video/avi", KindVideo, true
	case "video/mp4":
		return "video/mp4", KindVideo, true
	case "audio/mpeg":
		return "audio/mpeg", KindAudio, true
	case "audio/wave":
		// The spelling browsers agree on; Go reports the registered one.
		return "audio/wav", KindAudio, true
	case "application/ogg":
		return oggStream(data)
	}
	if bytes.HasPrefix(data, []byte("fLaC")) {
		return "audio/flac", KindAudio, true
	}
	// ADTS AAC: 12 sync bits, then the MPEG-4 layer bits, which are
	// always zero. 0xF1 / 0xF9 separate it from an MPEG audio frame,
	// which the stdlib already answered above.
	if len(data) >= 2 && data[0] == 0xFF && (data[1] == 0xF1 || data[1] == 0xF9) {
		return "audio/aac", KindAudio, true
	}
	return "", "", false
}

// oggStream decides which half of an Ogg container this is by the codec
// identification header on the first page, which is the only thing that
// distinguishes .ogv from .oga on the wire.
func oggStream(data []byte) (mimeType, kind string, ok bool) {
	head := data
	if len(head) > 256 {
		head = head[:256]
	}
	if bytes.Contains(head, []byte("theora")) || bytes.Contains(head, []byte("\x01video")) {
		return "video/ogg", KindVideo, true
	}
	// Vorbis, Opus, Speex and Ogg FLAC all land here, and so does a
	// stream whose codec header this window missed: audio is the
	// answer that renders rather than the answer that is always right.
	return "audio/ogg", KindAudio, true
}

// isoBrands reads the ftyp box's major brand plus its compatible brands.
//
// The declared box size is clamped before it is used as a bound: it is a
// number from the payload, and a crafted one would otherwise decide how
// long this loop runs.
func isoBrands(data []byte) ([]string, bool) {
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		return nil, false
	}
	const maxBrandBoxBytes = 256
	size := int(binary.BigEndian.Uint32(data[:4]))
	if size > len(data) {
		size = len(data)
	}
	if size > maxBrandBoxBytes {
		size = maxBrandBoxBytes
	}
	brands := []string{string(data[8:12])}
	for i := 16; i+4 <= size; i += 4 {
		brands = append(brands, string(data[i:i+4]))
	}
	return brands, true
}

// fileMIME is the DISPLAY type for a payload no signature claimed. The
// extension is all there is left to go on, and the byte route never
// serves anything under it.
func fileMIME(filename string) string {
	if detected := mime.TypeByExtension(filepath.Ext(filename)); detected != "" {
		return detected
	}
	return "application/octet-stream"
}
