package jiraprovider

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// evidenceScenario is the scenario's site with WEB-1 holding comments of
// User 2, of an app and of a customer, and 25 worklogs of User 1 an hour
// apart from start, more than Jira embeds, and WEB-2 two worklogs of User
// 1, at start and 11 hours after. With Tempo, Tempo holds the scenario's
// worklog of User 1 on WEB-1 at start, one of User 2 on WEB-1 a day
// later, two on WEB-2, at start and two days later, and one on WEB-3 of
// User 3, whose clock is UTC-8, at 18:30 on start's day, the next day in
// UTC; requests records
// the routes each request reached.
func evidenceScenario(t *testing.T, withTempo bool) (s *scenario, start time.Time, requests func() []string) {
	t.Helper()
	var mu sync.Mutex
	var routes []string
	s = newEditedScenario(t, func(r trackerfake.Request) {
		mu.Lock()
		defer mu.Unlock()
		routes = append(routes, r.Route)
	}, func(site *trackerfake.JiraSite) {
		site.Users = append(site.Users, trackerfake.JiraUser{AccountID: "acc-app", DisplayName: "Automation", AccountType: "app"},
			trackerfake.JiraUser{AccountID: "acc-customer", DisplayName: "Customer 1", AccountType: "customer"},
			trackerfake.JiraUser{AccountID: "acc-west", DisplayName: "User 3", TimeZone: "America/Los_Angeles"})
		is := &site.Issues[0]
		start = is.Created
		if !withTempo {
			site.Tempo = nil
		} else {
			for i, w := range []struct {
				issue, author string
				at            time.Time
			}{{"10101", "acc-user2", start.Add(24 * time.Hour)}, {"10102", "acc-user1", start}, {"10102", "acc-user2", start.Add(48 * time.Hour)},
				{"10103", "acc-west", start.Add(9*time.Hour + 30*time.Minute)}} {
				site.Tempo.Worklogs = append(site.Tempo.Worklogs, trackerfake.TempoWorklog{ID: int64(7002 + i), Issue: w.issue, Author: w.author,
					StartDate: w.at.Format(time.DateOnly), StartTime: w.at.Format(time.TimeOnly), Spent: 1800})
			}
		}
		is.Comments = append(is.Comments, trackerfake.JiraComment{ID: "40002", Author: "acc-app", Body: scenarioDoc("deployed"), Created: start},
			trackerfake.JiraComment{ID: "40003", Author: "acc-customer", Body: scenarioDoc("thanks"), Created: start})
		for i := range 25 {
			is.Worklogs = append(is.Worklogs, trackerfake.JiraWorklog{ID: fmt.Sprint(50001 + i), Author: "acc-user1", Started: start.Add(time.Duration(i) * time.Hour),
				TimeSpent: 600})
		}
		two := &site.Issues[1]
		for i, at := range []time.Time{start, start.Add(11 * time.Hour)} {
			two.Worklogs = append(two.Worklogs, trackerfake.JiraWorklog{ID: fmt.Sprint(60001 + i), Author: "acc-user1", Started: at, TimeSpent: 600})
		}
	})
	return s, start, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(routes)
	}
}

// evidenceOf is ev as one line a source: "issue/c" or "issue/w", then
// each act as "ID author", with its start and time spent for a worklog,
// or "!" and the reason of a source that failed; the authors named last.
func evidenceOf(ev tracker.Evidence) string {
	noun := map[tracker.EvidenceSource]string{tracker.EvidenceComments: "c", tracker.EvidenceWorklogs: "w"}
	var lines []string
	for _, se := range ev.Read {
		line := string(se.Issue) + "/" + noun[se.Source] + ":"
		for _, a := range se.Acts {
			line += " " + string(a.ID) + " " + string(a.Author)
			if se.Source == tracker.EvidenceWorklogs {
				line += " " + a.At.UTC().Format(time.RFC3339) + " " + a.Spent.String()
			}
		}
		lines = append(lines, line)
	}
	for _, f := range ev.Failed {
		lines = append(lines, string(f.Issue)+"/"+noun[f.Source]+" ! "+f.Err.Error())
	}
	var users []string
	for _, u := range ev.Users {
		users = append(users, string(u.ID)+":"+u.Name)
	}
	return strings.Join(lines, "\n") + "\nusers " + strings.Join(users, " ")
}

