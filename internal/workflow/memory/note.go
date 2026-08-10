package memory

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The note vocabulary is CLOSED. A campaign accrues hundreds of these across
// waves and two readers consume them — `grep` and the injected digest — so both
// need to be able to say "show me the warnings" without knowing what an element
// decided to call one. An unrecognised kind is refused loudly (a CLI usage
// error, an envelope validation finding) rather than filed under itself: a kind
// nobody groups by is a note nobody reads.
//
// `ruling` as an operator-only kind is deliberately absent. It is deferred
// pending its own scope conversation; do not add it here without one.
const (
	// KindPattern is a shape that worked and should be repeated.
	KindPattern = "pattern"
	// KindWarning is a trap: something that looked right and was not.
	KindWarning = "warning"
	// KindLearning is a fact about the environment, the codebase, or the tools
	// that the next element would otherwise rediscover.
	KindLearning = "learning"
	// KindHandoff is state one element is deliberately leaving for the next.
	// It is the one kind with an ordering privilege in the digest.
	KindHandoff = "handoff"
)

// Kinds is the closed vocabulary, in the order the digest renders its groups.
// Only `handoff` leading is load-bearing (it exists to reach the next wave);
// the remaining order is a stable reading order, not a priority claim.
var Kinds = []string{KindHandoff, KindWarning, KindPattern, KindLearning}

// KnownKind reports whether name is one of the four kinds.
func KnownKind(name string) bool {
	for _, kind := range Kinds {
		if kind == name {
			return true
		}
	}
	return false
}

// KindList renders the vocabulary for a refusal message, so every surface
// naming the legal kinds names the same four in the same order.
func KindList() string { return strings.Join(Kinds, ", ") }

const (
	// MaxTextBytes bounds one note's prose. A note is a lesson, not an account:
	// the narrative file is where an element writes what it did, and a note past
	// this size is a narrative that wandered into the wrong channel. It also
	// keeps one appended line inside a single bounded write.
	MaxTextBytes = 4 * 1024
	// MaxFiles bounds the paths one note may cite. The list is a pointer at
	// evidence, not an inventory of a change.
	MaxFiles = 20
	// MaxFilePathBytes bounds one cited path.
	MaxFilePathBytes = 512
)

// Draft is what an AUTHOR supplies: the three fields an element or a human
// chooses. It carries no provenance and no timestamp, and it deliberately has
// no field for either — provenance is the app's answer to "who wrote this",
// and a shape that could carry a supplied one is a shape that could be lied to.
type Draft struct {
	Kind  string   `json:"kind"`
	Text  string   `json:"text"`
	Files []string `json:"files,omitempty"`
}

// Provenance is stamped by the app from the calling session's own coordinates.
// Every field is a fact the caller could not have chosen.
type Provenance struct {
	// RunID is the run the note was written from.
	RunID string `json:"runId"`
	// PhaseID and Attempt name the element. Both are empty/zero for a note a
	// human wrote from an interactive thread, which belongs to no phase.
	PhaseID string `json:"phaseId,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	// UnitID is set when the writer was a fan-out unit or a join.
	UnitID string `json:"unitId,omitempty"`
	// Wave is the writing run's caller-chain depth relative to the tree root:
	// 0 for the root run itself, 1 for a run it called, and so on. It is the
	// engine's own `call_depth`, never a counter this package maintains.
	Wave int `json:"wave"`
}

// Note is one line of `notes.ndjson`.
type Note struct {
	Kind       string     `json:"kind"`
	Text       string     `json:"text"`
	Files      []string   `json:"files,omitempty"`
	Provenance Provenance `json:"provenance"`
	// At is the app's clock in unix milliseconds. Ordering in the digest is by
	// this value, and it is what "newest first" means.
	At int64 `json:"at"`
}

// Finding is one reason a draft is not a note. Path is relative to the draft
// (`.kind`, `.files[3]`), so a caller composing a wider document — the envelope
// post-validator prefixing `$.memory[2]` — owns its own root without this
// package knowing about one.
type Finding struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (f Finding) Error() string { return f.Path + ": " + f.Message }

// ValidationError carries every finding a draft produced.
type ValidationError struct {
	Findings []Finding
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Findings))
	for _, finding := range e.Findings {
		parts = append(parts, finding.Error())
	}
	return strings.Join(parts, "; ")
}

// ValidateDraft reports every rule a draft breaks. It is the ONE definition of
// a valid note: the CLI verb, the envelope post-validator, and the append path
// all call it, so a note refused on one channel cannot be accepted on another.
//
// Findings are collected rather than short-circuited, and sorted by path, so a
// single retry turn sees everything wrong with what it sent.
func ValidateDraft(draft Draft) []Finding {
	var findings []Finding
	if !KnownKind(draft.Kind) {
		findings = append(findings, Finding{
			Path:    ".kind",
			Message: fmt.Sprintf("must be one of %s", KindList()),
		})
	}
	switch text := strings.TrimSpace(draft.Text); {
	case text == "":
		findings = append(findings, Finding{Path: ".text", Message: "must be a non-empty string"})
	case len(draft.Text) > MaxTextBytes:
		findings = append(findings, Finding{
			Path: ".text",
			Message: fmt.Sprintf("is %d bytes; maximum is %d (a note is a lesson, not a narrative)",
				len(draft.Text), MaxTextBytes),
		})
	}
	if len(draft.Files) > MaxFiles {
		findings = append(findings, Finding{
			Path:    ".files",
			Message: fmt.Sprintf("cites %d paths; maximum is %d", len(draft.Files), MaxFiles),
		})
	}
	for index, path := range draft.Files {
		location := fmt.Sprintf(".files[%d]", index)
		switch {
		case strings.TrimSpace(path) == "":
			findings = append(findings, Finding{Path: location, Message: "must be a non-empty string"})
		case len(path) > MaxFilePathBytes:
			findings = append(findings, Finding{
				Path:    location,
				Message: fmt.Sprintf("is %d bytes; maximum is %d", len(path), MaxFilePathBytes),
			})
		case hasControlRune(path):
			findings = append(findings, Finding{Path: location, Message: "must not contain control characters"})
		}
	}
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Path < findings[j].Path })
	return findings
}

// hasControlRune reports whether a path carries a byte no filesystem path
// legitimately holds. Text may contain newlines — a note is prose — but a
// cited path that does is a parsing accident, not a path.
func hasControlRune(value string) bool {
	for _, r := range value {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// NewNote validates a draft and stamps it into a note. The provenance and the
// timestamp are parameters rather than draft fields precisely so this is the
// only way a note comes into existence.
func NewNote(draft Draft, provenance Provenance, atUnixMillis int64) (Note, error) {
	if findings := ValidateDraft(draft); len(findings) > 0 {
		return Note{}, &ValidationError{Findings: findings}
	}
	files := make([]string, 0, len(draft.Files))
	for _, path := range draft.Files {
		files = append(files, strings.TrimSpace(path))
	}
	if len(files) == 0 {
		files = nil
	}
	return Note{
		Kind:       draft.Kind,
		Text:       strings.TrimSpace(draft.Text),
		Files:      files,
		Provenance: provenance,
		At:         atUnixMillis,
	}, nil
}
