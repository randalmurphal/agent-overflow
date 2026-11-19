package store

import (
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Each session-ending writer, applied to one of a device's two sessions,
// releases nothing; applied to the second, it releases the device's links
// and returns their threads.
func TestEndingADevicesLastSessionReleasesItsLinks(t *testing.T) {
	writers := map[string]struct {
		// expires is each session's expiry; prepare readies a session for end.
		expires func(i int) int64
		prepare func(t *testing.T, s *Store, i int, session Session)
		end     func(s *Store, i int) ([]string, error)
	}{
		"RevokeSession": {
			end: func(s *Store, i int) ([]string, error) {
				_, released, err := s.RevokeSession(endSessionID(i), 3000)
				return released, err
			},
		},
		"RetireOwnIntroductions": {
			prepare: func(t *testing.T, s *Store, i int, session Session) {
				link := PairingLink{ID: "intro-" + strconv.Itoa(i), UserID: session.UserID, Scopes: []string{"threads:read"}, BindingClass: "device-bound", DeviceClass: "phone",
					Purpose: "own-introduction", ExpectedKey: introKey(i), MemberGeneration: 1, CreatedAt: 1000, ExpiresAt: 9000}
				hash := []byte("hash-" + link.ID)
				if err := s.CreatePairingLink(link, hash); err != nil {
					t.Fatal(err)
				}
				if _, err := s.RedeemPairingLink(hash, 1100, introKey(i)); err != nil {
					t.Fatal(err)
				}
				if err := s.AttachPairingRedemption(link.ID, session.DeviceID, session.ID); err != nil {
					t.Fatal(err)
				}
			},
			end: func(s *Store, i int) ([]string, error) {
				_, released, err := s.RetireOwnIntroductions(introKey(i), 3000)
				return released, err
			},
		},
		"DeleteSessionsExpiredBefore": {
			expires: func(i int) int64 { return int64(4000 + 1000*i) },
			end: func(s *Store, i int) ([]string, error) {
				_, released, err := s.DeleteSessionsExpiredBefore(int64(4500 + 1000*i))
				return released, err
			},
		},
	}
	for name, w := range writers {
		t.Run(name, func(t *testing.T) {
			s, owner, device := seedOwnerDevice(t)
			key := seedSigningKey(t, s)
			other, err := s.CreateDevice(owner.ID, "Other", "desktop", "linux")
			if err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				expires := int64(9000)
				if w.expires != nil {
					expires = w.expires(i)
				}
				session := newTestSession(endSessionID(i), owner, device, key, expires)
				if err := s.CreateSession(session); err != nil {
					t.Fatal(err)
				}
				if w.prepare != nil {
					w.prepare(t, s, i, session)
				}
			}
			if err := s.CreateSession(newTestSession("other-session", owner, other, key, 99000)); err != nil {
				t.Fatal(err)
			}
			mustCreateThread(t, s, "worker")
			mustCreateThread(t, s, "other-worker")
			acceptLink(t, s, "tok", device.ID, "worker", 1000)
			acceptLink(t, s, "tok-other", other.ID, "other-worker", 1000)

			if released, err := w.end(s, 0); err != nil || len(released) != 0 {
				t.Fatalf("ending one of two sessions released %v, %v; want none", released, err)
			}
			if got, _ := placementOf(t, s, "worker"); got != PlacementOwned {
				t.Fatalf("worker with a session left = %s, want owned", got)
			}
			if released, err := w.end(s, 1); err != nil || !slices.Equal(released, []string{"worker"}) {
				t.Fatalf("ending the last session released %v, %v; want [worker]", released, err)
			}
			for id, want := range map[string]string{"worker": PlacementStandalone, "other-worker": PlacementOwned} {
				if got, _ := placementOf(t, s, id); got != want {
					t.Errorf("%s = %s, want %s", id, got, want)
				}
			}
		})
	}
}

func endSessionID(i int) string { return "session-" + strconv.Itoa(i) }

func introKey(i int) string {
	key := make([]byte, 32)
	key[0] = byte(10 + i)
	return base64.RawURLEncoding.EncodeToString(key)
}

// Session-ending SQL: a revocation, a deletion or a replacement of sessions
// rows, and a write that ends them through their parent (a deleted device,
// user or signing key cascades; a revoked device admits none of them).
var (
	sessionsSetRE   = regexp.MustCompile(`(?is)\bUPDATE\s+sessions\s+SET\b(.*?)(\bWHERE\b|$)`)
	sessionsGoneRE  = regexp.MustCompile(`(?i)\b(DELETE\s+FROM|REPLACE\s+INTO|INSERT\s+OR\s+REPLACE\s+INTO)\s+sessions\b`)
	sessionParentRE = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+(devices|users|signing_keys)\b|\bUPDATE\s+devices\s+SET\s+revoked_at\s*=\s*\?`)
)

// Every store function that ends sessions does it through endSessionsTx,
// which releases the links of the devices it leaves unreachable. Go cannot
// make the sessions table private to one function inside the package, and a
// SQLite trigger could release the links but not hand their threads back to
// the writer for announcing, so the source is checked instead: only
// endSessionsTx writes session-ending SQL, and a function that ends sessions
// through their parent row also calls endSessionsTx.
func TestEverySessionEndGoesThroughEndSessionsTx(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	ends, parents := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
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
			endsSessions, endsParent, calls := false, false, false
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BasicLit:
					if n.Kind != token.STRING {
						return true
					}
					text, err := strconv.Unquote(n.Value)
					if err != nil {
						t.Fatalf("%s: %v", fset.Position(n.Pos()), err)
					}
					if m := sessionsSetRE.FindStringSubmatch(text); (m != nil && strings.Contains(strings.ToLower(m[1]), "revoked_at")) || sessionsGoneRE.MatchString(text) {
						endsSessions = true
					}
					if sessionParentRE.MatchString(text) {
						endsParent = true
					}
				case *ast.CallExpr:
					if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "endSessionsTx" {
						calls = true
					}
				}
				return true
			})
			switch {
			case endsSessions && where != "endSessionsTx":
				t.Errorf("%s: %s ends sessions outside endSessionsTx, so it skips the release of its devices' links", name, where)
			case endsSessions:
				ends++
			}
			switch {
			case endsParent && !calls:
				t.Errorf("%s: %s ends sessions through their parent row without endSessionsTx", name, where)
			case endsParent:
				parents++
			}
		}
	}
	// The patterns still find the writes they are meant to find.
	if ends != 1 || parents < 2 {
		t.Fatalf("found %d endSessionsTx and %d parent writers; want 1 and at least RevokeDevice and DeleteDevice", ends, parents)
	}
}
