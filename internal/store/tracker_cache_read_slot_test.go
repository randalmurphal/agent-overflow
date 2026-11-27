package store

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// mirrorPoolReads are the functions of the mirror's files that read on the
// read pool directly, with how many such reads each makes. Each is a
// bounded probe: its cost does not grow with a board or a root. Every
// other mirror read, and every mirror read of more than one statement,
// opens its snapshot through mirrorReadSnapshot, which waits for a history
// read slot first.
var mirrorPoolReads = map[string]int{
	"GetMirrorRoot":           1, // one root, by connection
	"GetMirrorCatalog":        1, // at most mirrorMaxLookup users and statuses, each by key
	"ListMirrorCards":         1, // at most mirrorMaxLookup cards, each by key
	"ListMirrorParents":       1, // at most mirrorMaxLookup parents' names, each by key
	"ListMirrorIssueRows":     1, // a page of one issue's rows
	"ListMirrorTicketRows":    1, // the rows of at most MirrorMaxBodies issues, each by key
	"ListMirrorSprintNames":   1, // at most mirrorMaxLookup sprints on as many boards, each pair by key
	"ListMirrorIssueWorklogs": 1, // a page of one issue's worklogs
	"GetMirrorSync":           1, // one board's state
	// One board's column statuses, at most mirrorMaxColumnStatuses.
	"GetMirrorBoardStatuses": 1,
	"ListMirrorBodies":       1, // at most MirrorMaxBodies bodies, each by key
	// At most mirrorMaxLookup cards, details and boards, each by key: the
	// cards' rows (one per board holding the issue) and kept issues, the
	// details, and the boards.
	"ReadMirrorRefreshTargets": 1,
	// The detached roots and the retired boards, from partial indexes
	// sized by the work pending.
	"FinishMirrorCleanup":         2,
	"finishMirrorHistoryStagings": 1, // the dirty histories, likewise
	"ListMirrorHistoryDue":        1, // a page of the history-due indexes, each a seek of due issues alone
	"probeMirrorHistory":          2, // one issue's removals, and a chunk of its changes by key
}

// mirrorAttachedReads are the functions that begin a transaction on a
// connection of the caller's, which reads this store's file attached
// read-only, not on the read pool; each runs under a history read slot
// its caller takes (TestMirrorReadsTakeAHistoryReadSlot lists them).
var mirrorAttachedReads = map[string]string{
	"exportMirrorBoardTx": "a dashboard's own database with the file attached; ExportMirrorBoard takes the slot",
}

// No mirror read opens a read transaction, or a read whose cost grows with
// a board or a root, on the read pool without a history read slot: a
// read transaction opens through mirrorReadSnapshot, a transaction on
// the pool otherwise is the writer's, one on a caller's connection is one
// of mirrorAttachedReads, and a direct read of the pool is one of
// mirrorPoolReads. Go cannot make the pool private to one helper
// inside the package, so the source is checked.
func TestMirrorReadsGoThroughTheSlot(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := map[string]int{}
	snapshots := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "tracker_cache") || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			where := "package level"
			if fn != nil {
				where = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if n.Sel.Name == "read" {
						t.Errorf("%s: %s names the read pool; read through mirrorReadSnapshot or reader()", fset.Position(n.Pos()), where)
					}
				case *ast.CallExpr:
					switch fun := n.Fun.(type) {
					case *ast.Ident:
						switch fun.Name {
						case "readSnapshot", "readSnapshotContext":
							t.Errorf("%s: %s opens a read snapshot without a history read slot; use mirrorReadSnapshot", fset.Position(n.Pos()), where)
						case "historyReadSnapshot":
							if where != "mirrorReadSnapshot" {
								t.Errorf("%s: %s opens a history read snapshot itself; use mirrorReadSnapshot", fset.Position(n.Pos()), where)
							}
						case "mirrorReadSnapshot":
							snapshots++
						}
					case *ast.SelectorExpr:
						switch fun.Sel.Name {
						case "reader":
							found[where]++
						case "Begin", "BeginTx":
							if _, attached := mirrorAttachedReads[where]; attached {
								break
							}
							if db, ok := fun.X.(*ast.SelectorExpr); !ok || db.Sel.Name != "db" {
								t.Errorf("%s: %s begins a transaction off the writer; open a read snapshot with mirrorReadSnapshot", fset.Position(n.Pos()), where)
							}
						}
					}
				}
				return true
			})
		}
	}
	if !maps.Equal(found, mirrorPoolReads) {
		t.Errorf("direct read pool reads by function = %v\nwant %v: a read whose cost grows with a board or a root, or a read of more than one statement, goes through mirrorReadSnapshot; a bounded probe is listed in mirrorPoolReads", found, mirrorPoolReads)
	}
	// The scan still finds the reads it is meant to find.
	if snapshots < 8 {
		t.Fatalf("found %d mirrorReadSnapshot calls; want at least the 8 mirror reads that walk a board or a root", snapshots)
	}
}

