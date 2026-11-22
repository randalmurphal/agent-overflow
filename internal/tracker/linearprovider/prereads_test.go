package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

// preRead is one write whose read before it writes fails: before are the
// reads that come first, answered; fails is the read that fails, by its
// operation and variables; gone is Linear's answer when what that read
// names does not exist: an error at the field for one entity, an empty
// page for a list. A row whose fails is the write's own request is a
// write that reads nothing first; a row with no fails is a write refused
// before any request.
type preRead struct {
	method, label string
	before        []lineartest.Exchange
	fails         lineartest.Exchange
	gone          string
	call          func(ctx context.Context, p *Provider) tracker.Result
}

// goneAt is Linear's answer to a read of one entity at field that does
// not exist.
func goneAt(field, typ string) string {
	return `{"data":null,"errors":[{"message":"Entity not found: ` + typ + `","path":["` + field + `"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`
}

const emptyPage = `{"data":{"page":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`

func preReads() []preRead {
	one := tracker.IssueID(issueID(1))
	labels := []tracker.FieldEdit{{Field: tracker.FieldLabels, Value: tracker.TypedValues("Bug")}}
	assignee := []tracker.FieldEdit{{Field: tracker.FieldAssignee, Value: tracker.TypedValues("User 1")}}
	project := []tracker.FieldEdit{{Field: FieldProject, Value: tracker.TypedValues("Launch")}}
	description := []tracker.FieldEdit{{Field: tracker.FieldDescription, Value: tracker.MarkdownEdit("x", "")}}
	create := func(fields []tracker.FieldEdit) func(context.Context, *Provider) tracker.Result {
		return func(ctx context.Context, p *Provider) tracker.Result {
			return p.Create(ctx, tracker.CreateInput{Project: web, Title: "x", Fields: fields})
		}
	}
	edit := func(fields []tracker.FieldEdit) func(context.Context, *Provider) tracker.Result {
		return func(ctx context.Context, p *Provider) tracker.Result {
			return p.Edit(ctx, tracker.EditInput{Issue: one, Fields: fields})
		}
	}
	move := func(ctx context.Context, p *Provider) tracker.Result {
		return p.Move(ctx, tracker.MoveInput{Issue: one, Move: "state-todo"})
	}
	estimate := func(ctx context.Context, p *Provider) tracker.Result {
		return p.SetEstimate(ctx, tracker.EstimateInput{Board: web, Issue: one})
	}
	rank := func(ctx context.Context, p *Provider) tracker.Result {
		return p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one}, Prev: neighbor(8)})
	}
	crowded := func(ctx context.Context, p *Provider) tracker.Result {
		return p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one}, Prev: neighbor(8), Next: neighbor(9)})
	}
	toSprint := func(ctx context.Context, p *Provider) tracker.Result {
		return p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: web, Sprint: "cycle-1", Issues: []tracker.IssueID{one}})
	}
	open := lineartest.Cycle{ID: "cycle-1", Number: 1, Name: "Cycle 1", Team: web, StartsAt: at, EndsAt: "2026-11-16T00:00:00.000Z", Active: true}
	team := lineartest.NewTeam("WEB")
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	sprint := func(call func(context.Context, *Provider) tracker.Result) preRead {
		return preRead{label: "none", call: call}
	}
	return []preRead{
		{"Create", "labels", nil, labelsX(web), emptyPage, create(labels)},
		{"Create", "members", nil, membersX(web), goneAt("node", "Team"), create(assignee)},
		{"Create", "projects", nil, lineartest.Exchange{Op: "Projects", Vars: `{"first":100,"filter":{"accessibleTeams":{"some":{"id":{"eq":"` + web + `"}}}}}`}, emptyPage, create(project)},
		{"Create", "own request", nil, lineartest.Exchange{Op: "CreateIssue", Vars: `{"input":{"teamId":"` + web + `","title":"x"}}`}, goneAt("issueCreate", "Team"), create(nil)},
		{"Edit", "issue", nil, summaryX(issueID(1), issue(1, 1)), notFound, edit(description)},
		{"Edit", "labels", []lineartest.Exchange{summaryX(issueID(1), issue(1, 1))}, labelsX(web), emptyPage, edit(labels)},
		{"Move", "issue and states", nil, issueStatesX(issueID(1), "state-todo"), notFound, move},
		{"SetEstimate", "issue and team", nil, issueTeamX(1, team, false), notFound, estimate},
		{"Rank", "board check", nil, onBoardX(ids(1, 8)), emptyPage, rank},
		{"Rank", "manual order", []lineartest.Exchange{onBoardX(ids(1, 8, 9), issue(1, 0), issue(8, 1), issue(9, math.Nextafter(1, 2)))},
			manualX("", false, ""), emptyPage, crowded},
		{"MoveToSprint", "cycle and board", nil, cyclePositionsX(open, ids(1)), goneAt("cycle", "Cycle"), toSprint},
		{"MoveToSprint", "board, no cycle", nil, onBoardX(ids(1)), emptyPage, func(ctx context.Context, p *Provider) tracker.Result {
			return p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: web, Issues: []tracker.IssueID{one}})
		}},
		{"AddComment", "own request", nil, lineartest.Exchange{Op: "CreateComment", Vars: `{"input":{"issueId":"` + issueID(1) + `","body":"x"}}`},
			goneAt("commentCreate", "Issue"), func(ctx context.Context, p *Provider) tracker.Result {
				return p.AddComment(ctx, tracker.CommentInput{Issue: one, Markdown: "x"})
			}},
		{"EditComment", "comment", nil, commentX(lineartest.Comment{ID: "comment-1"}), goneAt("comment", "Comment"), func(ctx context.Context, p *Provider) tracker.Result {
			return p.EditComment(ctx, tracker.CommentEdit{Issue: one, Comment: "comment-1", Markdown: "x"})
		}},
		{"DeleteComment", "comment", nil, commentX(lineartest.Comment{ID: "comment-1"}), goneAt("comment", "Comment"), func(ctx context.Context, p *Provider) tracker.Result {
			return p.DeleteComment(ctx, tracker.CommentDelete{Issue: one, Comment: "comment-1"})
		}},
		{"Link", "own request", nil, lineartest.Exchange{Op: "CreateRelation", Vars: `{"input":{"issueId":"` + issueID(1) + `","relatedIssueId":"` + issueID(2) + `","type":"blocks"}}`},
			goneAt("issueRelationCreate", "Issue"), func(ctx context.Context, p *Provider) tracker.Result {
				return p.Link(ctx, tracker.LinkInput{Type: "blocks", Outward: one, Inward: tracker.IssueID(issueID(2))})
			}},
		{"Unlink", "link", nil, relationX("relation-1", 1, 2), goneAt("issueRelation", "IssueRelation"), func(ctx context.Context, p *Provider) tracker.Result {
			return p.Unlink(ctx, tracker.UnlinkInput{Issue: one, Link: "relation-1", Other: tracker.IssueID(issueID(2))})
		}},
		withMethod("CreateSprint", sprint(func(ctx context.Context, p *Provider) tracker.Result {
			return p.CreateSprint(ctx, tracker.SprintInput{Board: web, Name: "x"})
		})),
		withMethod("StartSprint", sprint(func(ctx context.Context, p *Provider) tracker.Result {
			return p.StartSprint(ctx, tracker.SprintStart{Board: web, Sprint: "cycle-1", Start: start, End: start.AddDate(0, 0, 14)})
		})),
		withMethod("CompleteSprint", sprint(func(ctx context.Context, p *Provider) tracker.Result {
			return p.CompleteSprint(ctx, tracker.SprintCompletion{Board: web, Sprint: "cycle-1"})
		})),
		withMethod("EditSprint", sprint(func(ctx context.Context, p *Provider) tracker.Result {
			name := "x"
			return p.EditSprint(ctx, tracker.SprintEdit{Board: web, Sprint: "cycle-1", Name: &name})
		})),
		withMethod("Attach", sprint(func(ctx context.Context, p *Provider) tracker.Result {
			return p.Attach(ctx, tracker.AttachInput{Issue: one, Files: []tracker.File{{Name: "a.txt", Content: strings.NewReader("a")}}})
		})),
	}
}

