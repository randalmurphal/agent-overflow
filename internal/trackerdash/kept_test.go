package trackerdash

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// shapes is one dashboard of own metrics over a sprint board, a widget of
// every form for each shape of metric the sink keeps by another rule:
// events by period, by day and by person, events some of which have no
// instant, and states by sprint and by period.
func shapes(b Board) Board {
	metrics := map[string]Metric{}
	var widgets []Widget
	add := func(m Metric, forms ...Form) {
		m.Name = fmt.Sprintf("w_%d", len(metrics)+1)
		m.Title, m.Words = "Shape "+m.Name, []string{"A shape of metric."}
		if m.Reduce == "" {
			m.Reduce = ReduceCount
		}
		m.Unit = UnitTickets
		metrics[m.Name] = m
		for i, f := range forms {
			widgets = append(widgets, Widget{ID: fmt.Sprintf("%s_%d", m.Name, i), Metric: m.Name, Form: f, Width: 6})
		}
	}
	add(Metric{SQL: `SELECT ticket, at FROM changes`, Bucket: BucketPeriod}, FormNumber, FormBars, FormLine, FormTable, FormList)
	add(Metric{SQL: `SELECT ticket, at FROM changes`, Bucket: BucketDay}, FormNumber, FormLine)
	add(Metric{SQL: `SELECT c.ticket AS ticket, c.at AS at, t.assignee AS bucket FROM changes c JOIN tickets t ON t.id = c.ticket`, Bucket: BucketPerson},
		FormBars, FormTable, FormNumber)
	add(Metric{SQL: `SELECT t.id AS ticket, iif(t.status_group = 'done', NULL, t.created_at) AS at FROM tickets t`, Bucket: BucketPeriod},
		FormNumber, FormBars, FormList)
	add(Metric{SQL: `SELECT ticket, sprint AS bucket, from_at AS at, to_at AS until FROM sprint_intervals`, Bucket: BucketSprint},
		FormNumber, FormBars, FormTable, FormList)
	add(Metric{SQL: `SELECT ticket, from_at AS at, to_at AS until FROM status_intervals`, Bucket: BucketPeriod}, FormNumber, FormLine, FormList)
	b.Own = Own{Dashboards: []Dashboard{{ID: "d_1", Title: "Shapes", Widgets: widgets}}, Metrics: metrics}
	return b
}

// rowKey is row c as the sink read it: place and the takes mark a copy
// with its period and whether it counts.
func rowKey(c contribution) string {
	c.period, c.counted = "", false
	return fmt.Sprintf("%+v", c)
}

