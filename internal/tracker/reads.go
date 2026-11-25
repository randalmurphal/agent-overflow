package tracker

import (
	"errors"
	"io"
	"strings"
	"unicode"
)

// ErrUnsupported is matched, through errors.Is, by the error of a read
// the connection's tracker cannot serve (see Capabilities).
var ErrUnsupported = errors.New("tracker: not supported by this tracker")

// MaxLookupKeys is the most keys one Lookup takes. A provider accepts any
// number up to it and splits them into the requests its tracker takes.
const MaxLookupKeys = 500

// maxKeyLen bounds one key's length; a tracker key is far shorter.
const maxKeyLen = 64

// CheckLookupKeys refuses a lookup of no keys, of more than
// MaxLookupKeys, of a key that is empty, longer than 64 bytes or holds a
// space or control character, or of one key twice (case ignored, as both
// trackers ignore it). Keys come from branch names, pull request titles
// and agents, so a provider runs it before any request.
func CheckLookupKeys(keys []string) error {
	if len(keys) == 0 || len(keys) > MaxLookupKeys {
		return invalidf("a lookup takes 1 to %d keys, not %d", MaxLookupKeys, len(keys))
	}
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k == "" || len(k) > maxKeyLen || strings.IndexFunc(k, notKeyRune) >= 0 {
			return invalidf("%q is not an issue key", k)
		}
		folded := strings.ToUpper(k)
		if seen[folded] {
			return invalidf("key %q is named twice", k)
		}
		seen[folded] = true
	}
	return nil
}

func notKeyRune(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }

// KeyLookup is what a Lookup found. Found has one match for each key
// that names an issue the credential can see, in the order asked;
// Unmatched are the other keys as given.
type KeyLookup struct {
	Found     []KeyMatch
	Unmatched []string
	Catalog
}

// KeyMatch is one key and the issue it names. Card.Key differs from Key
// when the tracker answered a key the issue had before it moved.
type KeyMatch struct {
	Key  string
	Card Card
}

// MaxSearchPage is the most cards one search page holds.
const MaxSearchPage = 50

// maxSearchText bounds the free text of a search.
const maxSearchText = 500

// SearchQuery is a search for issues by free text, optionally within one
// project. Text is what a person or an agent typed and is never read as
// a tracker query language. Limit bounds the page, 1 to MaxSearchPage;
// zero means MaxSearchPage. Page is the token the previous page of the
// same query returned; empty asks for the first.
type SearchQuery struct {
	Text    string
	Project ProjectID
	Limit   int
	Page    PageToken
}

// Check refuses a search with neither text nor a project, text longer
// than 500 bytes or holding a control character, or a limit outside 0 to
// MaxSearchPage.
func (q SearchQuery) Check() error {
	text := strings.TrimSpace(q.Text)
	if text == "" && q.Project == "" {
		return invalidf("a search needs text or a project")
	}
	if len(q.Text) > maxSearchText || strings.IndexFunc(q.Text, unicode.IsControl) >= 0 {
		return invalidf("search text must be at most %d bytes with no control characters", maxSearchText)
	}
	if q.Limit < 0 || q.Limit > MaxSearchPage {
		return invalidf("a search page holds 1 to %d cards, not %d", MaxSearchPage, q.Limit)
	}
	return nil
}

// SearchPage is one page of a search's cards, in the order the tracker
// ranks them, with the users and statuses they name. Next is empty on the
// last page.
type SearchPage struct {
	Cards []Card
	Next  PageToken
	Catalog
}

// ChoiceList is the values a pick list offers for typed text. More
// reports that the tracker may hold matches the list leaves out, so the
// absence of a value from Choices is not proof that it does not exist: a
// picker says so and the person types more. A provider sets More whenever
// it cannot show that the list is whole.
type ChoiceList struct {
	Choices []Choice
	More    bool
}

// UserList is the users a pick list offers for typed text, with More as
// ChoiceList has it.
type UserList struct {
	Users []User
	More  bool
}

// DownloadInput streams one attachment of one issue to To. To is written
// as the content arrives: after a failure it may hold part of the file,
// which the caller discards.
type DownloadInput struct {
	Issue      IssueID
	Attachment AttachmentID
	To         io.Writer
}

// Check refuses a download that names no issue, attachment or writer.
func (in DownloadInput) Check() error {
	if in.Issue == "" || in.Attachment == "" || in.To == nil {
		return invalidf("a download needs an issue, an attachment and a writer")
	}
	return nil
}
