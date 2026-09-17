package app

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/settings"
)

// The SHARED decision table. Two gates now answer "may this moment interrupt
// this screen": the Go one here, for the backend machine's own screen
// (app_notifications.go), and the TypeScript one a remote browser runs
// against its own settings and its own focus
// (frontend/src/lib/notifications/gate.ts). They are the same decision about
// different screens, and a copy that drifted would be invisible — the failure
// mode is one screen staying quiet where the other spoke, from settings that
// read identically in both settings pages.
//
// So the cases live in ONE file that both sides run, case for case:
// internal/notify/testdata/gate_cases.json. It sits in internal/notify
// because that package owns the wire contract both gates read (Kind,
// SoundEventFor, Send), not in either gate's own package, which would make
// one side the owner and the other the follower.
//
// STRICT ON THE WAY IN, on purpose. A case this runner cannot decode fails
// rather than being skipped, and the TypeScript runner refuses the same
// shapes: a case added for a preference only one side knows about would
// otherwise pass on that side and quietly not run on the other, which is
// exactly the drift the file exists to prevent.
const gateCasesPath = "internal/notify/testdata/gate_cases.json"

// gateCase is one row of the table. Every field is required except the two
// answers, which are null for "raised" and "no cue".
type gateCase struct {
	Name     string          `json:"name"`
	Settings json.RawMessage `json:"settings"`
	Send     gateCaseSend    `json:"send"`
	Screen   gateCaseScreen  `json:"screen"`
	Refusal  *string         `json:"refusal"`
	Cue      *string         `json:"cue"`
}

// gateCaseSend is the part of notify.Send a gate reads. Title and body are
// absent because no gate reads them, and a fixture carrying fields the
// decision ignores invites a reader to believe they matter.
type gateCaseSend struct {
	Kind         string         `json:"kind"`
	HiddenThread bool           `json:"hiddenThread"`
	Target       gateCaseTarget `json:"target"`
}

type gateCaseTarget struct {
	Kind       string `json:"kind"`
	ThreadID   string `json:"threadId"`
	WorkItemID string `json:"workItemId"`
}

// gateCaseScreen is what the screen last said about itself: the transport's
// presence facts on this side (EventBus.LocalScreenPresence), the document's
// own focus and panes on the other.
type gateCaseScreen struct {
	Focused       bool `json:"focused"`
	ThreadVisible bool `json:"threadVisible"`
}

// gateRefusals is every answer a case may expect. A case naming anything else
// fails rather than being read as "raised".
var gateRefusals = map[string]NotificationErrorCode{
	string(NotificationSuppressed):     NotificationSuppressed,
	string(NotificationHiddenThread):   NotificationHiddenThread,
	string(NotificationScreenAttended): NotificationScreenAttended,
}

