package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/trackerapi"
)

// Each error a tracker call can end with lands in its kind, with a
// message built from its type and status, never from its text.
func TestClassifyFailureSortsEveryKind(t *testing.T) {
	t.Parallel()
	label := keyFileLabel("jira")
	until := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	secret := "tracker text " + jiraToken
	wrap := func(err error) error { return fmt.Errorf("jira: who the credential belongs to: %w", err) }
	for name, tc := range map[string]struct {
		kind    string
		err     error
		want    FailureKind
		message string
	}{
		"a missing key file": {"jira", wrap(&trackerapi.NotSentError{Err: &KeyFileError{Path: "/k", Err: label.ErrMissing()}}),
			FailureSetup, "there is no Jira API token file"},
		"an empty key file": {"jira", wrap(&trackerapi.NotSentError{Err: &KeyFileError{Path: "/k", Err: label.ErrEmpty()}}),
			FailureSetup, "the Jira API token file is empty"},
		"a refused key file": {"jira", &KeyFileError{Path: "/k", Err: &keyfile.RefusedError{Reason: "it is a symlink"}},
			FailureSetup, "it is a symlink"},
		"a refused token": {"jira", wrap(&trackerapi.AuthError{Tracker: "jira", Status: 401, Message: secret}),
			FailureSetup, "Jira refused the token in the key file"},
		"a refused token whose file then went missing": {"jira", &trackerapi.AuthError{Tracker: "jira", Status: 401, Err: &KeyFileError{Err: label.ErrMissing()}},
			FailureSetup, "there is no Jira API token file"},
		"a refused Tempo token": {"tempo", &trackerapi.AuthError{Tracker: "tempo", Status: 401},
			FailureSetup, "check that the region"},
		"another workspace": {"linear", &trackerapi.NotSentError{Err: &linear.WrongWorkspaceError{Want: "example", Got: "Other"}},
			FailureSetup, "belongs to the workspace other, not example; make a key in example"},
		"another workspace that is no URL key": {"linear", &linear.WrongWorkspaceError{Want: "example", Got: secret},
			FailureSetup, "belongs to another workspace than example"},
		"a credential replaced under the answer": {"linear", fmt.Errorf("linear: viewer: %w", trackerapi.ErrCredentialChanged),
			FailureTransient, "as the token in the key file was being replaced; try again"},
		"a Tempo credential that changed twice during one read": {"tempo",
			fmt.Errorf("tempo: the credential changed twice during one read: %w", trackerapi.ErrCredentialChanged),
			FailureTransient, "Tempo answered as the token in the key file was being replaced; try again"},
		"a Tempo credential that changed between pages": {"tempo",
			fmt.Errorf("tempo: the credential changed between the pages of /4/worklogs, which were handed over: %w", trackerapi.ErrCredentialChanged),
			FailureTransient, "Tempo answered as the token in the key file was being replaced; try again"},
		"a Jira credential that changed twice during one walk": {"jira",
			wrap(fmt.Errorf("jira: the credential changed twice during one read: %w", trackerapi.ErrCredentialChanged)),
			FailureTransient, "Jira answered as the token in the key file was being replaced; try again"},
		"no client": {"jira", &ClientError{Kind: "jira", Err: errors.New("no fake tracker")},
			FailureSetup, "cannot connect to Jira from this computer: no fake tracker"},
		"a rate limit": {"jira", wrap(&trackerapi.RateLimitedError{Tracker: "jira", Status: 429, Until: until}),
			FailureRateLimited, "Jira is limiting requests until 2026-11-19T10:00:00Z"},
		"the gate's refusal": {"linear", &trackerapi.NotSentError{Err: &trackerapi.RateLimitedError{Tracker: "linear", Until: until}},
			FailureRateLimited, "Linear is limiting requests"},
		"a 403": {"jira", wrap(&trackerapi.StatusError{Tracker: "jira", Status: 403, Message: secret, Body: secret}),
			FailureForbidden, "Jira refused access (HTTP 403)"},
		"a refused rule": {"jira", fmt.Errorf("%w: %w", tracker.ErrRuleRefused, &trackerapi.StatusError{Tracker: "jira", Status: 400, Body: secret}),
			FailureTracker, "Jira refused the query for this board's issues (HTTP 400). Refresh to try again now. Agent Overflow tries again by itself later while the board is open"},
		"a 400": {"jira", wrap(&trackerapi.StatusError{Tracker: "jira", Status: 400, Body: secret}),
			FailureTracker, "Jira answered HTTP 400"},
		"a 500": {"tempo", &trackerapi.StatusError{Tracker: "tempo", Status: 500, Body: secret},
			FailureTracker, "Tempo answered HTTP 500"},
		"a body too large": {"jira", &trackerapi.BodyTooLargeError{Tracker: "jira", Limit: 1},
			FailureTracker, "larger than Agent Overflow reads"},
		"a reset": {"jira", &trackerapi.TransientError{Err: errors.New(secret)},
			FailureTransient, "Jira could not be reached"},
		"a dial failure": {"linear", &trackerapi.NotSentError{Err: errors.New(secret)},
			FailureTransient, "Linear could not be reached"},
		"the verification's bound": {"jira", fmt.Errorf("%w", context.DeadlineExceeded),
			FailureTransient, "Jira could not be reached"},
		"a refused input": {"jira", trackerapi.InvalidInput(errors.New(secret)),
			FailureTracker, "Jira answered in a way"},
		"anything else": {"linear", errors.New(secret),
			FailureTracker, "Linear answered in a way"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyFailure(tc.kind, tc.err)
			if got.Kind != tc.want || !strings.Contains(got.Message, tc.message) {
				t.Fatalf("ClassifyFailure = %+v, want %s containing %q", got, tc.want, tc.message)
			}
			if strings.Contains(got.Message, jiraToken) || strings.Contains(got.Message, "tracker text") {
				t.Fatalf("the message %q quotes the error's text", got.Message)
			}
		})
	}
}
