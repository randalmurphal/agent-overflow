package transport

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"agent-overflow/internal/eventchan"
)

// The correspondence gate between the two halves of one table: the
// SPELLING (internal/eventchan's constants, which every emit site is
// typed against) and the POLICY (channelPolicies here, which decides
// audience + retention). Either half missing its counterpart is the
// failure this file exists to catch:
//
//   - a constant with no row emits onto the fail-closed loopback-only
//     default, so a remote client silently stops receiving it, and
//     nobody decided that;
//   - a row with no constant is a policy for a channel no emit site can
//     name, which means either a deleted channel or a typo one side of.
//
// The constants are enumerated by parsing the eventchan package's source
// rather than from a hand-maintained All() slice, so there is no third
// list to forget. The technique mirrors
// internal/store/migrate_freeze_test.go.

// eventChannelConstants parses ../eventchan and returns every
// Channel-typed constant as name → value. Failing the test on a parse
// error is deliberate: silently returning nothing would make both
// directions pass vacuously.
func eventChannelConstants(t *testing.T) map[string]eventchan.Channel {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Join("..", "eventchan"), nil, 0)
	if err != nil {
		t.Fatalf("parse internal/eventchan: %v", err)
	}
	pkg, ok := pkgs["eventchan"]
	if !ok {
		t.Fatalf("internal/eventchan holds no package named eventchan (found %d packages)", len(pkgs))
	}

	constants := make(map[string]eventchan.Channel)
	byValue := make(map[eventchan.Channel]string)
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			// A grouped const carries its type forward when a spec omits
			// it, so track the last one seen rather than reading each spec
			// in isolation.
			var groupType string
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if ident, ok := value.Type.(*ast.Ident); ok {
					groupType = ident.Name
				}
				if groupType != "Channel" {
					continue
				}
				for i, name := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Errorf("eventchan.%s is not a plain string literal; the registry cross-check cannot read it", name.Name)
						continue
					}
					unquoted, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Errorf("eventchan.%s has an unquotable value %s", name.Name, lit.Value)
						continue
					}
					channel := eventchan.Channel(unquoted)
					if first, dup := byValue[channel]; dup {
						t.Errorf("eventchan.%s and eventchan.%s are both %q — one channel, one constant", first, name.Name, unquoted)
					}
					byValue[channel] = name.Name
					constants[name.Name] = channel
				}
			}
		}
	}
	if len(constants) == 0 {
		t.Fatal("parsed no Channel constants out of internal/eventchan; the cross-check would pass vacuously")
	}
	return constants
}

// TestEveryEventChannelConstantHasAPolicyRow: a constant an emit site can
// name must have a decided audience and retention. Without a row it lands
// on unregisteredChannelPolicy — loopback-only, full ring — which is safe
// but is nobody's decision.
func TestEveryEventChannelConstantHasAPolicyRow(t *testing.T) {
	constants := eventChannelConstants(t)
	for _, name := range sortedConstantNames(constants) {
		channel := constants[name]
		if _, registered := policyForChannel(string(channel)); !registered {
			t.Errorf("eventchan.%s (%q) has no ChannelPolicy row: add one in event_channels.go deciding its Audience and Retention", name, channel)
		}
	}
}

// TestEveryChannelPolicyRowHasAConstant is the other direction. The row's
// Channel field is already eventchan.Channel-typed, so a row cannot hold
// an arbitrary string without an explicit conversion — this catches the
// conversion, and a row left behind by a deleted constant.
func TestEveryChannelPolicyRowHasAConstant(t *testing.T) {
	constants := eventChannelConstants(t)
	known := make(map[eventchan.Channel]string, len(constants))
	for name, channel := range constants {
		known[channel] = name
	}
	for _, policy := range channelPolicies {
		if _, ok := known[policy.Channel]; !ok {
			t.Errorf("channelPolicies row %q has no internal/eventchan constant: add one, or delete the row if the channel is gone", policy.Channel)
		}
	}
}

// TestEventChannelConstantCountMatchesRegistry is the cheap arithmetic
// backstop for the two tests above: equal counts plus both containment
// directions means the two tables are the same set.
func TestEventChannelConstantCountMatchesRegistry(t *testing.T) {
	if got, want := len(eventChannelConstants(t)), len(channelPolicies); got != want {
		t.Errorf("internal/eventchan declares %d channels, channelPolicies holds %d rows", got, want)
	}
}

func sortedConstantNames(constants map[string]eventchan.Channel) []string {
	names := make([]string, 0, len(constants))
	for name := range constants {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
