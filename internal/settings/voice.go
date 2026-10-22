package settings

import (
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// The OpenAI API dictation options: what this computer sends with every
// live transcription session (internal/openai). The vocabulary is OpenAI's,
// restated here because this package stays dependency-free;
// internal/openai's tests pin the delay list to its own.

// Bounds for the dictation options. GetSettings ships them to every client,
// and OpenAI refuses a session whose options it rejects, so a hand-edited
// file must not be able to decide either.
const (
	MaxDictationAPILanguages    = 16
	MaxDictationAPIKeywords     = 100
	MaxDictationAPIKeywordRunes = 100
	MaxDictationAPIPromptBytes  = 2000
)

// DictationAPIDelays are gpt-live-transcribe's latency settings, earliest
// partial text first.
var DictationAPIDelays = []string{"minimal", "low", "medium", "high", "xhigh"}

// languageCode is an ISO 639 code such as "en" or "fil".
var languageCode = regexp.MustCompile(`^[a-z]{2,3}$`)

// validateDictationAPI is the strict path. Entries are trimmed and blank
// ones dropped, as a blank row in the settings UI is an unfilled row, but
// an entry OpenAI would refuse is refused here, naming it.
func validateDictationAPI(current *Settings) error {
	current.DictationAPIDelay = strings.TrimSpace(current.DictationAPIDelay)
	if current.DictationAPIDelay != "" && !slices.Contains(DictationAPIDelays, current.DictationAPIDelay) {
		return fmt.Errorf("dictationAPIDelay must be one of %s, or empty", strings.Join(DictationAPIDelays, ", "))
	}
	languages, err := dictationAPIList("dictationAPILanguages", current.DictationAPILanguages, MaxDictationAPILanguages, dictationAPILanguageProblem)
	if err != nil {
		return err
	}
	current.DictationAPILanguages = languages
	keywords, err := dictationAPIList("dictationAPIKeywords", current.DictationAPIKeywords, MaxDictationAPIKeywords, dictationAPIKeywordProblem)
	if err != nil {
		return err
	}
	current.DictationAPIKeywords = keywords
	current.DictationAPIPrompt = strings.TrimSpace(current.DictationAPIPrompt)
	if len(current.DictationAPIPrompt) > MaxDictationAPIPromptBytes {
		return fmt.Errorf("dictationAPIPrompt must be at most %d bytes (got %d)", MaxDictationAPIPromptBytes, len(current.DictationAPIPrompt))
	}
	return nil
}

// sanitizeDictationAPI is the lenient load path: whatever validation
// would refuse is dropped or cut, and logged.
func sanitizeDictationAPI(current *Settings) {
	current.DictationAPIDelay = strings.TrimSpace(current.DictationAPIDelay)
	if current.DictationAPIDelay != "" && !slices.Contains(DictationAPIDelays, current.DictationAPIDelay) {
		log.Printf("settings: invalid dictationAPIDelay %q, using OpenAI's default", current.DictationAPIDelay)
		current.DictationAPIDelay = ""
	}
	current.DictationAPILanguages = sanitizeDictationAPIList("dictationAPILanguages", current.DictationAPILanguages, MaxDictationAPILanguages, dictationAPILanguageProblem)
	current.DictationAPIKeywords = sanitizeDictationAPIList("dictationAPIKeywords", current.DictationAPIKeywords, MaxDictationAPIKeywords, dictationAPIKeywordProblem)
	current.DictationAPIPrompt = strings.TrimSpace(current.DictationAPIPrompt)
	if len(current.DictationAPIPrompt) > MaxDictationAPIPromptBytes {
		log.Printf("settings: dictationAPIPrompt over %d bytes, truncating", MaxDictationAPIPromptBytes)
		current.DictationAPIPrompt = truncateRuneSafe(current.DictationAPIPrompt, MaxDictationAPIPromptBytes)
	}
}

func dictationAPILanguageProblem(code string) string {
	if !languageCode.MatchString(code) {
		return "is not a language code such as en or fr"
	}
	return ""
}

// dictationAPIKeywordProblem applies OpenAI's keyword rule: one line,
// with no angle brackets.
func dictationAPIKeywordProblem(keyword string) string {
	switch {
	case strings.ContainsAny(keyword, "<>\r\n"):
		return "must be one line without < or >"
	case utf8.RuneCountInString(keyword) > MaxDictationAPIKeywordRunes:
		return fmt.Sprintf("is longer than %d characters", MaxDictationAPIKeywordRunes)
	}
	return ""
}

func dictationAPIList(field string, entries []string, limit int, problem func(string) string) ([]string, error) {
	var kept []string
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" || slices.Contains(kept, entry) {
			continue
		}
		if why := problem(entry); why != "" {
			return nil, fmt.Errorf("%s entry %q %s", field, entry, why)
		}
		kept = append(kept, entry)
	}
	if len(kept) > limit {
		return nil, fmt.Errorf("%s has %d entries, max is %d", field, len(kept), limit)
	}
	return kept, nil
}

func sanitizeDictationAPIList(field string, entries []string, limit int, problem func(string) string) []string {
	var kept []string
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" || slices.Contains(kept, entry) {
			continue
		}
		if why := problem(entry); why != "" {
			log.Printf("settings: dropping %s entry %q: it %s", field, entry, why)
			continue
		}
		if len(kept) == limit {
			log.Printf("settings: %s has more than %d entries, dropping the rest", field, limit)
			break
		}
		kept = append(kept, entry)
	}
	return kept
}
