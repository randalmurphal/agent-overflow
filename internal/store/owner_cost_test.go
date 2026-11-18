package store

import (
	"slices"
	"testing"
)

func TestOwnerChangeAndRecoveryReadTheSameWorkWithTenOrTenThousandSettledLinks(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"caller", "working", "resting"} {
		mustCreateThread(t, s, id)
	}
	mustExec(t, s.db, `INSERT INTO thread_request_receipts(token,owner_device_id,kind,target_thread_id,state,created_at,updated_at)
	VALUES ('active','owner','spawn','working','running',1,1)`)
	steps := func(from, to int) []int {
		t.Helper()
		const numbers = `WITH RECURSIVE n(x) AS (SELECT ?1 UNION ALL SELECT x+1 FROM n WHERE x<?2) `
		mustExec(t, s.db, numbers+`INSERT INTO thread_requests(token,caller_thread_id,kind,target_computer_id,target_thread_id,state,polling,settled_at,created_at,updated_at,contact_due,expires_at)
		SELECT 'old-'||x,'caller','spawn','desk','worker-'||x,'finished',1,2,1,2,100,100 FROM n`, from, to)
		mustExec(t, s.db, numbers+`INSERT INTO thread_request_receipts(token,owner_device_id,kind,target_thread_id,state,created_at,updated_at,settled_at,revision,collected_revision)
		SELECT 'old-'||x,'owner','spawn','resting','finished',1,2,2,1,1 FROM n`, from, to)
		return []int{
			statementVMSteps(t, s, ownerComputersSQL),
			statementVMSteps(t, s, earliestRemotePollSQL, `["desk"]`),
			statementVMSteps(t, s, dueRemotePollSQL(32), `["desk"]`, 3),
			statementVMSteps(t, s, activityOwnersSQL+" ORDER BY 1,2", "[]", `["working"]`),
			statementVMSteps(t, s, recoverOwnerRequestsSQL+"32", "desk", ""),
			statementVMSteps(t, s, uncollectedOwnerTokensSQL+"32", "owner", ""),
			statementVMSteps(t, s, threadsWithOpenAgentRowsSQL(33), "", `{"waiting":[],"live":[]}`),
		}
	}
	ten := steps(1, 10)
	many := steps(11, 10000)
	if !slices.Equal(ten, many) {
		t.Fatalf("owner discovery, activity resolution and recovery VM steps: 10 links %v, 10,000 links %v", ten, many)
	}
	t.Logf("VM steps for both histories: %v", many)
}
