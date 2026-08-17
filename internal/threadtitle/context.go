package threadtitle

import (
	"slices"
	"strings"

	"agent-overflow/internal/stringsx"
)

// Budgets and markers for the regeneration context, ported from
// t3-code's ProviderCommandReactor.formatThreadTitleContext so both
// apps hand the model comparable material.
const (
	// maxContextChars bounds the whole formatted context.
	maxContextChars = 8_000
	// maxFirstUserChars bounds the pinned first user message, which is
	// kept even when the rest of the thread is truncated: it is where
	// the thread's original subject lives.
	maxFirstUserChars = 2_000

	earlierTruncatedMarker   = "[Earlier content truncated]\n\n"
	firstUserTruncatedMarker = "\n[First user message truncated]"

	sectionSeparator = "\n\n"
)

// Message is one turn of thread history as the context formatter sees
// it. Role is the store's role string; anything other than "user" or
// "assistant" is skipped. AttachmentNames are filenames only — the
// regeneration path passes no bytes and no image paths.
type Message struct {
	Role            string
	Text            string
	AttachmentNames []string
}

// FormatThreadContext renders an oldest-first slice of thread messages
// into the "Thread contents:" section of the regeneration prompt.
//
// Retention is newest-first inside a total budget: recent turns are
// what tell the model where the thread ended up. When anything was
// dropped, the output is marked as truncated — the regeneration prompt
// tells the model to preserve scope words "especially when earlier
// content is truncated", so a windowed transcript that renders as a
// seamless whole is a lie the model acts on.
//
// rowsDropped is the caller's report that the STORE's row window
// already excluded matching rows. It is a second, independent source of
// truncation: 201 short messages fit the character budget with room to
// spare and are still an incomplete thread.
//
// The thread's FIRST user message is pinned back at the top (capped on
// its own) when truncation dropped it, because the original ask is what
// keeps a long thread's subject from drifting to its last finding. It
// is NOT pinned when the newest-first walk already retained it in
// place — that would render the same message twice. Returns "" for a
// thread with no renderable message and no dropped rows.
func FormatThreadContext(messages []Message, rowsDropped bool) string {
	recent, truncated, lowestFullIdx := collectRecentContext(messages, maxContextChars)
	if !truncated && !rowsDropped {
		return recent
	}

	pinIdx, pinned, ok := firstUserSection(messages)
	if !ok {
		// Nothing to pin. Re-collect inside the marker's own budget so the
		// marker cannot push the output past maxContextChars.
		retained, _, _ := collectRecentContext(messages, maxContextChars-len(earlierTruncatedMarker))
		if retained == "" {
			// A marker with nothing behind it is not a context — it is a
			// prompt that says only "something was dropped". Callers treat
			// "" as "no subject to name" and skip the run.
			return ""
		}
		return earlierTruncatedMarker + retained
	}
	if pinIdx >= lowestFullIdx {
		// The full-budget walk already retained the first user message in
		// place. Re-collect inside the marker's budget — and re-CHECK: the
		// marker's few bytes can push the walk short of the first user
		// message it just retained, and dropping the original ask to make
		// room for a truncation marker is exactly backwards. When the
		// re-collect still holds it, pinning would render it twice; when it
		// does not, fall through to the pin branch below.
		// An empty re-collect cannot take this return: it reports
		// markerLowestFullIdx == len(messages), which pinIdx is always
		// below, so the walk-lost-everything case pins below instead of
		// emitting a bare marker.
		retained, _, markerLowestFullIdx := collectRecentContext(messages, maxContextChars-len(earlierTruncatedMarker))
		if pinIdx >= markerLowestFullIdx {
			return earlierTruncatedMarker + retained
		}
	}
	pinned = limitFirstUserSection(pinned)

	budget := maxContextChars - len(pinned) - len(sectionSeparator) - len(earlierTruncatedMarker)
	retained, _, _ := collectRecentContext(messages, budget)
	return pinned + sectionSeparator + earlierTruncatedMarker + retained
}

// collectRecentContext walks messages newest-first, prepending each
// renderable section until maxChars is exhausted. The section that
// overruns the budget contributes its TAIL (its most recent lines)
// behind its own role header when any room is left, and stops the walk
// either way.
//
// Reports whether anything was dropped, plus the LOWEST message index
// retained in full — the overrun section does not count, since only
// part of it made it in. len(messages) means nothing was retained,
// which keeps "was message i retained?" a plain `i >= lowestFullIdx`
// for every caller.
func collectRecentContext(messages []Message, maxChars int) (string, bool, int) {
	var sections []string
	total := 0
	truncated := false
	lowestFullIdx := len(messages)

	for i := len(messages) - 1; i >= 0; i-- {
		section, header, ok := formatSection(messages[i])
		if !ok {
			continue
		}
		separator := 0
		if total > 0 {
			separator = len(sectionSeparator)
		}
		available := maxChars - total - separator
		if len(section) > available {
			// The overrun section keeps its `ROLE:\n` header. The
			// regeneration prompt's first instruction is "Read the USER
			// messages first", which an unlabeled tail blob defeats —
			// a deliberate divergence from t3, which tails the whole
			// section header included. Too little room for the header
			// plus any text means the section contributes nothing.
			// TailRunes can return "" even for room > 0 (the cut landing
			// inside one multi-byte rune), so the text is checked, not the
			// room — a bare `ROLE:` label is exactly what this branch must
			// not emit.
			if room := available - len(header); room > 0 {
				if text := stringsx.TailRunes(section[len(header):], room); text != "" {
					tail := header + text
					sections = append(sections, tail)
					total += len(tail) + separator
				}
			}
			truncated = true
			break
		}
		sections = append(sections, section)
		total += len(section) + separator
		lowestFullIdx = i
	}

	slices.Reverse(sections)
	return strings.Join(sections, sectionSeparator), truncated, lowestFullIdx
}

// formatSection renders one message as `ROLE:\n<text>` with an optional
// trailing `[Attachments: ...]` line, returning the section and its
// `ROLE:\n` header separately so a tail cut can keep the header.
// Reports false for a message that renders to nothing — a
// non-conversational role, or no text and no attachment names.
func formatSection(message Message) (section, header string, ok bool) {
	role := strings.ToLower(strings.TrimSpace(message.Role))
	if role != "user" && role != "assistant" {
		return "", "", false
	}

	var contents []string
	if text := strings.TrimSpace(message.Text); text != "" {
		contents = append(contents, text)
	}
	if names := strings.Join(message.AttachmentNames, ", "); names != "" {
		contents = append(contents, "[Attachments: "+names+"]")
	}
	if len(contents) == 0 {
		return "", "", false
	}
	header = strings.ToUpper(role) + ":\n"
	return header + strings.Join(contents, "\n"), header, true
}

// firstUserSection returns the index and rendered section of the oldest
// user message that renders to a non-empty section.
func firstUserSection(messages []Message) (int, string, bool) {
	for i, message := range messages {
		if strings.ToLower(strings.TrimSpace(message.Role)) != "user" {
			continue
		}
		if section, _, ok := formatSection(message); ok {
			return i, section, true
		}
	}
	return len(messages), "", false
}

func limitFirstUserSection(section string) string {
	if len(section) <= maxFirstUserChars {
		return section
	}
	return stringsx.ClipRunes(section, maxFirstUserChars-len(firstUserTruncatedMarker)) + firstUserTruncatedMarker
}