// read reads the evidence of issues, every source of each unless an
// issue says otherwise, and fails the test on a failure of the read.
func readEvidence(t *testing.T, s *scenario, issues ...tracker.EvidenceIssue) tracker.Evidence {
	t.Helper()
	ev, err := s.p.Evidence(s.ctx, tracker.EvidenceQuery{Issues: issues})
	if err != nil {
		t.Fatalf("evidence %s, %v", evidenceOf(ev), err)
	}
	return ev
}

// both asks every source of issue, worklogs from from.
func both(issue tracker.IssueID, from time.Time) tracker.EvidenceIssue {
	return tracker.EvidenceIssue{Issue: issue, Comments: true, Worklogs: true, WorklogsFrom: from}
}

// wantEvidence fails the test when ev is not want.
func wantEvidence(t *testing.T, ev tracker.Evidence, want string) {
	t.Helper()
	if got := evidenceOf(ev); got != want {
		t.Fatalf("evidence\n%s\nwant\n%s", got, want)
	}
}

// Evidence reads an issue's comments and its worklogs while Jira's own are
// the time, from the issue's bound: the worklogs Jira did not embed
// through one read of the issue's worklog list from the bound, and an
// embedded list whole, kept from the bound. An app's and a customer's
// comments are left out, the authors named. An issue Jira does not return
// fails each source asked of it alone, and a source not asked is not read.
func TestEvidenceReadsCommentsAndJirasWorklogs(t *testing.T) {
	s, start, requests := evidenceScenario(t, false)
	if !s.p.Capabilities().Evidence {
		t.Fatal("Jira reads no evidence")
	}
	from := start.Add(10 * time.Hour)
	ev := readEvidence(t, s, both("10101", from), tracker.EvidenceIssue{Issue: "10102", Worklogs: true, WorklogsFrom: from}, both("99999", time.Time{}))
	var web1 []string
	for i := range 15 {
		web1 = append(web1, fmt.Sprintf("%d acc-user1 %s 10m0s", 50011+i, from.Add(time.Duration(i)*time.Hour).UTC().Format(time.RFC3339)))
	}
	wantEvidence(t, ev, "10101/c: 40001 acc-user2\n"+
		"10101/w: "+strings.Join(web1, " ")+"\n"+
		"10102/w: 60002 acc-user1 "+start.Add(11*time.Hour).UTC().Format(time.RFC3339)+" 10m0s\n"+
		"99999/c ! Jira did not return issue 99999: it was deleted, or this account cannot see it\n"+
		"99999/w ! Jira did not return issue 99999: it was deleted, or this account cannot see it\n"+
		"users acc-user2:User 2 acc-user1:User 1")
	if got := fmt.Sprint(requests()); got != "[POST rest/api/3/issue/bulkfetch GET rest/api/3/issue/{issue}/worklog]" {
		t.Errorf("requests %s", got)
	}
}

// While Tempo is the time, Evidence reads every author's Tempo worklogs
// of the issues and none of Jira's: one Tempo search for the issues of
// each bound, from the day before it, each worklog kept from its bound,
// a worklog dated on its author's clock included, and each author named
// by Jira. A worklog before its bound is dropped before its author is
// looked up.
func TestEvidenceReadsTempoWorklogsWhileTempoIsTheTime(t *testing.T) {
	s, start, requests := evidenceScenario(t, true)
	from := start.Add(10 * time.Hour)
	ev := readEvidence(t, s, both("10101", time.Time{}), both("10102", from), both("10103", start.Add(17*time.Hour)))
	day := func(d time.Duration) string { return start.Add(d).UTC().Format(time.RFC3339) }
	wantEvidence(t, ev, "10101/c: 40001 acc-user2\n10102/c:\n10103/c:\n"+
		"10101/w: 7001 acc-user1 "+day(0)+" 1h0m0s 7002 acc-user2 "+day(24*time.Hour)+" 30m0s\n"+
		"10102/w: 7004 acc-user2 "+day(48*time.Hour)+" 30m0s\n"+
		"10103/w: 7005 acc-west "+day(17*time.Hour+30*time.Minute)+" 30m0s\n"+
		"users acc-user2:User 2 acc-user1:User 1 acc-west:User 3")
	want := "[POST rest/api/3/issue/bulkfetch POST 4/worklogs/search GET rest/api/3/user GET rest/api/3/user POST 4/worklogs/search" +
		" POST 4/worklogs/search GET rest/api/3/user]"
	if got := fmt.Sprint(requests()); got != want {
		t.Errorf("requests %s; want %s", got, want)
	}
}

