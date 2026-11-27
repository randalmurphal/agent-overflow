package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// tokenTurns hands out user a's credential on the first read, b's on the
// second and c's on every later one.
type tokenTurns struct{ reads atomic.Int32 }

func (s *tokenTurns) Credential(context.Context) (*trackerapi.Credential, error) {
	user := string(rune('a' + min(s.reads.Add(1)-1, 2)))
	return trackerapi.Basic(user+"@example.com", secret.New([]byte("fake-token-"+user))), nil
}

// walkSite is a fake Jira whose routes answer by the user a request is
// made as ("a", "b" or "c"), and that refuses with 401 a request of a
// user who used up its budget of answered requests, so the transport
// reads the credential again and asks as the next user.
type walkSite struct {
	*jiratest.Server
	t      *testing.T
	mu     sync.Mutex
	budget map[string]int // answered requests left; absent is unlimited
}

// walkAnswer answers one request as user.
type walkAnswer func(user string, r *http.Request, body []byte) string

func newWalkSite(t *testing.T, budget map[string]int) *walkSite {
	return &walkSite{Server: unnamedSite(t), t: t, budget: budget}
}

func (s *walkSite) serve(answer walkAnswer) jiratest.Handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		name, _, _ := r.BasicAuth()
		user, _, _ := strings.Cut(name, "@")
		s.mu.Lock()
		left, limited := s.budget[user]
		if limited {
			s.budget[user] = left - 1
		}
		s.mu.Unlock()
		if limited && left <= 0 {
			jiratest.Reply(401, `{"errorMessages":["Client must be authenticated"]}`)(w, r, b)
			return
		}
		jiratest.Reply(200, answer(user, r, b))(w, r, b)
	}
}

func (s *walkSite) route(method, path string, answer walkAnswer) { s.On(method, path, s.serve(answer)) }

// uid is item n of user's listing: 101 is a's first, 202 b's second.
func uid(user string, n int) int { return int(user[0]-'a'+1)*100 + n }

// offsetPaged answers an offset-paged list of two pages, one item each.
func offsetPaged(item func(user string, n int) string) walkAnswer {
	return func(user string, r *http.Request, _ []byte) string {
		start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
		return fmt.Sprintf(`{"startAt":%d,"maxResults":1,"total":2,"isLast":%t,"values":[%s]}`, start, start >= 1, item(user, start+1))
	}
}

// tokenPaged answers a token-paged issue list of two pages, one issue
// each, the token read from token.
func tokenPaged(token func(r *http.Request, body []byte) string) walkAnswer {
	return func(user string, r *http.Request, b []byte) string {
		n, next := 1, `"nextPageToken":"t2","isLast":false`
		if token(r, b) == "t2" {
			n, next = 2, `"isLast":true`
		}
		return fmt.Sprintf(`{"issues":[{"id":"%d","key":"WEB-%d","fields":{}}],%s}`, uid(user, n), uid(user, n), next)
	}
}

func bodyToken(_ *http.Request, b []byte) string {
	var v struct {
		Token string `json:"nextPageToken"`
	}
	_ = json.Unmarshal(b, &v)
	return v.Token
}

func queryToken(r *http.Request, _ []byte) string { return r.URL.Query().Get("nextPageToken") }

// ids joins what a walk returned as text, for comparison.
func ids[T any](items []T, id func(T) string) string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = id(it)
	}
	return strings.Join(out, ",")
}

// walkCase is one walk: routes serves it on a walkSite, and read runs it
// and says what it returned; want is user b's whole answer.
type walkCase struct {
	routes func(s *walkSite)
	read   func(ctx context.Context, c *Client) (string, error)
	want   string
}

const changeAt = `"created":"2026-11-10T10:00:00.000+0000","items":[]`

