package def

import (
	"bytes"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Parse decodes exactly one workflow document with strict field checking.
func Parse(r io.Reader) (Workflow, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	var workflow Workflow
	if err := decoder.Decode(&workflow); err != nil {
		return Workflow{}, fmt.Errorf("decode workflow: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Workflow{}, fmt.Errorf("decode workflow: multiple YAML documents are not allowed")
		}
		return Workflow{}, fmt.Errorf("decode workflow trailing document: %w", err)
	}
	return workflow, nil
}

// ParseBytes decodes one strict workflow document.
func ParseBytes(data []byte) (Workflow, error) { return Parse(bytes.NewReader(data)) }

// ParseFile reads and strictly decodes a workflow file.
func ParseFile(path string) (Workflow, error) {
	data, err := readLimitedFile(path, "workflow", MaxDefinitionBytes)
	if err != nil {
		return Workflow{}, err
	}
	workflow, err := ParseBytes(data)
	if err != nil {
		return Workflow{}, fmt.Errorf("parse workflow %q: %w", path, err)
	}
	return workflow, nil
}