// A mirror read that walks a board or a root waits for a history read slot
// before it takes a read connection. While reads in flight hold every slot
// and a connection each, five board patches and every other such read wait
// holding no connection, and a single-statement read still answers on the
// pool's last one; each runs once the slots free.
func TestMirrorReadsTakeAHistoryReadSlot(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	ids := patchBoard(t, s, root, 30)
	export := exportConn(t)
	releases := holdHistoryReadSlots(t, s, true)
	type read struct {
		name string
		read func() error
	}
	var reads []read
	for range 5 {
		reads = append(reads, read{"ReadMirrorBoardPatch", func() error {
			_, ok, err := s.ReadMirrorBoardPatch(root, "10", MirrorRegroup{}, patchSections, ids[:3], MirrorTotalsAt{})
			if err == nil && !ok {
				t.Error("the patch read no board")
			}
			return err
		}})
	}
	reads = append(reads,
		read{"ReadMirrorBoardView", func() error {
			_, _, err := s.ReadMirrorBoardView(root, "10", MirrorRegroup{}, theseSections(patchSections))
			return err
		}},
		read{"ListMirrorSection", func() error {
			_, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, MirrorSection{Kind: MirrorSectionBacklog}, MirrorRowKey{})
			return err
		}},
		read{"CountMirrorSections", func() error { _, err := s.CountMirrorSections(root, "10", MirrorRegroup{}); return err }},
		read{"GetMirrorBoard", func() error { _, _, err := s.GetMirrorBoard(root, "10"); return err }},
		read{"ListMirrorOffBoardLinkedIssues", func() error { _, _, err := s.ListMirrorOffBoardLinkedIssues(root, "", []string{"10"}); return err }},
		read{"FinishMirrorCleanup", func() error { return s.FinishMirrorCleanup(context.Background(), nil) }},
		read{"ExportMirrorBoard", func() error {
			_, _, err := s.ExportMirrorBoard(context.Background(), export, root, "10", MirrorRegroup{}, MirrorExportTables, 0, math.MaxInt64, MirrorLoggedDays{}, CardShapeForTest)
			return err
		}},
	)
	done := make([]chan error, len(reads))
	for i, r := range reads {
		done[i] = make(chan error, 1)
		go func() { done[i] <- r.read() }()
	}
	// Let every read reach its wait.
	time.Sleep(100 * time.Millisecond)
	for i, r := range reads {
		select {
		case err := <-done[i]:
			t.Fatalf("%s ran with every history read slot held: %v", r.name, err)
		default:
		}
	}
	if inUse := s.read.Stats().InUse; inUse != historyReadSlots {
		t.Errorf("%d read connections in use; want only the %d the slots' holders took", inUse, historyReadSlots)
	}
	requireReadRuns(t, "a single-statement read", func() error {
		var one int
		return s.reader().QueryRow("SELECT 1").Scan(&one)
	})
	requireReadRuns(t, "a mirror lookup", func() error { _, _, err := s.GetMirrorSync(root, "10", testNow); return err })
	for _, release := range releases {
		release()
	}
	for i, r := range reads {
		select {
		case err := <-done[i]:
			if err != nil {
				t.Errorf("%s: %v", r.name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not run after the slots freed", r.name)
		}
	}
}
