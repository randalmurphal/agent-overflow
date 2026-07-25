package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workspacepath"
)

// WorkflowArtifact is one app-managed file deliverable copied from a phase
// workspace. Files are discovered from the deterministic per-item directory.
type WorkflowArtifact struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
}

// settleDone runs the post-success work every done outcome owes the run,
// whatever produced it. Both execution paths call it, so a tool phase and a tool
// join get the artifact capture and the worktree retirement an agent one gets.
func (r *workflowAppRunner) settleDone(done workflowCompletion, envelope json.RawMessage) {
	// Artifacts are declared against phase outputs, so only the envelope that
	// *is* the phase's can carry one. A work unit's outputs reach the workflow
	// through its join, which captures them when it completes.
	if done.producesPhaseEnvelope() {
		r.captureArtifacts(done, envelope)
	}
	if done.unitKind == engine.UnitJoin {
		r.retireUnitWorktrees(done)
	}
}

func (r *workflowAppRunner) captureArtifacts(done workflowCompletion, envelope json.RawMessage) {
	var control struct {
		Outputs map[string]any `json:"outputs"`
	}
	if err := json.Unmarshal(envelope, &control); err != nil {
		log.Printf("workflow artifact capture %s: decode validated envelope: %v", done.key.ItemID, err)
		r.app.emit("workflow:error", map[string]any{
			"itemId": done.key.ItemID,
			"error":  "workflow artifact capture could not decode the validated envelope; inspect local diagnostics",
		})
		return
	}
	vars := make(map[string]any, len(control.Outputs))
	for name, value := range control.Outputs {
		vars[done.key.PhaseID+"."+name] = value
	}
	names := make([]string, 0, len(done.workflow.Outputs))
	for name := range done.workflow.Outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		output := done.workflow.Outputs[name]
		if !output.Artifact || !strings.HasPrefix(output.From, done.key.PhaseID+".") {
			continue
		}
		value, ok := def.LookupVariable(vars, output.From)
		path, stringOK := value.(string)
		if !ok || !stringOK {
			r.emitArtifactError(done.key.ItemID, name, fmt.Errorf("source %q did not produce a string path", output.From))
			continue
		}
		if err := captureWorkflowArtifact(r.dataRoot, done.key.ItemID, name, done.workspace, path); err != nil {
			r.emitArtifactError(done.key.ItemID, name, err)
		}
	}
}

func (r *workflowAppRunner) emitArtifactError(itemID, output string, err error) {
	log.Printf("workflow artifact capture %s output %q: %v", itemID, output, err)
	r.app.emit("workflow:error", map[string]any{
		"itemId": itemID,
		"output": output,
		"error":  fmt.Sprintf("workflow artifact %q capture failed; inspect local diagnostics", output),
	})
}

func captureWorkflowArtifact(dataRoot, itemID, outputName, workspace, relative string) (resultErr error) {
	if filepath.Base(itemID) != itemID || itemID == "." || itemID == ".." {
		return fmt.Errorf("invalid work item id")
	}
	if filepath.Base(outputName) != outputName || outputName == "." || outputName == ".." {
		return fmt.Errorf("invalid workflow output name")
	}
	relative, err := workspacepath.NormalizeRelative(relative)
	if err != nil {
		return fmt.Errorf("artifact path: %w", err)
	}
	source := filepath.Join(workspace, relative)
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return fmt.Errorf("resolve phase workspace: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve artifact source: %w", err)
	}
	contained, err := filepath.Rel(resolvedWorkspace, resolved)
	if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
		return fmt.Errorf("artifact source escapes the phase workspace")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("inspect artifact source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact source is not a regular file")
	}
	directory := workflowArtifactDir(dataRoot, itemID)
	destination := filepath.Join(directory, outputName+filepath.Ext(resolved))
	destinationRelative, err := filepath.Rel(dataRoot, destination)
	if err != nil {
		return fmt.Errorf("resolve artifact destination: %w", err)
	}
	if err := copyWorkflowFile(workspace, relative, dataRoot, destinationRelative, appSensitiveFilePerm); err != nil {
		return fmt.Errorf("copy artifact: %w", err)
	}
	artifactRoot, err := openWorkflowArtifactRoot(dataRoot, itemID)
	if err != nil {
		return fmt.Errorf("clean prior artifact versions: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, artifactRoot.Close()) }()
	entries, err := fs.ReadDir(artifactRoot.FS(), ".")
	if err != nil {
		return fmt.Errorf("clean prior artifact versions: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == filepath.Base(destination) || strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())) != outputName {
			continue
		}
		if err := artifactRoot.Remove(entry.Name()); err != nil {
			return fmt.Errorf("remove prior artifact %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func openWorkflowArtifactRoot(dataRoot, itemID string) (*os.Root, error) {
	managedRoot, err := os.OpenRoot(dataRoot)
	if err != nil {
		return nil, err
	}
	artifactRoot, openErr := managedRoot.OpenRoot(filepath.Join("workflow-runs", itemID, "artifacts"))
	closeErr := managedRoot.Close()
	if openErr != nil || closeErr != nil {
		if artifactRoot != nil {
			_ = artifactRoot.Close()
		}
		return nil, errors.Join(openErr, closeErr)
	}
	return artifactRoot, nil
}

func workflowArtifactDir(dataRoot, itemID string) string {
	return filepath.Join(dataRoot, "workflow-runs", itemID, "artifacts")
}

func (a *App) workflowDataRoot() string {
	if a.workflowRunner != nil && strings.TrimSpace(a.workflowRunner.dataRoot) != "" {
		return a.workflowRunner.dataRoot
	}
	return a.configDir
}

func listWorkflowArtifacts(dataRoot, itemID string) (result []WorkflowArtifact, resultErr error) {
	if filepath.Base(itemID) != itemID || itemID == "." || itemID == ".." {
		return nil, fmt.Errorf("invalid work item id")
	}
	directory := workflowArtifactDir(dataRoot, itemID)
	if err := validateWorkflowDestination(dataRoot, filepath.Join(directory, ".artifact")); err != nil {
		return nil, fmt.Errorf("list workflow artifacts: %w", err)
	}
	artifactRoot, err := openWorkflowArtifactRoot(dataRoot, itemID)
	if errors.Is(err, fs.ErrNotExist) {
		return []WorkflowArtifact{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list workflow artifacts: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, artifactRoot.Close()) }()
	entries, err := fs.ReadDir(artifactRoot.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("list workflow artifacts: %w", err)
	}
	byName := make(map[string]WorkflowArtifact, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ao-copy-") {
			continue
		}
		info, err := fs.Stat(artifactRoot.FS(), entry.Name())
		if err != nil {
			return nil, fmt.Errorf("inspect workflow artifact %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		artifact := WorkflowArtifact{
			Name: strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())),
			Path: filepath.Join(directory, entry.Name()), Size: info.Size(), ModTime: info.ModTime().UnixMilli(),
		}
		if current, exists := byName[artifact.Name]; !exists || artifact.ModTime > current.ModTime || artifact.ModTime == current.ModTime && artifact.Path > current.Path {
			byName[artifact.Name] = artifact
		}
	}
	artifacts := make([]WorkflowArtifact, 0, len(byName))
	for _, artifact := range byName {
		artifacts = append(artifacts, artifact)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	return artifacts, nil
}
