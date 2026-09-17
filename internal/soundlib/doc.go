// Package soundlib owns the custom notification-cue directory
// (<configDir>/sounds/): listing, writing and deleting `<id>.wav` cues,
// validating every one of them against the single canonical WAV shape the
// frontend is allowed to play, and seeding the generated SOUNDS.md
// reference at boot.
package soundlib