func loadGateCases(t *testing.T) []gateCase {
	t.Helper()
	raw, err := os.ReadFile(gateCasesPath)
	if err != nil {
		t.Fatalf("read the shared gate table: %v", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var cases []gateCase
	if err := decoder.Decode(&cases); err != nil {
		t.Fatalf("decode %s: %v", gateCasesPath, err)
	}
	if len(cases) == 0 {
		t.Fatalf("%s holds no cases, so neither gate is under test", gateCasesPath)
	}
	return cases
}

// gateCaseSettings applies one case's patch to the shipped defaults. Unknown
// fields are refused: a patch naming a key this build does not have would
// silently test the defaults instead of the preference it names.
func gateCaseSettings(t *testing.T, patch json.RawMessage) settings.Settings {
	t.Helper()
	current := settings.DefaultSettings
	decoder := json.NewDecoder(strings.NewReader(string(patch)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&current); err != nil {
		t.Fatalf("apply settings patch: %v", err)
	}
	return current
}

// TestTheSharedGateTableDecidesTheSameWayHere runs every case through the
// production gate halves in their production order: the per-kind toggle and
// the hidden-thread opt-in (notificationPreferenceRefusalIn), then the
// attended screen (screenAttendedIn), then the cue reading
// (notificationSoundCueIn over notify.SoundEventFor).
//
// It asserts the pieces App.notifyOS composes rather than notifyOS itself,
// because the screen facts arrive there through a transport bus and the cue
// through an event emission — neither of which the remote gate has, and a
// table both sides run has to be about the decision, not the plumbing.
// TestTheGateTableMatchesNotifyOS below closes that seam by running the table
// through the real App for one case.
func TestTheSharedGateTableDecidesTheSameWayHere(t *testing.T) {
	for _, testCase := range loadGateCases(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			current := gateCaseSettings(t, testCase.Settings)
			target := notify.Target{
				Kind:       testCase.Send.Target.Kind,
				ThreadID:   testCase.Send.Target.ThreadID,
				WorkItemID: testCase.Send.Target.WorkItemID,
			}
			// The target shape is validated even though the gate does not
			// read it: a case describing a send no presenter would accept
			// proves nothing about a send one would.
			if err := notify.ValidateTarget(target); err != nil {
				t.Fatalf("case target is not a shape any send may carry: %v", err)
			}
			send := notify.Send{
				ID:           "case",
				Kind:         notify.Kind(testCase.Send.Kind),
				Title:        testCase.Name,
				Target:       target,
				HiddenThread: testCase.Send.HiddenThread,
			}

			got := refusalCodeFor(notificationPreferenceRefusalIn(current, send))
			if got == "" && screenAttendedIn(
				current.NotifyQuietWhen,
				testCase.Screen.Focused,
				testCase.Screen.ThreadVisible,
				target.Kind == notify.TargetThread,
			) {
				got = NotificationScreenAttended
			}
			wantRefusal(t, testCase, got)

			cue := ""
			if event, ok := notify.SoundEventFor(send.Kind); ok {
				if resolved, enabled := notificationSoundCueIn(current, event); enabled {
					cue = resolved
				}
			}
			wantCue(t, testCase, cue)
		})
	}
}

// refusalCodeFor narrows the gate's typed error to the code the table names.
// An error that is not a NotificationError is a fault rather than a refusal
// and must not read as one.
func refusalCodeFor(err error) NotificationErrorCode {
	if err == nil {
		return ""
	}
	var notificationErr *NotificationError
	if errors.As(err, &notificationErr) {
		return notificationErr.Code
	}
	return NotificationErrorCode("unexpected: " + err.Error())
}

func wantRefusal(t *testing.T, testCase gateCase, got NotificationErrorCode) {
	t.Helper()
	if testCase.Refusal == nil {
		if got != "" {
			t.Fatalf("refusal = %q, want the notification raised", got)
		}
		return
	}
	want, known := gateRefusals[*testCase.Refusal]
	if !known {
		t.Fatalf("case expects refusal %q, which this gate has no answer for", *testCase.Refusal)
	}
	if got != want {
		t.Fatalf("refusal = %q, want %q", got, want)
	}
}

func wantCue(t *testing.T, testCase gateCase, got string) {
	t.Helper()
	want := ""
	if testCase.Cue != nil {
		want = *testCase.Cue
		if want == "" {
			t.Fatalf("case expects an empty cue; use null for no cue")
		}
	}
	if got != want {
		t.Fatalf("cue = %q, want %q", got, want)
	}
}

// TestTheGateTableMatchesNotifyOS is the seam the table cannot cover on its
// own: that the pieces above are the ones App.notifyOS actually composes, in
// that order, against the backend machine's own screen.
//
// One case per refusal plus one raised send, driven through the real
// settings service and the real transport presence, is enough — the
// combinations are the table's job, and repeating them here would only prove
// the same switch twice while costing a bus and a store per row.
func TestTheGateTableMatchesNotifyOS(t *testing.T) {
	// One representative case per distinct answer, chosen from the table so a
	// reading removed there stops being asserted here too. The representative
	// has to name a thread: the hidden-thread and attended-screen readings
	// are only reachable for a send that does.
	answers := []string{"", string(NotificationSuppressed), string(NotificationHiddenThread), string(NotificationScreenAttended)}
	chosen := make(map[string]gateCase, len(answers))
	for _, testCase := range loadGateCases(t) {
		if testCase.Send.Target.Kind != notify.TargetThread {
			continue
		}
		answer := ""
		if testCase.Refusal != nil {
			answer = *testCase.Refusal
		}
		if _, taken := chosen[answer]; !taken {
			chosen[answer] = testCase
		}
	}
	for _, answer := range answers {
		testCase, found := chosen[answer]
		if !found {
			t.Fatalf("the table holds no thread-targeted case answering %q, so notifyOS is unasserted for it", answer)
		}
		runGateCaseThroughNotifyOS(t, testCase)
	}
}

func runGateCaseThroughNotifyOS(t *testing.T, testCase gateCase) {
	t.Helper()
	t.Run(testCase.Name, func(t *testing.T) {
		app, _, subscriber := attendedApp(t)
		current := gateCaseSettings(t, testCase.Settings)
		if _, err := app.settings.BackendScreen().Update(map[string]any{
			"notificationsEnabled":    current.NotificationsEnabled,
			"notifyTurnComplete":      current.NotifyTurnComplete,
			"notifyApprovalNeeded":    current.NotifyApprovalNeeded,
			"notifyError":             current.NotifyError,
			"notifyProviderSignedOut": current.NotifyProviderSignedOut,
			"notifyWorkflowAttention": current.NotifyWorkflowAttention,
			"notifyAppUpdate":         current.NotifyAppUpdate,
			"notifyHiddenThreads":     current.NotifyHiddenThreads,
			"notifyQuietWhen":         current.NotifyQuietWhen,
		}); err != nil {
			t.Fatalf("write the case's preferences onto the backend screen: %v", err)
		}
		threadID := testCase.Send.Target.ThreadID
		visible := []string(nil)
		if testCase.Screen.ThreadVisible {
			visible = []string{threadID}
		}
		subscriber.SetPresence(testCase.Screen.Focused, visible)

		err := app.notifyOS(notify.Send{
			ID:           "thread:" + threadID,
			Kind:         notify.Kind(testCase.Send.Kind),
			Title:        testCase.Name,
			Target:       notify.Target{Kind: notify.TargetThread, ThreadID: threadID},
			HiddenThread: testCase.Send.HiddenThread,
		})
		wantRefusal(t, testCase, refusalCodeFor(err))
	})
}
