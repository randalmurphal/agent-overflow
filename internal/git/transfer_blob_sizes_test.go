package git

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestTransferBlobPreflightCountsRepeatedExpansionAndRejectsNonBlobs(t *testing.T) {
	core := NewCore()
	repo := testutil.InitGitRepo(t)
	ctx := context.Background()
	oid, _, err := core.executeSpec(commandSpec{binary: "git", cwd: repo, stdin: "small", args: []string{"hash-object", "-w", "--stdin"}})
	if err != nil {
		t.Fatal(err)
	}
	oid = strings.TrimSpace(oid)
	entries := []TransferIndexEntry{{Mode: "100644", OID: oid, Path: "first"}, {Mode: "100644", OID: oid, Path: "second"}}
	if sizes, err := core.transferBlobSizes(ctx, repo, entries, 10); err != nil || len(sizes) != 2 || sizes[0] != 5 || sizes[1] != 5 {
		t.Fatalf("sizes=%v err=%v", sizes, err)
	}
	if _, err := core.transferBlobSizes(ctx, repo, entries, 9); err == nil {
		t.Fatal("repeated blob evaded expansion cap")
	}
	head, _, err := core.Execute(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	entries[0].OID = strings.TrimSpace(head)
	if _, err := core.transferBlobSizes(ctx, repo, entries, 1000); err == nil {
		t.Fatal("commit accepted as file content")
	}
	entries[0].OID = strings.Repeat("a", 40)
	if _, err := core.transferBlobSizes(ctx, repo, entries, 1000); err == nil {
		t.Fatal("missing blob accepted")
	}
}
