package projectapp

import (
	"agent-overflow/internal/store"
	"context"
	"testing"
)

func TestCreateCheckoutValidatesBeforeWritingAndReusesExisting(t *testing.T) {
	t.Parallel()
	path := t.TempDir()
	fake := &fakeIdentity{answers: map[string]identityAnswer{path: repo("github:github.com:2", "stamp")}}
	answer := fake.answers[path]
	answer.identity.RepositoryID = "github:github.com:2"
	fake.answers[path] = answer
	s, db := newIdentityService(t, fake)
	expected := store.ProjectIdentity{RepositoryID: "github:github.com:1"}
	if _, err := s.CreateCheckout(context.Background(), path, expected); err == nil {
		t.Fatal("conflicting ID accepted")
	}
	rows, err := db.ListAllProjects()
	if err != nil || len(rows) != 0 {
		t.Fatal("refusal wrote a project")
	}
	if _, err := s.CreateCheckout(context.Background(), path, store.ProjectIdentity{Error: "git read failed"}); err == nil {
		t.Fatal("unknown source identity accepted")
	}
	expected.RepositoryID = answer.identity.RepositoryID
	first, err := s.CreateCheckout(context.Background(), path, expected)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateCheckout(context.Background(), path, expected)
	if err != nil || second.Project.ID != first.Project.ID || second.Changed {
		t.Fatalf("retry: %+v %v", second, err)
	}
	if _, err := s.Archive(first.Project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCheckout(context.Background(), path, expected); err == nil {
		t.Fatal("archived checkout silently adopted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.CreateCheckout(ctx, path, expected); err == nil {
		t.Fatal("cancelled checkout accepted")
	}
}

func TestRepositoryIDSurvivesFailureOnlyForTheSameRemote(t *testing.T) {
	t.Parallel()
	path := t.TempDir()
	fake := &fakeIdentity{answers: map[string]identityAnswer{path: repo("github:github.com:1", "stamp")}}
	answer := fake.answers[path]
	answer.identity.RepositoryID = "github:github.com:1"
	fake.answers[path] = answer
	s, _ := newIdentityService(t, fake)
	created, err := s.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	answer.identity.RepositoryID = ""
	answer.identity.LookupError = "offline"
	fake.answers[path] = answer
	refresh, err := s.RefreshProject(context.Background(), created.ID)
	if err != nil || refresh.Project.RepositoryID != created.RepositoryID || refresh.Project.IdentityError != "offline" {
		t.Fatalf("offline: %+v %v", refresh, err)
	}
	answer.identity.IdentitySource = "different-checkout-source"
	fake.answers[path] = answer
	refresh, err = s.RefreshProject(context.Background(), created.ID)
	if err != nil || refresh.Project.RepositoryID != "" {
		t.Fatalf("retarget retained old identity: %+v %v", refresh, err)
	}
}

func TestCreateCheckoutChecksKnownIdentityWhenForgeIsOffline(t *testing.T) {
	t.Parallel()
	path := t.TempDir()
	fake := &fakeIdentity{answers: map[string]identityAnswer{path: repo("github:github.com:1", "stamp")}}
	answer := fake.answers[path]
	answer.identity.RepositoryID = "github:github.com:2"
	fake.answers[path] = answer
	s, _ := newIdentityService(t, fake)
	created, err := s.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	answer.identity.RepositoryID = ""
	answer.identity.LookupError = "offline"
	fake.answers[path] = answer
	expected := store.ProjectIdentity{RepositoryID: "github:github.com:1"}
	if _, err := s.CreateCheckout(context.Background(), path, expected); err == nil {
		t.Fatal("cached conflicting identity bypassed by an unavailable forge")
	}
	inspected, err := s.InspectFolder(context.Background(), path)
	if err != nil || inspected.RepositoryID != created.RepositoryID {
		t.Fatalf("offline preflight: %+v %v", inspected, err)
	}
	expected.RepositoryID = created.RepositoryID
	accepted, err := s.CreateCheckout(context.Background(), path, expected)
	if err != nil || accepted.Project.ID != created.ID || accepted.Project.RepositoryID != created.RepositoryID {
		t.Fatalf("known checkout: %+v %v", accepted, err)
	}
}

func TestInspectFolderCannotReuseIDFromChangedOrigin(t *testing.T) {
	t.Parallel()
	path := t.TempDir()
	fake := &fakeIdentity{answers: map[string]identityAnswer{path: repo("github:github.com:1", "original")}}
	s, _ := newIdentityService(t, fake)
	if _, err := s.Create(path); err != nil {
		t.Fatal(err)
	}
	answer := fake.answers[path]
	answer.identity.RepositoryID = ""
	answer.identity.LookupError = "offline"
	answer.identity.IdentitySource = "changed"
	fake.answers[path] = answer
	folder, err := s.InspectFolder(context.Background(), path)
	if err != nil || folder.RepositoryID != "" || folder.IdentityError == "" {
		t.Fatalf("changed origin: %+v %v", folder, err)
	}
	if _, err := s.CreateCheckout(context.Background(), path, store.ProjectIdentity{RepositoryID: "github:github.com:1"}); err == nil {
		t.Fatal("changed origin adopted with stale ID")
	}
}
