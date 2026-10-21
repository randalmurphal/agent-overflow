package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/transport"
)

// The Pipelines tab's own refresh is background work: under the rate
// limit reserve it is refused before it reaches the forge, while the
// user's open, refresh or filter change (interactive) still reads; above
// the reserve both read.
func TestPipelineReadsSpendTheReserveOnlyWhenInteractive(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	engine := forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	if _, err := engine.Seed(forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "acme/pipes", Pipelines: []forgefake.Pipeline{{ID: 200, Name: "CI", Ref: "main", Jobs: pipelineJobs(2001, "success")}}},
		{Forge: "gitlab", Project: "grp/pipes", Pipelines: []forgefake.Pipeline{{ID: 20, Ref: "main", Jobs: pipelineJobs(2101, "success")}}},
	}}); err != nil {
		t.Fatal(err)
	}
	useForgefake(t, app, engine)
	reset := time.Now().Add(time.Hour).Unix()
	for _, c := range []struct {
		origin, forge, pool, id string
		repo                    gitops.RepoReference
	}{
		{"https://github.com/acme/pipes.git", "github", "core", "200", gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "pipes"}},
		{"https://gitlab.com/grp/pipes.git", "gitlab", "throttle_authenticated_api", "20", gitops.RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "pipes"}},
	} {
		project := forgeProjectRepo(t, app, c.origin)
		read := func(interactive bool) (listErr error, stagesKind string) {
			if _, err := app.ListProjectPipelines(context.Background(), project, gitops.PipelineListFilter{}, interactive); err != nil {
				listErr = err
			}
			stages, err := app.ReadPipelineStages(context.Background(), c.repo, []string{c.id}, interactive)
			if err != nil {
				t.Fatalf("%s: ReadPipelineStages: %v", c.forge, err)
			}
			return listErr, stages[c.id].ErrorKind
		}
		// Just above the 10% reserve, the background refresh reads.
		if err := engine.SetRateLimit(forgefake.RateLimit{Forge: c.forge, Pool: c.pool, Remaining: 501, Reset: reset}); err != nil {
			t.Fatal(err)
		}
		if listErr, stagesKind := read(true); listErr != nil || stagesKind != "" {
			t.Fatalf("%s above the reserve, interactive: %v / %q", c.forge, listErr, stagesKind)
		}
		if listErr, stagesKind := read(false); listErr != nil || stagesKind != "" {
			t.Fatalf("%s above the reserve, background: %v / %q", c.forge, listErr, stagesKind)
		}

		// Just under it, the user's read primes the quota and goes through;
		// the background refresh is refused without a request.
		if err := engine.SetRateLimit(forgefake.RateLimit{Forge: c.forge, Pool: c.pool, Remaining: 499, Reset: reset}); err != nil {
			t.Fatal(err)
		}
		if listErr, stagesKind := read(true); listErr != nil || stagesKind != "" {
			t.Fatalf("%s under the reserve, interactive: %v / %q", c.forge, listErr, stagesKind)
		}
		before := len(engine.Invocations(0).Invocations)
		listErr, stagesKind := read(false)
		var forgeErr *gitops.ForgeActionError
		if !errors.As(listErr, &forgeErr) || forgeErr.Kind != gitops.ForgeErrorRateLimited || stagesKind != gitops.ForgeErrorRateLimited {
			t.Fatalf("%s under the reserve, background: %v / %q, want both refused as rate limited", c.forge, listErr, stagesKind)
		}
		// The error frame says the reserve was kept and when it lifts.
		var release interface{ RateLimitRelease() (bool, time.Time) }
		if code, _, _ := errorsx.PublicDetails(listErr); code != transport.ErrCodeForgeRateLimited || !errors.As(listErr, &release) {
			t.Fatalf("%s: refusal code %q carries no release: %v", c.forge, code, listErr)
		}
		if reserve, until := release.RateLimitRelease(); !reserve || until.Unix() != reset {
			t.Fatalf("%s: release = %v %v, want the reserve until the reset", c.forge, reserve, until)
		}
		if after := len(engine.Invocations(0).Invocations); after != before {
			t.Fatalf("%s: the refused background reads made %d requests", c.forge, after-before)
		}
		if listErr, stagesKind := read(true); listErr != nil || stagesKind != "" {
			t.Fatalf("%s under the reserve, interactive again: %v / %q", c.forge, listErr, stagesKind)
		}
	}
	expectForgeCallsHandled(t, engine)
}

// pipelineJobs is one job of status, for a fixture pipeline.
func pipelineJobs(id int, status string) []forgefake.Job {
	return []forgefake.Job{{ID: int64(id), Name: "build", Stage: "build", Status: status}}
}

