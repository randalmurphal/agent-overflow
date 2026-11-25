package trackerapp

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/tempo"
)

// ErrInvalid matches the error of an input this package refuses before
// it reaches the store: a site, account, parent or label a connection of
// its kind cannot have. Each such error is an *InvalidError.
var ErrInvalid = errors.New("trackerapp: invalid tracker connection")

// InvalidError is an input this package refuses before it reaches the
// store. Reason says what is wrong and what it must be, a sentence a
// person can act on; it matches ErrInvalid.
type InvalidError struct {
	Reason string
}

func (e *InvalidError) Error() string { return ErrInvalid.Error() + ": " + e.Reason }

// Is matches ErrInvalid.
func (e *InvalidError) Is(target error) bool { return target == ErrInvalid }

// invalid is an *InvalidError with its reason formatted from format and
// args.
func invalid(format string, args ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, args...)}
}

// NewConnection is what Add needs. Site is the Jira site (with or without
// https:// and a trailing slash), one of the tempo client's base URLs
// (none for tempo.DefaultBaseURL), or
// the Linear workspace's URL key (bare or as a linear.app address).
// Account is the Jira account's email and empty for the others; ParentID
// names the Jira connection a Tempo connection belongs to.
type NewConnection struct {
	Kind     string
	Site     string
	Account  string
	ParentID string
	Label    string
}

// maxLabelRunes bounds a connection's label.
const maxLabelRunes = 100

