package def

import (
	"fmt"
	"reflect"
	"strings"
)

func validateVariables(workflow Workflow, phaseIndex map[string]int, graph workflowGraph) []Finding {
	var findings []Finding
	for consumerIndex, phase := range workflow.Phases {
		for ref, consumer := range phase.Inputs {
			element := fmt.Sprintf("workflow %q phase %q input %q", workflow.ID, phase.ID, ref)
			findings = append(findings, validateSchemaDefinition(consumer.Schema, element)...)
			producer, producerIndex, ok := resolveReference(workflow, phaseIndex, ref)
			if !ok {
				findings = append(findings, finding("variable.unresolved", element, fmt.Sprintf("reference %q does not resolve", ref)))
				continue
			}
			if !consumer.Optional && producerIndex >= 0 && (producerIndex == consumerIndex || !graph.dominators[consumerIndex][producerIndex]) {
				findings = append(findings, finding("variable.dominance", element, fmt.Sprintf("producer phase %q does not dominate phase %q", workflow.Phases[producerIndex].ID, phase.ID)))
			}
			if producer.Optional && !consumer.Optional {
				findings = append(findings, finding("variable.optionality", element, fmt.Sprintf("optional producer %q requires this input to be optional", ref)))
			}
			if !schemasCompatible(producer.Schema, consumer.Schema) {
				findings = append(findings, finding("variable.type", element, fmt.Sprintf("declared type %q does not match producer type %q", consumer.Schema.Type, producer.Schema.Type)))
			}
		}
		for routeIndex, route := range phase.Gate.Routes {
			if route.When != nil {
				findings = append(findings, validatePredicate(workflow, phaseIndex, graph, consumerIndex, routeIndex, *route.When)...)
			}
			for _, feedback := range route.Feedback {
				validateFeedbackRef(workflow, phaseIndex, graph, consumerIndex, routeIndex, feedback, &findings)
			}
			if route.Human != nil && route.Human.Reject != nil {
				for _, feedback := range route.Human.Reject.Feedback {
					validateFeedbackRef(workflow, phaseIndex, graph, consumerIndex, routeIndex, feedback, &findings)
				}
			}
		}
	}
	return findings
}

func resolveReference(workflow Workflow, phaseIndex map[string]int, ref string) (Variable, int, bool) {
	parts := strings.Split(ref, ".")
	if input, ok := workflow.Inputs[parts[0]]; ok {
		resolved, ok := descendVariable(input, parts[1:])
		return resolved, -1, ok
	}
	if len(parts) < 2 {
		return Variable{}, -1, false
	}
	index, ok := phaseIndex[parts[0]]
	if !ok {
		return Variable{}, -1, false
	}
	output, ok := PhaseOutputs(workflow.Phases[index])[parts[1]]
	if !ok {
		return Variable{}, -1, false
	}
	resolved, ok := descendVariable(output, parts[2:])
	return resolved, index, ok
}

func descendVariable(variable Variable, path []string) (Variable, bool) {
	schema := variable.Schema
	optional := variable.Optional
	for _, field := range path {
		if schema.Type != "object" {
			return Variable{}, false
		}
		child, ok := schema.Properties[field]
		if !ok {
			return Variable{}, false
		}
		if !contains(schema.Required, field) {
			optional = true
		}
		schema = child
	}
	return Variable{Schema: schema, Optional: optional}, true
}

