package trackerapp

import (
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/tempo"
)

func TestNormalizeKeepsWhatTheStoreKeeps(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   NewConnection
		want store.NewTrackerConnection
	}{
		"a Jira host": {
			NewConnection{Kind: "jira", Site: "Example.Atlassian.net", Account: " User1@Example.COM ", Label: "  Work "},
			store.NewTrackerConnection{Kind: "jira", Site: "https://example.atlassian.net", Account: "user1@example.com", Label: "Work"},
		},
		"a Jira origin with a slash": {
			NewConnection{Kind: "jira", Site: "https://example.atlassian.net/", Account: "user1@example.com"},
			store.NewTrackerConnection{Kind: "jira", Site: "https://example.atlassian.net", Account: "user1@example.com"},
		},
		"a Jira site off atlassian.net": {
			NewConnection{Kind: "jira", Site: "jira.example.com", Account: "user1@example.com"},
			store.NewTrackerConnection{Kind: "jira", Site: "https://jira.example.com", Account: "user1@example.com"},
		},
		"each Tempo region": {
			NewConnection{Kind: "tempo", Site: "HTTPS://api.us.tempo.io/", ParentID: "p1"},
			store.NewTrackerConnection{Kind: "tempo", Site: tempo.USBaseURL, ParentID: "p1"},
		},
		"the Tempo default": {
			NewConnection{Kind: "tempo", Site: tempo.DefaultBaseURL, ParentID: "p1"},
			store.NewTrackerConnection{Kind: "tempo", Site: tempo.DefaultBaseURL, ParentID: "p1"},
		},
		"no Tempo site, which is the default": {
			NewConnection{Kind: "tempo", Site: " ", ParentID: "p1"},
			store.NewTrackerConnection{Kind: "tempo", Site: tempo.DefaultBaseURL, ParentID: "p1"},
		},
		"a bare Linear key": {
			NewConnection{Kind: "linear", Site: " Example "},
			store.NewTrackerConnection{Kind: "linear", Site: "example"},
		},
		"a Linear address": {
			NewConnection{Kind: "linear", Site: "https://linear.app/example-co/team/WEB/active"},
			store.NewTrackerConnection{Kind: "linear", Site: "example-co"},
		},
		"a Linear address without a scheme": {
			NewConnection{Kind: "linear", Site: "linear.app/example"},
			store.NewTrackerConnection{Kind: "linear", Site: "example"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := normalize(tc.in)
			if err != nil || got != tc.want {
				t.Fatalf("normalize(%+v) = %+v, %v; want %+v", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestNormalizeRefuses(t *testing.T) {
	t.Parallel()
	jira := func(site, account string) NewConnection {
		return NewConnection{Kind: "jira", Site: site, Account: account}
	}
	for name, in := range map[string]NewConnection{
		"an unknown kind":                     {Kind: "github", Site: "example"},
		"a Jira site over http":               jira("http://example.atlassian.net", jiraEmail),
		"a Jira site over ftp":                jira("ftp://example.atlassian.net", jiraEmail),
		"a Jira site with a path":             jira("example.atlassian.net/jira", jiraEmail),
		"a Jira site with a query":            jira("example.atlassian.net?x=1", jiraEmail),
		"a Jira site with a fragment":         jira("example.atlassian.net#top", jiraEmail),
		"a Jira site with a user":             jira("https://user1@example.atlassian.net", jiraEmail),
		"no Jira site":                        jira("", jiraEmail),
		"no Jira account":                     jira("example.atlassian.net", ""),
		"a Jira account that is no address":   jira("example.atlassian.net", "user1"),
		"a Jira account with a name":          jira("example.atlassian.net", "User 1 <user1@example.com>"),
		"a Jira account with a colon":         jira("example.atlassian.net", `"a:b"@example.com`),
		"a Jira account with no domain dot":   jira("example.atlassian.net", "user1@example"),
		"a Jira connection with a parent":     {Kind: "jira", Site: "example.atlassian.net", Account: jiraEmail, ParentID: "p1"},
		"a Tempo site of another host":        {Kind: "tempo", Site: "https://tempo.example.com", ParentID: "p1"},
		"a Tempo site with a path":            {Kind: "tempo", Site: tempo.EUBaseURL + "/4", ParentID: "p1"},
		"a Tempo connection with no parent":   {Kind: "tempo", Site: tempo.EUBaseURL},
		"a Tempo connection with an account":  {Kind: "tempo", Site: tempo.EUBaseURL, ParentID: "p1", Account: jiraEmail},
		"a Linear key with a space":           {Kind: "linear", Site: "my workspace"},
		"a Linear address elsewhere":          {Kind: "linear", Site: "https://example.com/example"},
		"a Linear connection with a parent":   {Kind: "linear", Site: "example", ParentID: "p1"},
		"a Linear connection with an account": {Kind: "linear", Site: "example", Account: jiraEmail},
		"a label too long":                    {Kind: "linear", Site: "example", Label: strings.Repeat("x", maxLabelRunes+1)},
		"a label with a control character":    {Kind: "linear", Site: "example", Label: "a\nb"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, err := normalize(in); !errors.Is(err, ErrInvalid) {
				t.Fatalf("normalize(%+v) = %+v, %v; want ErrInvalid", in, got, err)
			}
		})
	}
}

func TestTokenPages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ kind, parent, want string }{
		{"jira", "", "https://id.atlassian.com/manage-profile/security/api-tokens"},
		{"tempo", jiraSiteAt, jiraSiteAt},
		{"linear", "", "https://linear.app/settings/account/security"},
	} {
		if got, err := tokenPage(tc.kind, tc.parent); err != nil || got != tc.want {
			t.Errorf("tokenPage(%s) = %q, %v; want %q", tc.kind, got, err, tc.want)
		}
	}
	if _, err := tokenPage("github", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("tokenPage of an unknown kind = %v", err)
	}
}

func TestAPIOriginIsTheSiteOrLinearsAPI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ kind, site, want string }{
		{"jira", jiraSiteAt, jiraSiteAt},
		{"tempo", tempo.USBaseURL, tempo.USBaseURL},
		{"linear", "example", linear.DefaultBaseURL},
	} {
		if got, err := APIOrigin(tc.kind, tc.site); err != nil || got != tc.want {
			t.Errorf("APIOrigin(%s, %s) = %q, %v; want %q", tc.kind, tc.site, got, err, tc.want)
		}
	}
	if _, err := APIOrigin("github", "x"); err == nil {
		t.Error("APIOrigin answered an unknown kind")
	}
}
