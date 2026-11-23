package treecheck

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// Server-push payloads are addressed by the ENTITY they describe — a cwd, a PR
// key, a thread id — never by the subscription that happens to be listening.
// A subscription-keyed event forces every consumer to keep a private copy
// filtered by its own handle, which is exactly how two panes on one worktree
// ended up disagreeing about whether there was anything to commit (audit
// 2026-08-08). Entity keying is what lets one observation heal every consumer.
//
// The subscription id itself stays legitimate on the RPC RESULT that hands out
// the unsubscribe/cleanup handle (GitStatusSubscriptionResult.ID) — that is a
// per-caller lease, not an address. So the rule is about the wire NAME:
// nothing that gets serialized may be called "subscriptionId".
//
// Doctrine: internal/transport/AGENTS.md → "Events are entity-keyed";
// frontend/CLAUDE.md → "State Boundaries".

// bannedWireNames are the serialized names no field may carry, compared
// lowercased so SubscriptionId / subscriptionID / SubscriptionID all trip
// them. The set is the same idea spelled the ways a future stream is
// plausibly named — a tripwire, not an exhaustive taxonomy: anything that
// addresses a payload by the LEASE rather than by the entity belongs here.
var bannedWireNames = map[string]string{
	"subscriptionid": "subscription handle",
	"subid":          "subscription handle",
	"streamid":       "stream handle",
	"handleid":       "lease handle",
	"watcherid":      "watcher handle",
}

// exemptWireNames carves out the one shape where a per-caller handle IS the
// entity, keyed by file so it cannot widen by accident.
//
// A byte-stream MULTIPLEXER is not an event: the CDP tunnel interleaves N
// live TCP connections over one WebSocket, and the stream number is the
// only identity such a connection has. There is no cwd, thread id or PR key
// to key by, and no consumer is filtering a shared observation down to its
// own slice — the two sides are the two ends of one socket. Every
// multiplexer on the wire (HTTP/2, SSH, yamux) numbers streams for exactly
// this reason.
//
// Renaming the field to dodge the tripwire would have been the wrong fix:
// it would keep the shape and blind the guard. If a second entry ever wants
// to land here, that is the moment to check it is a multiplexer and not an
// event that has run out of a better key.
var exemptWireNames = map[string]map[string]bool{
	"internal/webview2host/cdpframe.go": {"streamid": true},
}

// wirePayloadCheck fails on a serialized field named by a per-caller
// handle. Test files are scanned too: a fixture that reintroduces the shape
// is as much a regression as production code doing it.
func wirePayloadCheck() goFileCheck {
	var found []string
	return goFileCheck{
		name: "WirePayloadsAreEntityKeyedNotSubscriptionKeyed",
		visit: func(fset *token.FileSet, path string, file *ast.File) {
			exempt := exemptWireNames[path]
			ast.Inspect(file, func(node ast.Node) bool {
				structType, ok := node.(*ast.StructType)
				if !ok || structType.Fields == nil {
					return true
				}
				// An untagged field on a serialized struct still goes over the wire
				// under its Go name, so untagged fields are checked too — but only
				// on structs that are demonstrably wire shapes (at least one json
				// tag). Go-internal bookkeeping that tracks subscription handles by
				// field name is not this rule's business.
				wireShape := structHasJSONTag(structType)
				for _, field := range structType.Fields.List {
					name, tagged, serialized := jsonFieldName(field)
					if !serialized || (!tagged && !wireShape) {
						continue
					}
					kind, banned := bannedWireNames[strings.ToLower(name)]
					if !banned || exempt[strings.ToLower(name)] {
						continue
					}
					found = append(found, fmt.Sprintf(
						"%s:%d serializes %q (a %s) — events are keyed by their entity (cwd, PR key, thread id), "+
							"never by a per-caller handle; see internal/transport/AGENTS.md",
						path, fset.Position(field.Pos()).Line, name, kind,
					))
				}
				return true
			})
		},
		report: func(t *testing.T) {
			for _, finding := range found {
				t.Error(finding)
			}
			// A carve-out for a file that no longer exists is dead prose that would
			// quietly re-arm somewhere else if the path were ever reused.
			for file := range exemptWireNames {
				if _, err := os.Stat(reporoot.Path(t, file)); err != nil {
					t.Errorf("wire-name exemption names %s, which is not there: %v", file, err)
				}
			}
		},
	}
}

func structHasJSONTag(structType *ast.StructType) bool {
	for _, field := range structType.Fields.List {
		if _, ok := jsonTag(field); ok {
			return true
		}
	}
	return false
}

// jsonFieldName resolves the name a field serializes under: the json tag's name
// when it sets one, otherwise the Go field name (encoding/json's default).
// `tagged` reports whether a json tag was present; `serialized` is false for
// embedded fields and `json:"-"`, which carry no name of their own.
func jsonFieldName(field *ast.Field) (name string, tagged bool, serialized bool) {
	if len(field.Names) == 0 {
		return "", false, false
	}
	goName := field.Names[0].Name
	tag, ok := jsonTag(field)
	if !ok {
		return goName, false, true
	}
	tagName, _, _ := strings.Cut(tag, ",")
	switch tagName {
	case "-":
		return "", true, false
	case "":
		return goName, true, true
	default:
		return tagName, true, true
	}
}

func jsonTag(field *ast.Field) (string, bool) {
	if field.Tag == nil {
		return "", false
	}
	raw, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		// The parser only produces a valid string literal here; an unquotable
		// tag would be a silent hole, so fail loudly rather than skipping.
		panic("unquotable struct tag literal: " + field.Tag.Value)
	}
	return reflect.StructTag(raw).Lookup("json")
}