func schemasCompatible(producer, consumer JSONSchema) bool {
	if producer.Type != consumer.Type {
		return false
	}
	if len(consumer.Enum) > 0 {
		if len(producer.Enum) == 0 {
			return false
		}
		for _, produced := range producer.Enum {
			matched := false
			for _, accepted := range consumer.Enum {
				if reflect.DeepEqual(produced, accepted) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	if producer.Type == "array" && producer.Items != nil && consumer.Items != nil {
		if !schemasCompatible(*producer.Items, *consumer.Items) {
			return false
		}
	}
	if producer.Type == "object" {
		if consumer.AdditionalProperties != nil && !*consumer.AdditionalProperties {
			if producer.AdditionalProperties == nil || *producer.AdditionalProperties {
				return false
			}
			for name := range producer.Properties {
				if _, ok := consumer.Properties[name]; !ok {
					return false
				}
			}
		}
		for name, consumerProperty := range consumer.Properties {
			producerProperty, produced := producer.Properties[name]
			if contains(consumer.Required, name) && (!produced || !contains(producer.Required, name)) {
				return false
			}
			if produced && !schemasCompatible(producerProperty, consumerProperty) {
				return false
			}
		}
	}
	if !boundsContained(producer.Minimum, producer.Maximum, consumer.Minimum, consumer.Maximum) ||
		!intBoundsContained(producer.MinLength, producer.MaxLength, consumer.MinLength, consumer.MaxLength) ||
		!intBoundsContained(producer.MinItems, producer.MaxItems, consumer.MinItems, consumer.MaxItems) {
		return false
	}
	if consumer.Format != "" && producer.Format != consumer.Format {
		return false
	}
	return true
}

func boundsContained(producerMin, producerMax, consumerMin, consumerMax *float64) bool {
	return (consumerMin == nil || producerMin != nil && *producerMin >= *consumerMin) &&
		(consumerMax == nil || producerMax != nil && *producerMax <= *consumerMax)
}

func intBoundsContained(producerMin, producerMax, consumerMin, consumerMax *int) bool {
	return (consumerMin == nil || producerMin != nil && *producerMin >= *consumerMin) &&
		(consumerMax == nil || producerMax != nil && *producerMax <= *consumerMax)
}

func validateFeedbackRef(workflow Workflow, phaseIndex map[string]int, graph workflowGraph, consumer, route int, ref string, findings *[]Finding) {
	_, producer, ok := resolveReference(workflow, phaseIndex, ref)
	element := fmt.Sprintf("workflow %q phase %q route %d", workflow.ID, workflow.Phases[consumer].ID, route)
	if !ok {
		*findings = append(*findings, finding("gate.feedback", element, fmt.Sprintf("feedback reference %q does not resolve", ref)))
		return
	}
	if producer >= 0 && producer != consumer && !graph.dominators[consumer][producer] {
		*findings = append(*findings, finding("gate.feedback", element, fmt.Sprintf("producer phase %q for feedback %q does not dominate this phase", workflow.Phases[producer].ID, ref)))
	}
}

func validatePredicate(workflow Workflow, phaseIndex map[string]int, graph workflowGraph, phase, route int, predicate Predicate) []Finding {
	element := fmt.Sprintf("workflow %q phase %q route %d predicate", workflow.ID, workflow.Phases[phase].ID, route)
	operators := 0
	if predicate.Eq != nil {
		operators++
	}
	if predicate.Neq != nil {
		operators++
	}
	if predicate.Gt != nil {
		operators++
	}
	if predicate.Gte != nil {
		operators++
	}
	if predicate.Lt != nil {
		operators++
	}
	if predicate.Lte != nil {
		operators++
	}
	if predicate.In != nil {
		operators++
	}
	if predicate.Exists != "" {
		operators++
	}
	if predicate.All != nil {
		operators++
	}
	if predicate.Any != nil {
		operators++
	}
	if predicate.Not != nil {
		operators++
	}
	if operators != 1 {
		return []Finding{finding("predicate.operator", element, "predicate must declare exactly one supported operator")}
	}
	var findings []Finding
	if predicate.All != nil && len(predicate.All) == 0 {
		findings = append(findings, finding("predicate.all", element, "all requires at least one predicate"))
	}
	if predicate.Any != nil && len(predicate.Any) == 0 {
		findings = append(findings, finding("predicate.any", element, "any requires at least one predicate"))
	}
	comparisons := []struct {
		name  string
		value *Comparison
	}{
		{"eq", predicate.Eq}, {"neq", predicate.Neq}, {"gt", predicate.Gt},
		{"gte", predicate.Gte}, {"lt", predicate.Lt}, {"lte", predicate.Lte},
	}
	for _, comparison := range comparisons {
		if comparison.value == nil {
			continue
		}
		variable, ok := validatePredicateRef(workflow, phaseIndex, graph, phase, comparison.value.Ref, element, &findings)
		if ok && !literalMatches(variable.Schema, comparison.value.Value) {
			findings = append(findings, finding("predicate.type", element, fmt.Sprintf("%s reference %q type %q does not match value", comparison.name, comparison.value.Ref, variable.Schema.Type)))
		}
		if ok && (comparison.name == "gt" || comparison.name == "gte" || comparison.name == "lt" || comparison.name == "lte") && variable.Schema.Type != "number" {
			findings = append(findings, finding("predicate.type", element, fmt.Sprintf("%s reference %q must be numeric", comparison.name, comparison.value.Ref)))
		}
	}
	if predicate.In != nil {
		variable, ok := validatePredicateRef(workflow, phaseIndex, graph, phase, predicate.In.Ref, element, &findings)
		if len(predicate.In.Values) == 0 {
			findings = append(findings, finding("predicate.in", element, "in requires at least one value"))
		}
		if ok {
			for _, value := range predicate.In.Values {
				if !literalMatches(variable.Schema, value) {
					findings = append(findings, finding("predicate.type", element, fmt.Sprintf("in reference %q type %q does not match a value", predicate.In.Ref, variable.Schema.Type)))
					break
				}
			}
		}
	}
	if predicate.Exists != "" {
		validatePredicateRef(workflow, phaseIndex, graph, phase, predicate.Exists, element, &findings)
	}
	for _, child := range predicate.All {
		findings = append(findings, validatePredicate(workflow, phaseIndex, graph, phase, route, child)...)
	}
	for _, child := range predicate.Any {
		findings = append(findings, validatePredicate(workflow, phaseIndex, graph, phase, route, child)...)
	}
	if predicate.Not != nil {
		findings = append(findings, validatePredicate(workflow, phaseIndex, graph, phase, route, *predicate.Not)...)
	}
	return findings
}

func validatePredicateRef(workflow Workflow, phaseIndex map[string]int, graph workflowGraph, consumer int, ref, element string, findings *[]Finding) (Variable, bool) {
	variable, producer, ok := resolveReference(workflow, phaseIndex, ref)
	if !ok {
		*findings = append(*findings, finding("predicate.ref", element, fmt.Sprintf("reference %q does not resolve", ref)))
		return Variable{}, false
	}
	// A phase gate is evaluated after that phase emits its outputs, so the
	// current phase is a valid predicate producer. Earlier producers must
	// dominate the phase.
	if producer >= 0 && producer != consumer && !graph.dominators[consumer][producer] && !variable.Optional {
		*findings = append(*findings, finding("predicate.dominance", element, fmt.Sprintf("producer phase %q for %q does not dominate this phase", workflow.Phases[producer].ID, ref)))
	}
	return variable, true
}

func literalMatches(schema JSONSchema, value any) bool {
	switch schema.Type {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		switch value.(type) {
		case int, int64, uint64, float32, float64:
			return true
		}
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	}
	return false
}