func withMethod(method string, r preRead) preRead {
	r.method = method
	return r
}

// TestPreReadsComplete proves the table of TestFailedPreReads names every
// write of tracker.Provider, so a write added later fails here until it
// has a row.
func TestPreReadsComplete(t *testing.T) {
	t.Parallel()
	named := map[string]bool{}
	for _, r := range preReads() {
		named[r.method] = true
	}
	var writes []string
	for m := range reflect.TypeFor[tracker.Provider]().Methods() {
		if m.Type.NumOut() == 1 && m.Type.Out(0) == reflect.TypeFor[tracker.Result]() {
			writes = append(writes, m.Name)
		}
	}
	if len(writes) < 16 {
		t.Fatalf("found only %d writes: %v", len(writes), writes)
	}
	for _, w := range writes {
		if !named[w] {
			t.Errorf("write %s has no row in preReads", w)
		}
	}
}

// TestFailedPreReads proves that when the read a write makes before it
// writes fails, with an HTTP 500 or with Linear finding nothing, on the
// most minimal input Check permits, the result passes Result.Check and
// nothing is sent. Linear's answer is reachable from every part's
// outcome; a list that comes back empty carries no error of Linear's,
// and every part is refused with its reason. A write that reads nothing
// first has its own request fail instead, and one refused before any
// request sends nothing.
func TestFailedPreReads(t *testing.T) {
	t.Parallel()
	for _, r := range preReads() {
		kinds := []string{"500", "gone"}
		if r.fails.Op == "" {
			kinds = []string{"none"}
		}
		for _, kind := range kinds {
			t.Run(fmt.Sprintf("%s/%s/%s", r.method, r.label, kind), func(t *testing.T) {
				t.Parallel()
				script := cloneScript(r.before)
				if r.fails.Op != "" {
					fail := lineartest.Exchange{Op: r.fails.Op, Vars: r.fails.Vars}
					switch kind {
					case "500":
						fail.Status, fail.Body = 500, `{"errors":[{"message":"boom"}]}`
					default:
						fail.Body = r.gone
					}
					script = append(script, fail)
				}
				srv := lineartest.NewServer(t, script...)
				var mu sync.Mutex
				var sent []string
				srv.SetReport(func(format string, args ...any) {
					mu.Lock()
					defer mu.Unlock()
					sent = append(sent, fmt.Sprintf(format, args...))
				})
				res := r.call(t.Context(), providerOf(t, srv))
				checkResult(t, res)
				if len(sent) > 0 {
					t.Errorf("sent after the failed read: %v", sent)
				}
				if len(res.Parts) == 0 {
					t.Fatal("no parts")
				}
				for i, part := range res.Parts {
					err := part.Outcome.Err()
					var le *linear.Error
					switch {
					case part.Outcome.State() == tracker.StateApplied:
						t.Errorf("part %d applied: %s", i, describe(res))
					case kind == "500" && (!errors.As(err, &le) || le.Status != 500):
						t.Errorf("part %d %s: Linear's 500 is not reachable: %v", i, describe(res), err)
					case kind == "gone" && r.gone != emptyPage && !errors.As(err, &le):
						t.Errorf("part %d %s: Linear's answer is not reachable: %v", i, describe(res), err)
					case kind != "500" && err == nil:
						t.Errorf("part %d %s carries no error", i, describe(res))
					}
					if _, refused := part.Outcome.Refusal(); kind != "500" && r.fails.Op != "" && !refused && !isOwnRequest(r) {
						t.Errorf("part %d %s: what the read did not find is not refused", i, describe(res))
					}
				}
			})
		}
	}
}

// isOwnRequest reports a row whose failing request is the write itself.
func isOwnRequest(r preRead) bool { return r.label == "own request" }

func cloneScript(xs []lineartest.Exchange) []lineartest.Exchange {
	return append([]lineartest.Exchange(nil), xs...)
}

// A client's refusal of a read's input, a refused input that was never
// sent, is a refusal as invalid, not a write left unsent.
func TestClientRefusalBeforeWriteIsInvalid(t *testing.T) {
	t.Parallel()
	p, _ := newProvider(t)
	_, err := p.c.Comment(context.Background(), "")
	if !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Fatalf("the client did not refuse an empty ID: %v", err)
	}
	one := tracker.IssueID(issueID(1))
	for _, r := range []tracker.Result{beforeWrite(tracker.StepComment, err, one), editRefusal(tracker.StepComment, err, one)} {
		expectResult(t, r, "1:comment:refused(invalid)")
		if !errors.Is(r.Parts[0].Outcome.Err(), tracker.ErrInvalidInput) {
			t.Errorf("the refusal does not carry the client's: %v", r.Parts[0].Outcome.Err())
		}
	}
}