// isLinearURLKey reports the shape of a Linear workspace URL key: 1 to 64
// lower-case letters, digits and hyphens, not starting with a hyphen.
// Written out because a compiled expression costs every process that
// links the package its compilation at start.
func isLinearURLKey(s string) bool {
	if len(s) < 1 || len(s) > 64 || s[0] == '-' {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// tempoSites are the base URLs a Tempo connection's site may be: the
// tempo client's, one per region.
var tempoSites = []string{tempo.DefaultBaseURL, tempo.EUBaseURL, tempo.USBaseURL}

// normalize checks c for its kind and returns it as the store keeps it.
// CreatedAt is the caller's.
func normalize(c NewConnection) (store.NewTrackerConnection, error) {
	out := store.NewTrackerConnection{Kind: c.Kind, ParentID: strings.TrimSpace(c.ParentID)}
	label, err := normalizeLabel(c.Label)
	if err != nil {
		return store.NewTrackerConnection{}, err
	}
	out.Label = label
	account := strings.TrimSpace(c.Account)
	switch c.Kind {
	case store.TrackerKindJira:
		out.Site, err = jiraSite(c.Site)
		if err == nil {
			out.Account, err = jiraAccount(account)
		}
	case store.TrackerKindTempo:
		out.Site, err = tempoSite(c.Site)
	case store.TrackerKindLinear:
		out.Site, err = linearSite(c.Site)
	default:
		return store.NewTrackerConnection{}, invalid("unknown kind %q; use jira, tempo or linear", c.Kind)
	}
	if err != nil {
		return store.NewTrackerConnection{}, err
	}
	switch {
	case c.Kind != store.TrackerKindJira && account != "":
		return store.NewTrackerConnection{}, invalid("only a Jira connection names an account")
	case c.Kind == store.TrackerKindTempo && out.ParentID == "":
		return store.NewTrackerConnection{}, invalid("a Tempo connection names the Jira connection it belongs to")
	case c.Kind != store.TrackerKindTempo && out.ParentID != "":
		return store.NewTrackerConnection{}, invalid("only a Tempo connection belongs to another connection")
	}
	return out, nil
}

// normalizeLabel trims a label and refuses one that is too long or holds
// a control character.
func normalizeLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	switch {
	case !utf8.ValidString(label):
		return "", invalid("the label is not valid text")
	case utf8.RuneCountInString(label) > maxLabelRunes:
		return "", invalid("the label is longer than %d characters", maxLabelRunes)
	case strings.ContainsFunc(label, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "", invalid("the label holds a control character")
	}
	return label, nil
}

// jiraSite is the site's https origin with its host lowercased. Input may
// leave out the scheme and end in a slash; a path, query, fragment, user
// info or a scheme other than https is refused.
func jiraSite(site string) (string, error) {
	site = strings.TrimSpace(site)
	if !strings.Contains(site, "://") {
		site = "https://" + site
	}
	u, err := url.Parse(site)
	refused := func(why string) error {
		return invalid("the Jira site must be an address such as https://example.atlassian.net: %s", why)
	}
	switch {
	case err != nil:
		return "", refused("it is not an address")
	case u.Scheme != "https":
		return "", refused("it must use https")
	case u.Host == "" || u.Opaque != "":
		return "", refused("it names no host")
	case u.User != nil:
		return "", refused("it carries a user name")
	case u.Path != "" && u.Path != "/":
		return "", refused("it has a path")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(site, "#"):
		return "", refused("it has a query or fragment")
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// jiraAccount is a Jira account's email, lowercased, refused unless it
// is one bare address with a dotted domain. An address that parses back
// to itself has no quoted part, so it holds no colon for Basic
// authentication to trip on.
func jiraAccount(account string) (string, error) {
	account = strings.ToLower(account)
	parsed, err := mail.ParseAddress(account)
	if err != nil || parsed.Name != "" || parsed.Address != account ||
		!strings.Contains(account[strings.LastIndex(account, "@")+1:], ".") {
		return "", invalid("the Jira account must be an email address such as user1@example.com")
	}
	return account, nil
}

// tempoSite is one of the tempo client's base URLs, compared without
// case or a trailing slash; none given is tempo.DefaultBaseURL, which
// serves every region.
func tempoSite(site string) (string, error) {
	site = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(site)), "/")
	if site == "" {
		return tempo.DefaultBaseURL, nil
	}
	for _, known := range tempoSites {
		if site == known {
			return known, nil
		}
	}
	return "", invalid("the Tempo site must be one of %s", strings.Join(tempoSites, ", "))
}

// linearSite is a workspace URL key, lowercased, given bare or as a
// linear.app address whose first path segment is the key.
func linearSite(site string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(site))
	key = strings.TrimPrefix(key, "https://")
	if rest, ok := strings.CutPrefix(key, "linear.app/"); ok {
		key, _, _ = strings.Cut(rest, "/")
	}
	if !isLinearURLKey(key) {
		return "", invalid("the Linear workspace must be its URL key, such as example, or an address such as https://linear.app/example")
	}
	return key, nil
}

// Origin maps a connection's kind and site to the origin its client
// connects to. Its error is shown to the person as the reason the
// connection cannot be used, so it holds no secret.
type Origin func(kind, site string) (string, error)

// APIOrigin is the production Origin: the site for Jira and Tempo, and
// Linear's one API origin for every workspace.
func APIOrigin(kind, site string) (string, error) {
	switch kind {
	case store.TrackerKindJira, store.TrackerKindTempo:
		return site, nil
	case store.TrackerKindLinear:
		return linear.DefaultBaseURL, nil
	}
	return "", fmt.Errorf("unknown tracker kind %q", kind)
}

// Token pages: the vendors' documented addresses, cited beside each.
const (
	// jiraTokenPage is the Atlassian account's API token page
	// (https://support.atlassian.com/atlassian-account/docs/manage-api-tokens-for-your-atlassian-account/).
	jiraTokenPage = "https://id.atlassian.com/manage-profile/security/api-tokens"
	// linearTokenPage is where personal API keys are made
	// (https://linear.app/developers/graphql and
	// https://linear.app/docs/api-and-webhooks).
	linearTokenPage = "https://linear.app/settings/account/security"
)

// tokenPage is where the person makes the token of a connection of kind.
// Tempo documents no address for its token page, only the steps inside
// the Jira site: Settings in the Tempo sidebar, then API Integration under
// Data Access, then New Token
// (https://help.tempo.io/timesheets/latest/using-rest-api-integrations).
// A Tempo connection's page is therefore its Jira site, parentSite, an
// origin as normalize stores it, and the surface shows those steps.
func tokenPage(kind, parentSite string) (string, error) {
	switch kind {
	case store.TrackerKindJira:
		return jiraTokenPage, nil
	case store.TrackerKindTempo:
		return parentSite, nil
	case store.TrackerKindLinear:
		return linearTokenPage, nil
	}
	return "", invalid("unknown kind %q", kind)
}