// walkCases are every walk of the client: each read composed of several
// requests whose answers must be one account's.
var walkCases = map[string]walkCase{
	"Boards": {
		func(s *walkSite) {
			s.route("GET", "/rest/agile/1.0/board", offsetPaged(func(u string, n int) string {
				return fmt.Sprintf(`{"id":%d,"name":"B","type":"scrum"}`, uid(u, n))
			}))
		},
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.Boards(ctx, BoardQuery{})
			return ids(l.Boards, func(b Board) string { return fmt.Sprint(b.ID) }), err
		}, "201,202",
	},
	"Projects": {
		func(s *walkSite) {
			s.route("GET", "/rest/api/3/project/search", offsetPaged(func(u string, n int) string {
				return fmt.Sprintf(`{"id":"%d","key":"P%d","name":"P"}`, uid(u, n), uid(u, n))
			}))
		},
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.Projects(ctx, ProjectQuery{})
			return ids(l.Projects, func(p Project) string { return p.ID }), err
		}, "201,202",
	},
	"BoardProjects": {
		func(s *walkSite) {
			s.route("GET", "/rest/agile/1.0/board/3/project", offsetPaged(func(u string, n int) string {
				return fmt.Sprintf(`{"id":"%d","key":"P%d","name":"P"}`, uid(u, n), uid(u, n))
			}))
		},
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.BoardProjects(ctx, 3)
			return ids(l, func(p Project) string { return p.ID }), err
		}, "201,202",
	},
	"Priorities": {
		func(s *walkSite) {
			s.route("GET", "/rest/api/3/priority/search", offsetPaged(func(u string, n int) string {
				return fmt.Sprintf(`{"id":"%d","name":"P"}`, uid(u, n))
			}))
		},
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.Priorities(ctx)
			return ids(l, func(p Priority) string { return p.ID }), err
		}, "201,202",
	},
	"BoardSprints": {
		sprintRoutes,
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.BoardSprints(ctx, 3)
			return ids(l.Sprints, func(s Sprint) string { return fmt.Sprint(s.ID) }) + fmt.Sprintf(" complete=%t", l.Complete), err
		}, "201,202 complete=true",
	},
	"Sprints": {
		sprintRoutes,
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.Sprints(ctx, SprintQuery{Board: 3})
			return ids(l.Sprints, func(s Sprint) string { return fmt.Sprint(s.ID) }), err
		}, "201,202",
	},
	"Changelog": {
		func(s *walkSite) {
			s.route("GET", "/rest/api/3/issue/WEB-1/changelog", offsetPaged(func(u string, n int) string {
				return fmt.Sprintf(`{"id":"%d",%s}`, uid(u, n), changeAt)
			}))
		},
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.Changelog(ctx, "WEB-1")
			return ids(l.Entries, func(e ChangeEntry) string { return e.ID }), err
		}, "201,202",
	},
	"IssueDetail": {
		func(s *walkSite) {
			s.route("GET", "/rest/api/3/issue/WEB-1", func(u string, _ *http.Request, _ []byte) string {
				return fmt.Sprintf(`{"id":"10001","key":"WEB-1","fields":{"summary":"%s"},"changelog":{"startAt":0,"maxResults":1,"total":2,"histories":[{"id":"%d",%s}]}}`,
					u, uid(u, 2), changeAt)
			})
			// Not a route, which the server would embed in the issue's
			// answer as its changelog expansion.
			s.Otherwise(s.serve(func(u string, r *http.Request, _ []byte) string {
				if r.URL.Path != "/rest/api/3/issue/WEB-1/changelog" {
					s.t.Errorf("unexpected request %s", r.URL.Path)
				}
				return fmt.Sprintf(`{"startAt":0,"maxResults":1,"total":1,"isLast":true,"values":[{"id":"%d",%s}]}`, uid(u, 1), changeAt)
			}))
		},
		func(ctx context.Context, c *Client) (string, error) {
			d, err := c.IssueDetail(ctx, "WEB-1")
			if err != nil {
				return "", err
			}
			return d.Issue.Summary + " " + ids(d.Changelog.Entries, func(e ChangeEntry) string { return e.ID }), nil
		}, "b 201,202",
	},
	"SearchAll": {
		func(s *walkSite) { s.route("POST", "/rest/api/3/search/jql", tokenPaged(bodyToken)) },
		func(ctx context.Context, c *Client) (string, error) {
			res, err := c.SearchAll(ctx, SearchQuery{JQL: "project = WEB"}, 0)
			return ids(res.Issues, func(is *Issue) string { return is.ID }) + fmt.Sprintf(" complete=%t", res.Complete), err
		}, "201,202 complete=true",
	},
	"ListAllIssues": {
		func(s *walkSite) { s.route("GET", "/rest/software/1.0/board/3/issue", tokenPaged(queryToken)) },
		func(ctx context.Context, c *Client) (string, error) {
			res, err := c.ListAllIssues(ctx, ListQuery{List: BoardIssues(3)}, 0)
			return ids(res.Issues, func(is *Issue) string { return is.ID }), err
		}, "201,202",
	},
	"BulkFetch": {
		func(s *walkSite) {
			s.route("POST", "/rest/api/3/issue/bulkfetch", func(u string, _ *http.Request, b []byte) string {
				var v struct {
					Refs []string `json:"issueIdsOrKeys"`
				}
				_ = json.Unmarshal(b, &v)
				var out []string
				for _, ref := range v.Refs {
					n, _ := strconv.Atoi(strings.TrimPrefix(ref, "WEB-"))
					out = append(out, fmt.Sprintf(`{"id":"%d","key":"%s","fields":{}}`, uid(u, 0)*10000+n, ref))
				}
				return `{"issues":[` + strings.Join(out, ",") + `],"issueErrors":[]}`
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			refs := make([]string, bulkFetchBatch+1)
			for i := range refs {
				refs[i] = fmt.Sprintf("WEB-%d", i+1)
			}
			res, err := c.BulkFetch(ctx, refs, "summary")
			users := map[byte]int{}
			for _, is := range res.Issues {
				users[is.ID[0]]++
			}
			return fmt.Sprintf("%v complete=%t", users, res.Complete), err
		}, fmt.Sprintf("map[50:%d] complete=true", bulkFetchBatch+1),
	},
	"BulkFetchByID": {
		func(s *walkSite) {
			s.route("POST", "/rest/api/3/issue/bulkfetch", func(u string, _ *http.Request, b []byte) string {
				var v struct {
					Refs []string `json:"issueIdsOrKeys"`
				}
				_ = json.Unmarshal(b, &v)
				var out []string
				for _, ref := range v.Refs {
					out = append(out, fmt.Sprintf(`{"id":"%s","key":"U%d-%s","fields":{}}`, ref, uid(u, 0), ref))
				}
				return `{"issues":[` + strings.Join(out, ",") + `],"issueErrors":[]}`
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			ids := make([]string, bulkFetchBatch+1)
			for i := range ids {
				ids[i] = strconv.Itoa(10001 + i)
			}
			res, err := c.BulkFetchByID(ctx, ids, "summary")
			users := map[byte]int{}
			for _, is := range res.Issues {
				users[is.Key[1]]++
			}
			return fmt.Sprintf("%v complete=%t", users, res.Complete), err
		}, fmt.Sprintf("map[50:%d] complete=true", bulkFetchBatch+1),
	},
	"BulkChangelogs": {
		func(s *walkSite) {
			s.route("POST", "/rest/api/3/changelog/bulkfetch", func(u string, r *http.Request, b []byte) string {
				n, next := 1, `,"nextPageToken":"t2"`
				if bodyToken(r, b) == "t2" {
					n, next = 2, ""
				}
				return fmt.Sprintf(`{"issueChangeLogs":[{"issueId":"10001","changeHistories":[{"id":"%d","created":1731232800000,"items":[]}]}]%s}`, uid(u, n), next)
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			res, err := c.BulkChangelogs(ctx, ChangelogQuery{Issues: []string{"10001"}})
			return ids(res.ByIssue["10001"], func(e ChangeEntry) string { return e.ID }), err
		}, "201,202",
	},
	"CreateIssueTypes": {
		func(s *walkSite) {
			s.route("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes", func(u string, r *http.Request, _ []byte) string {
				start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
				return fmt.Sprintf(`{"issueTypes":[{"id":"%d","name":"T"}],"total":2}`, uid(u, start+1))
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.CreateIssueTypes(ctx, "WEB")
			return ids(l, func(t IssueType) string { return t.ID }), err
		}, "201,202",
	},
	"CreateMeta": {
		func(s *walkSite) {
			s.route("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes/10002", func(u string, r *http.Request, _ []byte) string {
				start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
				return fmt.Sprintf(`{"fields":[{"fieldId":"f%d","name":"F","schema":{"type":"string"}}],"total":2}`, uid(u, start+1))
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			s, err := c.CreateMeta(ctx, "WEB", "10002")
			return ids(s.Fields, func(f FieldMeta) string { return f.ID }), err
		}, "f201,f202",
	},
	"KeptCreateMeta": {
		func(s *walkSite) {
			s.route("GET", "/rest/api/3/issue/createmeta/WEB/issuetypes/10002", func(u string, r *http.Request, _ []byte) string {
				start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
				return fmt.Sprintf(`{"fields":[{"fieldId":"f%d","name":"F","schema":{"type":"string"}}],"total":2}`, uid(u, start+1))
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			s, err := c.KeptCreateMeta(ctx, "WEB", "10002")
			return ids(s.Fields, func(f FieldMeta) string { return f.ID }), err
		}, "f201,f202",
	},
	"Worklogs": {
		func(s *walkSite) {
			s.route("GET", "/rest/api/3/issue/WEB-1/worklog", func(u string, r *http.Request, _ []byte) string {
				start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
				return fmt.Sprintf(`{"startAt":%d,"total":2,"worklogs":[{"id":"%d","issueId":"10001","started":"2026-11-10T10:00:00.000+0000","timeSpentSeconds":60}]}`,
					start, uid(u, start+1))
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.Worklogs(ctx, WorklogQuery{Issue: "WEB-1"})
			return ids(l.Worklogs, func(w Worklog) string { return w.ID }), err
		}, "201,202",
	},
	"WorklogActs": {
		func(s *walkSite) {
			s.route("GET", "/rest/api/3/issue/WEB-1/worklog", func(u string, r *http.Request, _ []byte) string {
				start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
				return fmt.Sprintf(`{"startAt":%d,"total":2,"worklogs":[{"id":"%d","started":"2026-11-10T10:00:00.000+0000","timeSpentSeconds":60}]}`,
					start, uid(u, start+1))
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.WorklogActs(ctx, WorklogQuery{Issue: "WEB-1"})
			return ids(l, func(a Act) string { return a.ID }), err
		}, "201,202",
	},
	"CommentActs": {
		func(s *walkSite) {
			s.route("GET", "/rest/api/3/issue/WEB-1/comment", func(u string, r *http.Request, _ []byte) string {
				start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
				return fmt.Sprintf(`{"startAt":%d,"total":2,"comments":[{"id":"%d","created":"2026-11-10T10:00:00.000+0000"}]}`, start, uid(u, start+1))
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.CommentActs(ctx, "WEB-1")
			return ids(l, func(a Act) string { return a.ID }), err
		}, "201,202",
	},
	"BulkActs": {
		func(s *walkSite) {
			s.route("POST", "/rest/api/3/issue/bulkfetch", func(u string, _ *http.Request, b []byte) string {
				var v struct {
					Refs []string `json:"issueIdsOrKeys"`
				}
				_ = json.Unmarshal(b, &v)
				var out []string
				for i, ref := range v.Refs {
					out = append(out, fmt.Sprintf(`{"id":"%s","fields":{"updated":"2026-11-10T10:00:00.000+0000",`+
						`"comment":{"startAt":0,"total":1,"comments":[{"id":"%d","created":"2026-11-10T10:00:00.000+0000"}]}}}`, ref, uid(u, 0)*10000+i))
				}
				return `{"issues":[` + strings.Join(out, ",") + `],"issueErrors":[]}`
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			ids := make([]string, bulkFetchCapacity([]string{"comment"})+1)
			for i := range ids {
				ids[i] = strconv.Itoa(10001 + i)
			}
			res, _, err := c.BulkActs(ctx, ids, true, false)
			users := map[byte]int{}
			for _, is := range res {
				users[is.Comments.Acts[0].ID[0]]++
			}
			return fmt.Sprint(users), err
		}, fmt.Sprintf("map[50:%d]", bulkFetchCapacity([]string{"comment"})+1),
	},
	"UpdatedWorklogs": {
		func(s *walkSite) { s.route("GET", "/rest/api/3/worklog/updated", worklogFeed) },
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.UpdatedWorklogs(ctx, time.UnixMilli(0))
			return ids(l.Changes, func(w WorklogChange) string { return fmt.Sprint(w.ID) }), err
		}, "201,202",
	},
	"DeletedWorklogs": {
		func(s *walkSite) { s.route("GET", "/rest/api/3/worklog/deleted", worklogFeed) },
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.DeletedWorklogs(ctx, time.UnixMilli(0))
			return ids(l.Changes, func(w WorklogChange) string { return fmt.Sprint(w.ID) }), err
		}, "201,202",
	},
	"WorklogsByID": {
		func(s *walkSite) {
			s.route("POST", "/rest/api/3/worklog/list", func(u string, _ *http.Request, b []byte) string {
				var v struct {
					IDs []int64 `json:"ids"`
				}
				_ = json.Unmarshal(b, &v)
				var out []string
				for _, id := range v.IDs {
					out = append(out, fmt.Sprintf(`{"id":"%d","issueId":"10001","author":{"accountId":"acc-%s"}}`, id, u))
				}
				return "[" + strings.Join(out, ",") + "]"
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			wanted := make([]int64, worklogListLimit+1)
			for i := range wanted {
				wanted[i] = int64(i + 1)
			}
			l, err := c.WorklogsByID(ctx, wanted)
			authors := map[string]int{}
			for _, w := range l.Worklogs {
				if w.Author != nil {
					authors[w.Author.AccountID]++
				}
			}
			return fmt.Sprintf("%v complete=%t", authors, l.Complete), err
		}, fmt.Sprintf("map[acc-b:%d] complete=true", worklogListLimit+1),
	},
	"PokerInstalled": {
		pokerRoutes,
		func(ctx context.Context, c *Client) (string, error) {
			ok, err := c.PokerInstalled(ctx)
			return fmt.Sprint(ok), err
		}, "true",
	},
	"PokerVotes": {
		pokerRoutes,
		func(ctx context.Context, c *Client) (string, error) {
			r, err := c.PokerVotes(ctx, "10101")
			return ids(r.Votes, func(v PokerVote) string { return v.Account }), err
		}, "acc-b@cloud-b",
	},
	"FindUser": {
		func(s *walkSite) {
			s.route("GET", "/rest/api/3/user/search", func(u string, r *http.Request, _ []byte) string {
				start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
				user := func(id, name string) string {
					return fmt.Sprintf(`[{"accountId":%q,"displayName":%q,"active":true,"accountType":"atlassian"}]`, id, name)
				}
				switch {
				case u == "a" && start == 0:
					return user("acc-a1", "Same Name")
				case start == 0:
					return user("acc-"+u+"1", "Other")
				case start == 1:
					return user("acc-"+u+"2", "Same Name")
				}
				return `[]`
			})
		},
		func(ctx context.Context, c *Client) (string, error) {
			u, err := c.FindUser(ctx, "Same Name")
			return u.AccountID, err
		}, "acc-b2",
	},
}

// pokerRoutes answer the poker calls by user: the cloud ID and the token
// are the user's, the listing names the panel only for the user's own
// cloud ID, and the one vote names its voter and the cloud ID the invoke
// was sent with.
func pokerRoutes(s *walkSite) {
	s.route("GET", "/_edge/tenant_info", func(u string, _ *http.Request, _ []byte) string { return `{"cloudId":"cloud-` + u + `"}` })
	s.route("GET", "/rest/api/3/issue/10101", func(string, *http.Request, []byte) string { return pokerTestIssue })
	s.route("POST", "/rest/internal/2/forge/context/token", func(u string, _ *http.Request, _ []byte) string {
		return `{"token":"` + pokerJWT(strings.Replace(pokerTestClaims, "acc-user1", "acc-"+u, 1)) + `","expiresIn":899442}`
	})
	s.route("POST", "/gateway/api/graphql", func(u string, _ *http.Request, body []byte) string {
		if strings.Contains(string(body), `"operationName":"Invoke"`) {
			var req struct {
				Variables struct {
					Input struct {
						Payload struct {
							Context struct {
								CloudID string `json:"cloudId"`
							} `json:"context"`
						} `json:"payload"`
					} `json:"input"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(body, &req)
			voter := "acc-" + u + "@" + req.Variables.Input.Payload.Context.CloudID
			return invokeBody(200, `{"issueId":"10101","isRevealed":true,"votes":[{"userId":"`+voter+`","vote":3}]}`)
		}
		if !strings.Contains(string(body), `site/cloud-`+u+`"`) {
			// Another user's site lists no poker panel.
			return `{"data":{"extensionContexts":[{"id":"x","extensionsByType":[]}]}}`
		}
		return pokerTestListing
	})
}

func sprintRoutes(s *walkSite) {
	s.route("GET", "/rest/agile/1.0/board/3/sprint", offsetPaged(func(u string, n int) string {
		return fmt.Sprintf(`{"id":%d,"name":"S","state":"active"}`, uid(u, n))
	}))
}

// worklogFeed answers worklog/updated or worklog/deleted in two pages.
func worklogFeed(u string, r *http.Request, _ []byte) string {
	n, last := 1, false
	if r.URL.Query().Get("since") == "1000" {
		n, last = 2, true
	}
	return fmt.Sprintf(`{"values":[{"worklogId":%d,"updatedTime":%d}],"until":%d,"lastPage":%t}`, uid(u, n), n*1000, n*1000, last)
}

// TestWalkStartsAgainUnderOneCredential runs every walk while the
// credential changes between its first and second request (user a's is
// refused after one answer, and the transport reads user b's): the walk
// starts again under b and returns b's whole answer, never a's first
// page with b's continued one.
func TestWalkStartsAgainUnderOneCredential(t *testing.T) {
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range walkCases {
			if !yield(k) {
				return
			}
		}
	}) {
		t.Run(name, func(t *testing.T) {
			wc := walkCases[name]
			s := newWalkSite(t, map[string]int{"a": 1})
			wc.routes(s)
			src := &tokenTurns{}
			c := markClient(t, s.Server, src, nil)
			got, err := wc.read(t.Context(), c)
			if err != nil || got != wc.want {
				t.Fatalf("got %q, %v; want %q", got, err, wc.want)
			}
			if n := src.reads.Load(); n != 2 {
				t.Errorf("%d credential reads, want 2: the credential did not change mid-walk", n)
			}
		})
	}
}

// TestWalkFailsWhenTheCredentialChangesAgain: when the credential
// changes again during the walk that started over, the walk fails with
// an error asking again resolves, returning nothing.
func TestWalkFailsWhenTheCredentialChangesAgain(t *testing.T) {
	// a answers the first page; b the second, so the walk starts again,
	// and the first page again; c the second again.
	s := newWalkSite(t, map[string]int{"a": 1, "b": 2})
	sprintRoutes(s)
	c := markClient(t, s.Server, &tokenTurns{}, nil)
	l, err := c.Sprints(t.Context(), SprintQuery{Board: 3})
	if !errors.Is(err, trackerapi.ErrCredentialChanged) || len(l.Sprints) != 0 {
		t.Fatalf("got %d sprints, %v; want none and ErrCredentialChanged", len(l.Sprints), err)
	}
}

// The other exported Client methods, by why they are no walk. A method
// added to Client fails TestEveryWalkIsListed until it is named in
// walkCases or here.
var (
	// pagesToTheCaller read one page the caller continues with a token or
	// an offset; each page is one answer.
	pagesToTheCaller = []string{"Search", "ListIssues", "Comments", "LatestComments", "SearchUsers", "AssignableUsers"}
	// oneRequest reads answer from one request (or a cache).
	oneRequest = []string{
		"AttachmentMediaID", "Attachments", "Board", "BoardConfig", "Comment", "CountIssues", "CountListIssues",
		"DownloadAttachment", "EditMeta", "Fields", "FieldsNaming", "GetIssue", "IssueLinkTypes", "MatchJQL", "Me", "Myself", "Project",
		"PokerVoter", "ProjectStatuses", "RemoteLinks", "Statuses", "Transitions", "User", "Watchers", "Worklog",
	}
	// writes and the plans and values they are built from compose a write,
	// not a read's answer.
	writes = []string{
		"AddComment", "AddRemoteLink", "AddWatcher", "AddWorklog", "ApplyCreate", "ApplyEdit", "Assign", "Attach",
		"CompleteSprint", "CreateSprint", "DeleteAttachment", "DeleteComment", "DeleteSprint", "DeleteWorklog",
		"EditComment", "LinkIssues", "MoveToBacklog", "MoveToSprint", "PlanCreate", "PlanEdit", "Rank",
		"RemoveRemoteLink", "RemoveWatcher", "SetEstimate", "StartSprint", "Transition", "Unassign", "UnlinkIssues",
		"UpdateSprint", "UpdateWorklog", "Value", "CastPokerVote", "RevealPokerVotes", "ClearPokerVotes",
	}
	noRequest = []string{"Close", "ForgetBoard", "ForgetCredential"}
)

// TestEveryWalkIsListed fails for an exported Client method that is
// neither a walk with a case in walkCases nor named in a list of the
// methods that are not walks, so a new walk cannot be added without its
// case.
func TestEveryWalkIsListed(t *testing.T) {
	named := map[string]string{}
	for name := range walkCases {
		named[name] = "walk"
	}
	for kind, list := range map[string][]string{"pages": pagesToTheCaller, "one": oneRequest, "write": writes, "none": noRequest} {
		for _, name := range list {
			if prev, ok := named[name]; ok {
				t.Errorf("%s is listed as %s and %s", name, prev, kind)
			}
			named[name] = kind
		}
	}
	typ := reflect.TypeFor[*Client]()
	for i := range typ.NumMethod() {
		if name := typ.Method(i).Name; named[name] == "" {
			t.Errorf("Client.%s is neither a walk with a case nor listed as no walk", name)
		}
	}
	for name := range named {
		if _, ok := typ.MethodByName(name); !ok {
			t.Errorf("%s names no Client method", name)
		}
	}
}