// The sink keeps exactly the rows a reduction takes. For every widget of
// every board, range and narrowing here, and every drill its answer
// offers, the answer made from the rows the sink kept is the answer made
// from every row of the narrowed page, so the sink dropped no row a
// number needs; and each row it kept is in one of the range's takes, so
// it charged none that no number reads.
func TestTheSinkKeepsExactlyWhatAReductionTakes(t *testing.T) {
	t.Parallel()
	weeks := newFixture(false, "points").person("u1", "User 1").person("u2", "User 2").
		status("1", "To Do", "todo").status("3", "In Progress", "in_progress").status("5", "Done", "done")
	weeks.ticket(ticket{id: "t1", key: "WEB-1", status: "5", group: "done", assignee: "u1", points: "5", created: ago(30), historyFrom: ago(30), started: ago(4), done: ago(1)})
	weeks.ticket(ticket{id: "t2", key: "WEB-2", status: "5", group: "done", assignee: "u2", points: "3", created: ago(30), res: ago(8)})
	weeks.ticket(ticket{id: "t3", key: "WEB-3", status: "5", group: "done", points: "2", created: ago(30), historyFrom: ago(30), started: ago(40), done: ago(33)})
	weeks.ticket(ticket{id: "t4", key: "WEB-4", status: "5", group: "done", assignee: "u1", points: "1", created: ago(100), updated: ago(90)})
	weeks.ticket(ticket{id: "t5", key: "WEB-5", status: "5", group: "done", assignee: "u2", points: "1", created: ago(20), updated: ago(2)})
	weeks.ticket(ticket{id: "t6", key: "WEB-6", status: "3", group: "in_progress", assignee: "u1", created: ago(9), since: ago(8)})
	for _, r := range [][3]string{{fmt.Sprint(ago(1)), "t1", "finished"}, {fmt.Sprint(ago(1)), "t1", "review"}, {fmt.Sprint(ago(8)), "t2", "finished"},
		{fmt.Sprint(ago(33)), "t3", "finished"}, {"NULL", "t4", "finished"}, {"NULL", "t5", "finished"}, {"NULL", "t5", "review"}, {fmt.Sprint(ago(3)), "t6", "support"}} {
		person, value := "'u2'", "0.5"
		if r[0] == "NULL" {
			person, value = "NULL", "NULL"
		}
		weeks.exec(`INSERT INTO credit(sprint, at, ticket, kind, person, value, share, review_share, did, qualified, hours, hours_per_point, note)
			VALUES (NULL, %s, '%s', '%s', %s, %s, %s, 10, 'commented, logged time', 1, 2, 4, NULL)`, r[0], r[1], r[2], person, value, value)
	}
	sprints, byWeek := []string{RangeThisSprint, RangeSprints3, RangeSprints5, RangeSprints10}, []string{RangeWeeks2, RangeWeeks4, RangeWeeks12}
	compared, dropped, drills := 0, 0, 0
	for _, bc := range []struct {
		name      string
		b         Board
		dashboard string
		ranges    []string
	}{
		{"the team's sprints", teamBoard().board(), builtinSprintsID, sprints},
		{"credit in points", creditOn(creditBoard("points")), builtinSprintsID, sprints},
		{"credit in tickets", creditOn(creditBoard("none")), builtinSprintsID, sprints},
		{"the burndown", burnBoard("points").board(), builtinSprintsID, sprints},
		{"the sprint scope", scopeBoard().board(), builtinSprintsID, sprints},
		{"weeks", weeks.board(), builtinWeeksID, byWeek},
		{"weeks with credit", creditOn(weeks), builtinWeeksID, byWeek},
		{"every shape", shapes(teamBoard().
			stretch("s4", "t6", "8", ago(40), ago(15)).stretch("s5", "t2", "5", ago(20), ago(8)).stretch("s5", "t3", "2", ago(20), ago(9)).
			stretch("s6", "t1", "3", ago(15), ago(1)).stretch("s6", "t3", "2", ago(9), 0).stretch("s7", "t3", "2", ago(1), 0).
			stretch("s7", "t4", "1", ago(1), 0).stretch("s5", "t9", "3", ago(20), 0).board()), "d_1", sprints},
		{"a kept dashboard and a proposal", keeping(teamBoard().board()), "d_1", sprints},
	} {
		for _, id := range bc.ranges {
			for _, n := range []Request{{}, {Person: "u1"}, {Person: "u2"}, {Epic: "e1"}} {
				r := Request{Dashboard: bc.dashboard, Range: id, TimeZone: "UTC", Person: n.Person, Epic: n.Epic}
				name := fmt.Sprintf("%s, %s, person %q, epic %q", bc.name, id, n.Person, n.Epic)
				o, err := evaluator().begin(context.Background(), bc.b, r, func(Widget, bool) bool { return true })
				if err != nil {
					if n.Person == "" && n.Epic == "" {
						t.Fatalf("%s: %v", name, err)
					}
					continue // the board has no such person or epic
				}
				now := o.now.UnixMilli()
				for _, w := range o.widgets() {
					m := o.metrics[w.Metric]
					pr := o.preps[m.Name]
					if pr.err != nil {
						t.Fatalf("%s: widget %s does not prepare: %s", name, w.ID, pr.err.Sentence)
					}
					whole, err := o.ev.run(o.ctx, m, pr.p, now, o.sp.since(now), kept{keep: o.ev.keeps(o.n, pr.p)})
					if err != nil || whole.err != nil {
						t.Fatalf("%s: widget %s with every row: %v, %+v", name, w.ID, err, whole.err)
					}
					c, err := o.count(m, widgetReads(w, m, o.sp))
					if err != nil || c.err != nil {
						t.Fatalf("%s: widget %s: %v, %+v", name, w.ID, err, c.err)
					}
					if c.note != "" {
						continue // its logged time is not read: nothing ran
					}
					got, want := o.widget(w, m, c), o.widget(w, m, whole)
					if !reflect.DeepEqual(got, want) {
						t.Errorf("%s: widget %s from the %d rows kept is\n%+v\nfrom all %d rows\n%+v", name, w.ID, len(c.rows), got, len(whole.rows), want)
					}
					compared++
					if len(c.rows) < len(whole.rows) {
						dropped++
					}
					g := o.ev.inRange(m, c, o.sp)
					inTake := map[string]bool{}
					for _, tk := range []taken{g.held(), g.whole(), g.earlier(), g.listed()} {
						for _, row := range tk.rows {
							inTake[rowKey(row)] = true
						}
					}
					for _, p := range g.periods {
						for _, row := range g.of(p).rows {
							inTake[rowKey(row)] = true
						}
					}
					for _, row := range c.rows {
						if !inTake[rowKey(row)] {
							t.Errorf("%s: widget %s kept a row no take of the range holds: %+v", name, w.ID, row)
							break
						}
					}
					reqs := []DrillRequest{{}}
					for _, b := range got.Buckets {
						reqs = append(reqs, DrillRequest{Cell: &DrillCell{Bucket: b.Key}})
						if len(got.Series) > 1 || got.Series[0] != "" {
							for _, s := range got.Series {
								reqs = append(reqs, DrillRequest{Cell: &DrillCell{Bucket: b.Key, Series: s}})
							}
						}
					}
					if got.More > 0 && ranks(w, m) {
						reqs = append(reqs, DrillRequest{Rest: true})
					}
					for _, dr := range reqs {
						dr.Request, dr.Widget = r, w.ID
						dc, err := o.count(m, drillReads(w, m, o.sp, dr))
						if err != nil || dc.err != nil {
							t.Fatalf("%s: drill %+v of widget %s: %v, %+v", name, dr.Cell, w.ID, err, dc.err)
						}
						gotD, gotErr := o.drill(w, m, dc, dr)
						wantD, wantErr := o.drill(w, m, whole, dr)
						if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || !reflect.DeepEqual(gotD, wantD) {
							t.Errorf("%s: drill %+v (rest %t) of widget %s from the %d rows kept is\n%+v, %v\nfrom all %d rows\n%+v, %v",
								name, dr.Cell, dr.Rest, w.ID, len(dc.rows), gotD, gotErr, len(whole.rows), wantD, wantErr)
						}
						if gotErr == nil {
							drills++
						}
						if dr.Cell != nil && !ranks(w, m) {
							if j := slices.IndexFunc(g.periods, func(p period) bool { return p.id == dr.Cell.Bucket }); j >= 0 {
								cell := map[string]bool{}
								for _, row := range o.ev.inRange(m, dc, o.sp).of(g.periods[j]).rows {
									cell[rowKey(row)] = true
								}
								for _, row := range dc.rows {
									if !cell[rowKey(row)] {
										t.Errorf("%s: the drill of %s of widget %s kept a row its period does not hold: %+v", name, dr.Cell.Bucket, w.ID, row)
										break
									}
								}
							}
						}
					}
				}
				if err := o.end(); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
		}
	}
	// The comparison proves something only where the sink dropped rows and
	// the drills answered.
	if compared < 500 || dropped < 100 || drills < 1000 {
		t.Fatalf("compared %d widgets, %d of them with rows dropped, and %d drills; want the matrix to exercise the sink", compared, dropped, drills)
	}
	t.Logf("compared %d widgets, %d with rows the sink dropped, and %d drills", compared, dropped, drills)
}
