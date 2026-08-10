package def

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// accountingWorkflow is the opted-in shape: a dynamic fan-out whose join must
// account for its units through the phase's `merged` / `blocked` outputs.
func accountingWorkflow() Workflow {
	workflow := dynamicFanOutWorkflow()
	phase := &workflow.Phases[1]
	phase.Join.AccountsForUnits = true
	phase.Outputs = accountingOutputs()
	return workflow
}

func accountingOutputs() map[string]Variable {
	return map[string]Variable{
		JoinMergedOutput: {Schema: JSONSchema{Type: "array", Items: &JSONSchema{Type: "string"}}},
		JoinBlockedOutput: {Schema: JSONSchema{Type: "array", Items: &JSONSchema{
			Type: "object",
			Properties: map[string]JSONSchema{
				JoinBlockedUnitField:   {Type: "string"},
				JoinBlockedReasonField: {Type: "string"},
			},
			Required: []string{JoinBlockedUnitField, JoinBlockedReasonField},
		}}},
	}
}

func TestAccountingJoinValidatesWithTheDeclaredContract(t *testing.T) {
	requireValid(t, Validate(fanOutFixture(t, accountingWorkflow(), fanOutPrompts()), validBindings(), nil))
}

// The declarations and the run-time verification have to agree. A definition
// whose `merged` is missing or the wrong shape would validate clean and then
// fail every join attempt it ever ran — which is the class of error a dry-run
// exists to catch.
func TestAccountingJoinRequiresTheDeclaredOutputs(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		outputs  func(map[string]Variable)
		contains string
	}{
		{"merged absent", func(o map[string]Variable) { delete(o, JoinMergedOutput) }, `must answer an output "merged"`},
		{"blocked absent", func(o map[string]Variable) { delete(o, JoinBlockedOutput) }, `must answer an output "blocked"`},
		{"merged not an array", func(o map[string]Variable) {
			o[JoinMergedOutput] = Variable{Schema: JSONSchema{Type: "boolean"}}
		}, `output "merged" must be an array of strings`},
		{"merged items not strings", func(o map[string]Variable) {
			o[JoinMergedOutput] = Variable{Schema: JSONSchema{Type: "array", Items: &JSONSchema{Type: "number"}}}
		}, `output "merged" must be an array of strings`},
		{"merged optional", func(o map[string]Variable) {
			declaration := o[JoinMergedOutput]
			declaration.Optional = true
			o[JoinMergedOutput] = declaration
		}, `output "merged" must not be optional`},
		{"blocked items not objects", func(o map[string]Variable) {
			o[JoinBlockedOutput] = Variable{Schema: JSONSchema{Type: "array", Items: &JSONSchema{Type: "string"}}}
		}, `output "blocked" must be an array of objects`},
		{"blocked missing reason", func(o map[string]Variable) {
			o[JoinBlockedOutput] = Variable{Schema: JSONSchema{Type: "array", Items: &JSONSchema{
				Type:       "object",
				Properties: map[string]JSONSchema{JoinBlockedUnitField: {Type: "string"}},
				Required:   []string{JoinBlockedUnitField},
			}}}
		}, `items must declare a string property "reason"`},
		{"blocked reason optional", func(o map[string]Variable) {
			o[JoinBlockedOutput] = Variable{Schema: JSONSchema{Type: "array", Items: &JSONSchema{
				Type: "object",
				Properties: map[string]JSONSchema{
					JoinBlockedUnitField:   {Type: "string"},
					JoinBlockedReasonField: {Type: "string"},
				},
				Required: []string{JoinBlockedUnitField},
			}}}
		}, `items must require "reason"`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workflow := accountingWorkflow()
			testCase.outputs(workflow.Phases[1].Outputs)
			result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil)
			finding := requireFinding(t, result, "join.accounting", testCase.contains)
			if !strings.Contains(finding.Element, `phase "port"`) {
				t.Fatalf("the finding must name the phase whose outputs need editing: %s", finding.Element)
			}
		})
	}
}

// A work unit consolidates nothing and answers its own envelope, so the
// obligation would hold it to a contract it does not own.
func TestAccountsForUnitsIsRefusedOutsideAJoin(t *testing.T) {
	t.Run("dynamic template", func(t *testing.T) {
		workflow := accountingWorkflow()
		workflow.Phases[1].Unit.AccountsForUnits = true
		requireFinding(t, Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil),
			"join.accounting", "accounts_for_units is valid on a join only")
	})
	t.Run("static unit", func(t *testing.T) {
		workflow := staticFanOutWorkflow()
		workflow.Phases[1].Join.AccountsForUnits = true
		workflow.Phases[1].Outputs = accountingOutputs()
		workflow.Phases[1].FanOut[0].AccountsForUnits = true
		requireFinding(t, Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil),
			"join.accounting", "accounts_for_units is valid on a join only")
	})
}

