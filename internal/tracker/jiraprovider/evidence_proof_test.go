package jiraprovider

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// Proofs of the reviewers' findings on the provider, against the API of
// eb733523b.

func proofScenario(t *testing.T, withTempo bool, edit func(site *trackerfake.JiraSite, start time.Time)) (*scenario, time.Time) {
	t.Helper()
	var start time.Time
	s := newEditedScenario(t, nil, func(site *trackerfake.JiraSite) {
		start = site.Issues[0].Created
		if !withTempo {
			site.Tempo = nil
		}
		edit(site, start)
	})
	return s, start
}

// A2, B4: a batch of 50 issues with ten 22 KiB comments each.
func TestProofOversizedBatch(t *testing.T) {
	var ids []tracker.EvidenceIssue
	s, _ := proofScenario(t, false, func(site *trackerfake.JiraSite, start time.Time) {
		base := site.Issues[0]
		for i := range 50 {
			is := base
			is.ID, is.Key = fmt.Sprint(30001+i), fmt.Sprintf("WEB-%d", 3001+i)
			is.Comments, is.Worklogs, is.Changelog = nil, nil, nil
			for c := range 10 {
				is.Comments = append(is.Comments, trackerfake.JiraComment{ID: fmt.Sprint(900000 + i*10 + c), Author: "acc-user2",
					Body: scenarioDoc(strings.Repeat("x", 22*1024)), Created: start})
			}
			site.Issues = append(site.Issues, is)
			ids = append(ids, tracker.EvidenceIssue{Issue: tracker.IssueID(is.ID)})
		}
	})
	ev, err := s.p.Evidence(s.ctx, tracker.EvidenceQuery{Issues: ids})
	if err != nil || len(ev.Issues) != 50 {
		t.Fatalf("%d issues read, %v", len(ev.Issues), err)
	}
}

// A3: a refused author lookup while Tempo is the time.
func TestProofRefusedAuthorLookup(t *testing.T) {
	s, _ := proofScenario(t, true, func(*trackerfake.JiraSite, time.Time) {})
	if err := s.e.AddFault(trackerfake.Fault{Origin: trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite}, Route: "GET rest/api/3/user",
		Kind: trackerfake.FaultRefuse, Status: 403, Body: []byte(`{"errorMessages":["Forbidden"],"errors":{}}`)}); err != nil {
		t.Fatal(err)
	}
	ev, err := s.p.Evidence(s.ctx, tracker.EvidenceQuery{Issues: []tracker.EvidenceIssue{{Issue: "10101"}}})
	if err != nil || len(ev.Issues) != 1 || len(ev.Issues[0].Comments) != 1 {
		t.Fatalf("evidence %+v, %v", ev, err)
	}
}

// B1: an author Jira names no account type for.
func TestProofTypelessAuthor(t *testing.T) {
	s, _ := proofScenario(t, false, func(site *trackerfake.JiraSite, start time.Time) {
		site.Users = append(site.Users, trackerfake.JiraUser{AccountID: "acc-bot", DisplayName: "Bot", OmitsAccountType: true})
		site.Issues[0].Comments = append(site.Issues[0].Comments, trackerfake.JiraComment{ID: "40009", Author: "acc-bot", Body: scenarioDoc("ok"), Created: start})
	})
	ev, err := s.p.Evidence(s.ctx, tracker.EvidenceQuery{Issues: []tracker.EvidenceIssue{{Issue: "10101"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ie := range ev.Issues {
		for _, c := range ie.Comments {
			if c.Author == "acc-bot" {
				t.Fatalf("a comment of an author of no account type counted: %+v", c)
			}
		}
	}
}

// B6: a Tempo worklog with no UTC start, 21:00 in New York.
func TestProofTempoWorklogWithoutUTCStart(t *testing.T) {
	var day time.Time
	s, _ := proofScenario(t, true, func(site *trackerfake.JiraSite, start time.Time) {
		site.Users = append(site.Users, trackerfake.JiraUser{AccountID: "acc-east", DisplayName: "User 4", TimeZone: "America/New_York"})
		day = start.Truncate(24 * time.Hour)
		site.Tempo.Worklogs = append(site.Tempo.Worklogs, trackerfake.TempoWorklog{ID: 7101, Issue: "10102", Author: "acc-east",
			StartDate: day.Format(time.DateOnly), StartTime: "21:00:00", Spent: 1800, OmitsUTCStart: true})
	})
	ev, err := s.p.Evidence(s.ctx, tracker.EvidenceQuery{Issues: []tracker.EvidenceIssue{{Issue: "10102", WorklogsFrom: day.AddDate(0, 0, 1)}}})
	if err != nil || len(ev.Issues) != 1 || len(ev.Issues[0].Worklogs) != 1 {
		t.Fatalf("evidence %+v, %v", ev, err)
	}
}