// expectForgeCallsHandled fails the test on any request the fake did not
// implement, as the e2e specs' expectEveryForgeCallHandled does.
func expectForgeCallsHandled(t *testing.T, engine *forgefake.Engine) {
	t.Helper()
	for _, inv := range engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Errorf("unhandled forge call %s %s: %s", inv.Method, inv.Path, inv.Detail)
		}
	}
}

// GitLab's tag scope names only each tag's latest pipeline: the list
// marks every run of a tag, an older one and one of a ref filter
// included, through the app's transport and the fake's documented scope.
func TestPipelinesListMarksEveryGitLabTagRunThroughTheFake(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	engine := forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	if _, err := engine.Seed(forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/tags", Pipelines: []forgefake.Pipeline{
		{ID: 10, Ref: "main", Jobs: pipelineJobs(1001, "success")},
		{ID: 20, RefKind: "tag", Ref: "v1", Jobs: pipelineJobs(2001, "success")},
		{ID: 30, RefKind: "tag", Ref: "v1", Jobs: pipelineJobs(3001, "failed")},
	}}}}); err != nil {
		t.Fatal(err)
	}
	useForgefake(t, app, engine)
	project := forgeProjectRepo(t, app, "https://gitlab.com/grp/tags.git")

	kinds := func(result PipelineListResult) string {
		var out []string
		for _, row := range result.Rows {
			out = append(out, fmt.Sprintf("%s:%s", row.ID, row.RefKind))
		}
		return strings.Join(out, " ")
	}
	for name, c := range map[string]struct {
		filter gitops.PipelineListFilter
		want   string
	}{
		"every ref":         {gitops.PipelineListFilter{}, "30:tag 20:tag 10:branch"},
		"the older tag run": {gitops.PipelineListFilter{Status: "success"}, "20:tag 10:branch"},
		"one tag ref":       {gitops.PipelineListFilter{Ref: "v1", Status: "success"}, "20:tag"},
		"one branch ref":    {gitops.PipelineListFilter{Ref: "main"}, "10:branch"},
	} {
		result, err := app.ListProjectPipelines(context.Background(), project, c.filter, true)
		if err != nil {
			t.Fatalf("%s: ListProjectPipelines: %v", name, err)
		}
		if got := kinds(result); got != c.want {
			t.Errorf("%s: rows = %s, want %s", name, got, c.want)
		}
	}
	expectForgeCallsHandled(t, engine)
}

// A repeated stage read of an unchanged pipeline costs what the forge
// charges: GitHub revalidates the run and its jobs list (304 each);
// GitLab's jobs list answers 200 whatever the ETag, as measured, and the
// single pipeline is read whole.
func TestPipelineStagesRereadCostsWhatEachForgeCharges(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	engine := forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	if _, err := engine.Seed(forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "acme/pipes", Pipelines: []forgefake.Pipeline{{ID: 200, Name: "CI", Ref: "main", Jobs: pipelineJobs(2001, "success")}}},
		{Forge: "gitlab", Project: "grp/pipes", Pipelines: []forgefake.Pipeline{{ID: 20, Ref: "main", Jobs: pipelineJobs(2101, "success")}}},
	}}); err != nil {
		t.Fatal(err)
	}
	useForgefake(t, app, engine)
	for _, c := range []struct {
		repo gitops.RepoReference
		id   string
		want string
	}{
		{gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "pipes"}, "200", "gh api run:304 gh api run jobs:304"},
		{gitops.RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "pipes"}, "20", "glab api pipeline:200 glab api pipeline jobs:200"},
	} {
		for read := range 2 {
			since := len(engine.Invocations(0).Invocations)
			got, err := app.ReadPipelineStages(context.Background(), c.repo, []string{c.id}, true)
			if err != nil {
				t.Fatalf("%s read %d: %v", c.repo.Forge, read, err)
			}
			if stages := got[c.id]; stages.Error != "" || len(stages.Stages) != 1 {
				t.Fatalf("%s read %d: stages = %+v", c.repo.Forge, read, stages)
			}
			if read == 0 {
				continue
			}
			var calls []string
			for _, inv := range engine.Invocations(0).Invocations[since:] {
				calls = append(calls, fmt.Sprintf("%s:%d", inv.Route, inv.Status))
			}
			if strings.Join(calls, " ") != c.want {
				t.Errorf("%s reread = %v, want %s", c.repo.Forge, calls, c.want)
			}
		}
	}
	expectForgeCallsHandled(t, engine)
}