// A batch whose answer is over the transport's bound is read again in
// halves, so every issue of it is read in bulk; an issue whose one
// comment is over the bound alone fails its comments, its worklogs and
// the other issues read.
func TestEvidenceReadsAnAnswerOverTheBoundInParts(t *testing.T) {
	var ids []tracker.EvidenceIssue
	var start time.Time
	var mu sync.Mutex
	routes := map[string]int{}
	s := newEditedScenario(t, func(r trackerfake.Request) {
		mu.Lock()
		defer mu.Unlock()
		routes[r.Route]++
	}, func(site *trackerfake.JiraSite) {
		site.Tempo = nil
		base := site.Issues[0]
		start = base.Created
		for i := range 50 {
			is := base
			is.ID, is.Key = fmt.Sprint(30001+i), fmt.Sprintf("WEB-%d", 3001+i)
			is.Comments, is.Worklogs, is.Changelog = nil, nil, nil
			size := 22 * 1024
			if i == 1 {
				size = 9 << 20
			}
			for c := range 10 {
				is.Comments = append(is.Comments, trackerfake.JiraComment{ID: fmt.Sprint(900000 + i*10 + c), Author: "acc-user2",
					Body: scenarioDoc(strings.Repeat("x", size)), Created: start})
				if i == 1 {
					break
				}
			}
			is.Worklogs = []trackerfake.JiraWorklog{{ID: fmt.Sprint(800000 + i), Author: "acc-user1", Started: start, TimeSpent: 60}}
			site.Issues = append(site.Issues, is)
			ids = append(ids, both(tracker.IssueID(is.ID), time.Time{}))
		}
	})
	ev := readEvidence(t, s, ids...)
	read := map[string]int{}
	for _, se := range ev.Read {
		read[fmt.Sprint(se.Source)] += len(se.Acts)
	}
	if fmt.Sprint(read) != "map[1:490 2:50]" || len(ev.Failed) != 1 || ev.Failed[0].Issue != "30002" || ev.Failed[0].Source != tracker.EvidenceComments ||
		!strings.Contains(ev.Failed[0].Err.Error(), "over its") {
		t.Fatalf("acts read by source %v, failed %+v", read, ev.Failed)
	}
	// Halving reads the batch in 11 bulk fetches; the issue over the bound
	// alone is the only one whose lists are read by page.
	mu.Lock()
	defer mu.Unlock()
	if routes["POST rest/api/3/issue/bulkfetch"] != 11 || routes["GET rest/api/3/issue/{issue}/worklog"] != 1 {
		t.Fatalf("requests %v", routes)
	}
}

// Jira's issueErrors fail each source of their issues alone, with Jira's
// message, and the other issues are read.
func TestEvidenceFailsAnIssueJiraCouldNotRead(t *testing.T) {
	s := newEditedScenario(t, nil, func(site *trackerfake.JiraSite) {
		site.Tempo = nil
		site.Issues[1].BulkError = "Jira is busy"
	})
	ev := readEvidence(t, s, both("10101", time.Time{}), both("10102", time.Time{}))
	if len(ev.Failed) != 2 || ev.Failed[0].Issue != "10102" || !strings.Contains(ev.Failed[0].Err.Error(), "Jira could not read issue 10102: Jira is busy") ||
		len(ev.Read) != 2 || ev.Read[0].Issue != "10101" || ev.Read[1].Issue != "10101" {
		t.Fatalf("evidence %s", evidenceOf(ev))
	}
}

// An act whose author Jira names with no account type fails its issue's
// source alone, naming it, and is not counted; the issue's other source
// is read.
func TestEvidenceFailsASourceWithAnAuthorOfNoAccountType(t *testing.T) {
	s := newEditedScenario(t, nil, func(site *trackerfake.JiraSite) {
		site.Tempo = nil
		site.Users = append(site.Users, trackerfake.JiraUser{AccountID: "acc-bot", DisplayName: "Bot", OmitsAccountType: true})
		is := &site.Issues[0]
		is.Comments = append(is.Comments, trackerfake.JiraComment{ID: "40009", Author: "acc-bot", Body: scenarioDoc("ok"), Created: is.Created})
	})
	ev := readEvidence(t, s, both("10101", time.Time{}))
	if len(ev.Failed) != 1 || ev.Failed[0].Source != tracker.EvidenceComments ||
		ev.Failed[0].Err.Error() != "Jira named no account type for acc-bot, the author of comment 40009 of issue 10101, so whether a person made it is not known" ||
		len(ev.Read) != 1 || ev.Read[0].Source != tracker.EvidenceWorklogs {
		t.Fatalf("evidence %s", evidenceOf(ev))
	}
	for _, u := range ev.Users {
		if u.ID == "acc-bot" {
			t.Fatalf("the author of no account type was named: %s", evidenceOf(ev))
		}
	}
}

