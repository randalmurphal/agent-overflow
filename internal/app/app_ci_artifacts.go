package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// Downloading CI artifacts: a GitLab job's archive, or every artifact of
// a GitHub pipeline's workflow runs. Each is streamed from the forge into
// a temp file under the app data directory's ci-artifacts and renamed
// into place once whole, through the writer Save uses for a CI log
// (writeCISavedFile): memory stays bounded by the copy buffer, an archive
// never reaches the webview, and the answer is where the files are.

// ciArtifactMaxBytes caps one artifact. A forge attachment is capped at
// forgeattach.MaxBytes (100 MiB, the forges' own upload limits), but CI
// artifacts are routinely larger, and a file on disk costs no memory, so
// an artifact may be as large as a remote artifact transfer
// (remoteArtifactMaxBytes, 1 GiB).
const ciArtifactMaxBytes = remoteArtifactMaxBytes

// maxCIArtifactsPerSave bounds the artifacts one download saves.
const maxCIArtifactsPerSave = 100

// CIArtifactsSaved answers SaveCIArtifacts: Dir is the directory the
// files went to and Paths every file saved, in the forge's order.
type CIArtifactsSaved struct {
	Dir   string   `json:"dir"`
	Paths []string `json:"paths"`
}

// SaveCIArtifacts downloads the artifacts req names (a GitLab job's
// archive by JobID, a GitHub pipeline's by the RunIDs its jobs carry)
// under the app-managed ci-artifacts directory, in the repository's own
// directory (ciRepoDir), and returns where they are. subject is the job's name, or the pipeline's for a run download,
// for the file names and sentences. A path is stable per (repository,
// job or artifact), so a re-save refreshes the same file. A failure after
// some files were saved names them in its sentence.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SaveCIArtifacts(ctx context.Context, repo gitops.RepoReference, req gitops.CIArtifactsRequest, subject string) (CIArtifactsSaved, error) {
	if a.shuttingDown.Load() {
		return CIArtifactsSaved{}, ErrShuttingDown
	}
	if err := repo.Validate(); err != nil {
		return CIArtifactsSaved{}, err
	}
	if err := req.Validate(); err != nil {
		return CIArtifactsSaved{}, err
	}
	explained := "the workflow runs of " + repo.Project()
	if len(req.RunIDs) == 1 {
		explained = gitops.CIPipelineSubject(repo, req.RunIDs[0], nil)
	}
	if req.JobID != "" {
		explained = gitops.CIJobSubject(repo, firstNonEmptyString(subject, req.JobID))
	}
	ctx = forgeapi.WithInteractive(ctx)
	core := a.gitCore()
	artifacts, err := core.ListCIArtifacts(ctx, repo, req)
	if err != nil {
		a.refreshStaleArtifactsFlag(repo, req, err)
		return CIArtifactsSaved{}, gitops.ExplainCIError(repo, explained, gitops.ActionArtifacts, err)
	}
	if len(artifacts) > maxCIArtifactsPerSave {
		return CIArtifactsSaved{}, &gitops.ForgeActionError{Kind: gitops.ForgeErrorRefused, Message: fmt.Sprintf("%s has %d artifacts, more than the %d one download saves. Download them from the forge.", explained, len(artifacts), maxCIArtifactsPerSave)}
	}
	subdir := filepath.Join("ci-artifacts", ciRepoDir(repo))
	saved := CIArtifactsSaved{Dir: filepath.Join(a.configDir, subdir), Paths: []string{}}
	for _, artifact := range artifacts {
		name := ciArtifactFileName(artifact, subject)
		path, err := a.writeCISavedFile(subdir, "CI artifact", func(w io.Writer) (string, error) {
			return name, core.StreamCIArtifact(ctx, repo, artifact, w, ciArtifactMaxBytes)
		})
		if err != nil {
			a.refreshStaleArtifactsFlag(repo, req, err)
			return saved, a.explainArtifactFailure(repo, explained, artifact, saved, err)
		}
		saved.Paths = append(saved.Paths, path)
	}
	return saved, nil
}

// refreshStaleArtifactsFlag has every pump showing the download's job or
// runs poll when the download found nothing to save: the flag that
// offered it is stale (git.Core dropped its remembered answer), and the
// poll clears it with no timer.
func (a *App) refreshStaleArtifactsFlag(repo gitops.RepoReference, req gitops.CIArtifactsRequest, err error) {
	if !errors.Is(err, gitops.ErrNoCIArtifacts) && !errors.Is(err, gitops.ErrCIArtifactExpired) {
		return
	}
	runs := make(map[string]bool, len(req.RunIDs))
	for _, id := range req.RunIDs {
		runs[id] = true
	}
	a.pollCIPumpsShowing(repo, nil, false, func(phase *ciPhase) bool {
		if req.JobID != "" {
			return gitops.FindCIJob(phase.ci, req.JobID) != nil
		}
		for id := range ciPipelineIDs(phase.ci) {
			if runs[id] {
				return true
			}
		}
		return false
	})
}

// explainArtifactFailure words a failed download: an artifact past the
// size cap by name, anything else as the forge's failure, after the files
// already saved.
func (a *App) explainArtifactFailure(repo gitops.RepoReference, subject string, artifact gitops.CIArtifact, saved CIArtifactsSaved, err error) error {
	var explained *gitops.ForgeActionError
	if errors.Is(err, forgeapi.ErrBodyTooLarge) {
		explained = &gitops.ForgeActionError{Kind: gitops.ForgeErrorRefused, Err: err, Message: fmt.Sprintf(
			"The artifact %s of %s is larger than the %d MiB the app downloads. Download it from the forge.", artifact.Name, subject, ciArtifactMaxBytes>>20)}
	} else {
		explained, _ = gitops.ExplainCIError(repo, subject, gitops.ActionArtifacts, err).(*gitops.ForgeActionError)
	}
	if len(saved.Paths) > 0 {
		explained = &gitops.ForgeActionError{Kind: explained.Kind, Err: err, Message: fmt.Sprintf(
			"Saved %d artifacts to %s before the download stopped. %s", len(saved.Paths), saved.Dir, explained.Message)}
	}
	return explained
}

// ciArtifactFileName builds a filesystem-safe name stable per job or
// artifact within the repository's directory (ciRepoDir): <job id>-<job
// name>.zip for a GitLab archive, <run id>-<artifact id>-<artifact
// name>.zip for a GitHub artifact.
func ciArtifactFileName(artifact gitops.CIArtifact, subject string) string {
	var parts []string
	label := subject
	if artifact.JobID != "" {
		parts = append(parts, artifact.JobID)
	} else {
		parts = append(parts, artifact.RunID, artifact.ID)
		label = strings.TrimSuffix(artifact.Name, ".zip")
	}
	if segment := sanitizeCIFileSegment(label); segment != "" {
		parts = append(parts, segment)
	}
	return strings.Join(parts, "-") + ".zip"
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