// A join that did NOT opt in is unchanged: the contract it answers is exactly
// the phase's, with no accounting obligation attached.
func TestJoinEnvelopeWithoutOptInCarriesNoObligation(t *testing.T) {
	phase := accountingWorkflow().Phases[1]
	phase.Join.AccountsForUnits = false
	contract := JoinEnvelope(phase, []string{"port-section-0"})
	payload := []byte(`{"status":"done","outputs":{"merged":[],"blocked":[]}}`)
	if err := contract.Validate(payload); err != nil {
		t.Fatalf("a join that did not opt in was held to the contract: %v", err)
	}
}

func accountingFindings(t *testing.T, unitIDs []string, payload string) []EnvelopeFinding {
	t.Helper()
	err := JoinEnvelope(accountingWorkflow().Phases[1], unitIDs).Validate([]byte(payload))
	if err == nil {
		return nil
	}
	var validation *EnvelopeValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("envelope refusal is not the ordinary validation error: %T %v", err, err)
	}
	return validation.Findings
}

func requireEnvelopeFinding(t *testing.T, findings []EnvelopeFinding, path, contains string) {
	t.Helper()
	for _, finding := range findings {
		if finding.Path == path && strings.Contains(finding.Message, contains) {
			return
		}
	}
	t.Fatalf("no finding at %q containing %q; got %+v", path, contains, findings)
}

// The whole point: a done join envelope must partition the units it ran over.
func TestAccountingVerifiesTheUnitSetOnADoneEnvelope(t *testing.T) {
	units := []string{"port-1", "port-2", "port-3"}

	t.Run("complete accounting passes", func(t *testing.T) {
		findings := accountingFindings(t, units,
			`{"status":"done","outputs":{"merged":["port-1","port-2"],`+
				`"blocked":[{"unit":"port-3","reason":"conflicts in a.go"}]}}`)
		if len(findings) != 0 {
			t.Fatalf("a complete accounting was refused: %+v", findings)
		}
	})

	// The live failure: a merge that gave up early reported the lanes it took
	// and said nothing at all about the one it dropped.
	t.Run("missing unit is named", func(t *testing.T) {
		findings := accountingFindings(t, units,
			`{"status":"done","outputs":{"merged":["port-1","port-2"],"blocked":[]}}`)
		requireEnvelopeFinding(t, findings, "$.outputs", `unit "port-3" is neither merged nor blocked`)
	})

	t.Run("unknown unit is refused", func(t *testing.T) {
		findings := accountingFindings(t, units,
			`{"status":"done","outputs":{"merged":["port-1","port-2","port-3","port-9"],"blocked":[]}}`)
		requireEnvelopeFinding(t, findings, "$.outputs.merged[3]", `unit "port-9" is not one of the units this join ran over`)
	})

	t.Run("a unit accounted twice is refused", func(t *testing.T) {
		findings := accountingFindings(t, units,
			`{"status":"done","outputs":{"merged":["port-1","port-2","port-3"],`+
				`"blocked":[{"unit":"port-3","reason":"conflicts"}]}}`)
		requireEnvelopeFinding(t, findings, "$.outputs.blocked[0].unit", `unit "port-3" is accounted for more than once`)
	})

	t.Run("a blocked unit needs a reason", func(t *testing.T) {
		findings := accountingFindings(t, units,
			`{"status":"done","outputs":{"merged":["port-1","port-2"],`+
				`"blocked":[{"unit":"port-3","reason":"   "}]}}`)
		requireEnvelopeFinding(t, findings, "$.outputs.blocked[0].reason", "must say why the unit could not be taken")
	})

	// A join over zero units still owes two empty lists — the flag and the set
	// are separate facts, so an empty set must not read as "no obligation".
	t.Run("zero units still owes empty lists", func(t *testing.T) {
		if findings := accountingFindings(t, []string{},
			`{"status":"done","outputs":{"merged":[],"blocked":[]}}`); len(findings) != 0 {
			t.Fatalf("an empty accounting over zero units was refused: %+v", findings)
		}
		findings := accountingFindings(t, []string{},
			`{"status":"done","outputs":{"merged":["ghost"],"blocked":[]}}`)
		requireEnvelopeFinding(t, findings, "$.outputs.merged[0]", `unit "ghost" is not one of the units this join ran over`)
	})
}

// A join that asks a question or gets stuck produced no result to account for,
// and demanding the lists there would refuse the very envelope that says the
// join could not decide.
func TestAccountingAppliesToDoneEnvelopesOnly(t *testing.T) {
	for _, payload := range []string{
		`{"status":"question","question":"which side wins in a.go?"}`,
		`{"status":"stuck","reason":"the campaign branch is mid-merge"}`,
	} {
		if findings := accountingFindings(t, []string{"port-1"}, payload); len(findings) != 0 {
			t.Fatalf("a non-done envelope was held to the accounting: %+v", findings)
		}
	}
}