// While Tempo is the time, an author lookup Jira refuses fails the
// worklogs of each issue the author logged on, naming the permission a
// lookup needs; the issues' comments are read, and a worklog before its
// bound looks up no one.
func TestEvidenceFailsTheWorklogsOfAnAuthorJiraWithholds(t *testing.T) {
	s, start, requests := evidenceScenario(t, true)
	if err := s.e.AddFault(trackerfake.Fault{Origin: trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite}, Route: "GET rest/api/3/user",
		Kind: trackerfake.FaultRefuse, Status: 403, Body: []byte(`{"errorMessages":["Forbidden"],"errors":{}}`)}); err != nil {
		t.Fatal(err)
	}
	ev := readEvidence(t, s, both("10101", time.Time{}), both("10102", start.Add(72*time.Hour)))
	got := evidenceOf(ev)
	if !strings.HasPrefix(got, "10101/c: 40001 acc-user2\n10102/c:\n10102/w:\n10101/w ! Jira did not show who logged Tempo worklog 7001 on issue 10101: "+
		"looking up a person needs Jira's Browse users and groups permission: ") || strings.Count(got, " ! ") != 1 {
		t.Fatalf("evidence %s", got)
	}
	if n := strings.Count(fmt.Sprint(requests()), "GET rest/api/3/user"); n != 1 {
		t.Fatalf("%d lookups: %v", n, requests())
	}
}

// A Tempo worklog that names no UTC start is placed by its date and time
// on its author's clock (21:00 in New York in November is 02:00 UTC the
// next day, after a bound at that day's start), and fails its issue's worklogs when Jira shows
// no time zone for the author.
func TestEvidencePlacesATempoWorklogWithoutAUTCStart(t *testing.T) {
	var day time.Time
	s := newEditedScenario(t, nil, func(site *trackerfake.JiraSite) {
		site.Users = append(site.Users, trackerfake.JiraUser{AccountID: "acc-east", DisplayName: "User 4", TimeZone: "America/New_York"},
			trackerfake.JiraUser{AccountID: "acc-hidden", DisplayName: "User 5", TimeZone: "Europe/Berlin", HidesTimeZone: true})
		day = site.Issues[0].Created.Truncate(24 * time.Hour)
		site.Tempo.Worklogs = append(site.Tempo.Worklogs,
			trackerfake.TempoWorklog{ID: 7101, Issue: "10102", Author: "acc-east", StartDate: day.Format(time.DateOnly), StartTime: "21:00:00", Spent: 1800, OmitsUTCStart: true},
			trackerfake.TempoWorklog{ID: 7102, Issue: "10103", Author: "acc-hidden", StartDate: day.Format(time.DateOnly), StartTime: "09:00:00", Spent: 1800, OmitsUTCStart: true})
	})
	ev := readEvidence(t, s, tracker.EvidenceIssue{Issue: "10102", Worklogs: true, WorklogsFrom: day.AddDate(0, 0, 1)},
		tracker.EvidenceIssue{Issue: "10103", Worklogs: true})
	wantEvidence(t, ev, "10102/w: 7101 acc-east "+day.Add(26*time.Hour).UTC().Format(time.RFC3339)+" 30m0s\n"+
		"10103/w ! Tempo gave worklog 7102 on issue 10103 no UTC start, and Jira shows no time zone for acc-hidden to place it by\n"+
		"users acc-east:User 4")
}

// A query Evidence cannot send is refused before any request.
func TestEvidenceRefusesAnInvalidQuery(t *testing.T) {
	s, _, requests := evidenceScenario(t, false)
	for name, q := range map[string]tracker.EvidenceQuery{
		"no issue":    {},
		"a key":       {Issues: []tracker.EvidenceIssue{{Issue: "WEB-1"}}},
		"twice":       {Issues: []tracker.EvidenceIssue{{Issue: "10101"}, {Issue: "10101"}}},
		"over a read": {Issues: make([]tracker.EvidenceIssue, tracker.MaxEvidenceIssues+1)},
	} {
		if _, err := s.p.Evidence(s.ctx, q); !errors.Is(err, tracker.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := len(requests()); n != 0 {
		t.Errorf("%d requests", n)
	}
}