// One mistake produces one finding. A `merged` the element could not answer is
// reported once by the declared-output rules; re-reporting it here as "every
// unit is unaccounted for" would bury the finding the join has to act on.
func TestAccountingDoesNotPileOntoAnUnreadableList(t *testing.T) {
	findings := accountingFindings(t, []string{"port-1", "port-2"},
		`{"status":"done","outputs":{"merged":null,"blocked":[]}}`)
	for _, finding := range findings {
		if strings.Contains(finding.Message, "neither merged nor blocked") {
			t.Fatalf("an unreadable merged list produced per-unit accounting noise: %+v", findings)
		}
	}
	requireEnvelopeFinding(t, findings, "$.outputs.merged", "required output must not be null when status is done")
}

// The refusal must be ordinary envelope-validation feedback so it rides the
// existing retry-with-feedback path (D44) rather than becoming a park or a new
// failure state.
func TestAccountingRefusalIsOrdinaryEnvelopeFeedback(t *testing.T) {
	err := JoinEnvelope(accountingWorkflow().Phases[1], []string{"port-1"}).
		Validate([]byte(`{"status":"done","outputs":{"merged":[],"blocked":[]}}`))
	var validation *EnvelopeValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("accounting refusal = %T %v, want *EnvelopeValidationError", err, err)
	}
	if !strings.Contains(validation.Error(), "neither merged nor blocked") {
		t.Fatalf("refusal text does not name the unaccounted unit: %s", validation.Error())
	}
}

// The generated schema is unchanged by the opt-in: JSON Schema cannot express
// "these two arrays partition this set", so the obligation lives in
// post-validation and in the prompt, and the wire contract stays identical.
func TestAccountingDoesNotChangeTheGeneratedSchema(t *testing.T) {
	phase := accountingWorkflow().Phases[1]
	withAccounting, err := JoinEnvelope(phase, []string{"port-1"}).Schema()
	if err != nil {
		t.Fatal(err)
	}
	plain, err := PhaseEnvelope(phase).Schema()
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(withAccounting) || string(withAccounting) != string(plain) {
		t.Fatalf("accounting changed the generated schema:\n%s\n%s", withAccounting, plain)
	}
}

// The set the join is JUDGED against is read from the same reserved binding it
// is SHOWN, so the two can never disagree.
func TestUnitIDsFromResultsReadsTheReservedBinding(t *testing.T) {
	ids := UnitIDsFromResults(map[string]any{UnitsVariable: []any{
		map[string]any{"id": "port-1", "index": 0, "status": "done"},
		map[string]any{"id": "port-2", "index": 1, "status": "dropped"},
		map[string]any{"index": 2},
		"not an entry",
	}})
	if strings.Join(ids, ",") != "port-1,port-2" {
		t.Fatalf("UnitIDsFromResults = %v", ids)
	}
	if got := UnitIDsFromResults(map[string]any{}); got != nil {
		t.Fatalf("an absent binding yielded %v", got)
	}
}

// An entry that is not a usable unit id is reported where it sits. Skipping it
// would leave the unit it was meant to account for reported as missing while the
// junk that caused that survives the very retry which fixed everything named —
// and a tool join hand-writes its envelope under no schema at all, so nothing
// else catches it.
func TestAccountingReportsMalformedEntriesRatherThanSkippingThem(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		payload  string
		path     string
		contains string
	}{
		{"blank merged entry", `{"status":"done","outputs":{"merged":["  "],"blocked":[]}}`,
			"$.outputs.merged[0]", "a blank entry accounts for nothing"},
		{"non-string merged entry", `{"status":"done","outputs":{"merged":[7],"blocked":[]}}`,
			"$.outputs.merged[0]", "must be a unit id string"},
		{"non-object blocked entry", `{"status":"done","outputs":{"merged":[],"blocked":["port-1"]}}`,
			"$.outputs.blocked[0]", "must be a {unit, reason} object"},
		{"blocked entry with no unit", `{"status":"done","outputs":{"merged":[],"blocked":[{"reason":"conflicts"}]}}`,
			"$.outputs.blocked[0].unit", "must be a unit id string"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			findings := accountingFindings(t, []string{"port-1"}, testCase.payload)
			requireEnvelopeFinding(t, findings, testCase.path, testCase.contains)
			// The unit it failed to account for is still named, so one retry can
			// fix both halves.
			requireEnvelopeFinding(t, findings, "$.outputs", `unit "port-1" is neither merged nor blocked`)
		})
	}
}
