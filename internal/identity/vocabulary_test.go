package identity

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// TestDeclaredValueSetsMatchTheSchemaChecks is the cross-check
// internal/store/AGENTS.md points at.
//
// The value sets exist twice on purpose: as Go types here, and as CHECK
// constraints in migration v75. internal/store stays identity-free the same
// way it stays provider-free, so it cannot import these constants — and
// this package can import it, which is why the test lives on this side.
//
// It fails in BOTH directions. A value declared here that the schema
// refuses would be a session nobody could mint; a value the schema accepts
// that is not declared here would be a row no Go code could name.
func TestDeclaredValueSetsMatchTheSchemaChecks(t *testing.T) {
	storePath := storetest.ClonePath(t)

	cases := []struct {
		table    string
		column   string
		declared []string
	}{
		{"devices", "class", asStrings(DeviceClasses)},
		{"sessions", "binding_class", asStrings(BindingClasses)},
	}
	for _, tc := range cases {
		t.Run(tc.table+"."+tc.column, func(t *testing.T) {
			inSchema := checkedValues(t, storePath, tc.table, tc.column)
			if !slices.Equal(inSchema, tc.declared) {
				t.Fatalf("%s.%s CHECK holds %v, Go declares %v",
					tc.table, tc.column, inSchema, tc.declared)
			}
		})
	}

	// The store's own outcome pair, restated as a third case because it is
	// the same class of duplication even though its Go constants live in
	// internal/store rather than here.
	if got := checkedValues(t, storePath, "auth_audit", "outcome"); !slices.Equal(got,
		[]string{store.AuthAuditOutcomeAllowed, store.AuthAuditOutcomeRefused}) {
		t.Fatalf("auth_audit.outcome CHECK holds %v", got)
	}
	if got := checkedValues(t, storePath, "users", "role"); !slices.Equal(got,
		[]string{store.UserRoleOwner, store.UserRoleMember}) {
		t.Fatalf("users.role CHECK holds %v", got)
	}
}

// TestEveryDeclaredValueIsWritable drives each declared value through a
// real store, so the sets are checked by behavior and not only by parsing
// DDL text. A CHECK the parser misreads would still be caught here.
func TestEveryDeclaredValueIsWritable(t *testing.T) {
	sessions, st, _, owner, _ := newFixture(t)
	for _, class := range DeviceClasses {
		device, err := st.CreateDevice(owner.ID, "Device", string(class), "linux")
		if err != nil {
			t.Fatalf("device class %q is declared but refused: %v", class, err)
		}
		for _, binding := range BindingClasses {
			if _, _, err := sessions.Mint(MintRequest{
				UserID: owner.ID, DeviceID: device.ID,
				BindingClass: binding, Scopes: Scopes, TTL: time.Minute,
			}); err != nil {
				t.Fatalf("binding class %q is declared but refused: %v", binding, err)
			}
		}
	}
	for _, event := range AuditEvents {
		if _, err := st.AppendAuthAudit(store.AuthAuditEntry{
			At: 1, Event: string(event), Outcome: store.AuthAuditOutcomeAllowed,
		}); err != nil {
			t.Fatalf("audit event %q is declared but refused: %v", event, err)
		}
	}
}

func TestScopeValidationRefusesWhatItCannotName(t *testing.T) {
	if _, err := ValidateScopes(Scopes); err != nil {
		t.Fatalf("the declared scope set was refused: %v", err)
	}
	if _, err := ValidateScopes([]Scope{ScopeThreadsRead, "threads:write"}); err == nil {
		t.Fatal("ValidateScopes accepted an undeclared scope")
	}
	// Refused, never silently dropped: a discarded scope produces a session
	// that works for months and then refuses one call.
	got, err := ValidateScopes([]Scope{ScopeThreadsRead, ScopeGitOperate})
	if err != nil {
		t.Fatalf("ValidateScopes: %v", err)
	}
	if !slices.Equal(got, []string{"threads:read", "git:operate"}) {
		t.Fatalf("ValidateScopes = %v", got)
	}
	if got, err := ValidateScopes(nil); err != nil || len(got) != 0 {
		t.Fatalf("ValidateScopes(nil) = %v (err %v), want an empty set", got, err)
	}
}

// TestScopeSetMatchesTheSpecTable pins the ten names. They are the audit
// vocabulary, so a rename is a wire change and a deletion loses history's
// ability to say what was granted.
func TestScopeSetMatchesTheSpecTable(t *testing.T) {
	want := []string{
		"threads:read", "files:read", "threads:operate", "approvals:respond",
		"threads:autonomy", "terminal:operate", "git:operate",
		"attachments:write", "settings:write", "access:admin",
	}
	if !slices.Equal(asStrings(Scopes), want) {
		t.Fatalf("scope set = %v\nwant %v", asStrings(Scopes), want)
	}
	// `host` is a phase-3 method annotation, not something a session holds.
	if Scope("host").Valid() {
		t.Fatal("`host` is declared as a session scope")
	}
}

func TestValidReportsMembership(t *testing.T) {
	for _, class := range DeviceClasses {
		if !class.Valid() {
			t.Fatalf("declared device class %q reports invalid", class)
		}
	}
	for _, binding := range BindingClasses {
		if !binding.Valid() {
			t.Fatalf("declared binding class %q reports invalid", binding)
		}
	}
	if DeviceClass("toaster").Valid() || BindingClass("sort-of").Valid() {
		t.Fatal("an undeclared value reported valid")
	}
}

func asStrings[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = string(value)
	}
	return out
}

// checkedValues extracts the literal list from `CHECK(<column> IN (...))`
// in a table's stored DDL. Reading the schema itself rather than a copy of
// it is what makes the comparison meaningful.
func checkedValues(t *testing.T, storePath, table, column string) []string {
	t.Helper()
	ddl := tableDDL(t, storePath, table)
	needle := "CHECK(" + column + " IN ("
	start := strings.Index(ddl, needle)
	if start < 0 {
		t.Fatalf("no `%s` in the DDL of %s:\n%s", needle, table, ddl)
	}
	rest := ddl[start+len(needle):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("unterminated CHECK list in %s", table)
	}
	var out []string
	for _, part := range strings.Split(rest[:end], ",") {
		out = append(out, strings.Trim(strings.TrimSpace(part), "'"))
	}
	return out
}

// tableDDL reads a table's stored CREATE statement straight out of
// sqlite_master. Opening the file directly rather than going through the
// store is deliberate: the point is to read what the migration actually
// wrote, not what a Go accessor believes it wrote.
func tableDDL(t *testing.T, storePath, table string) string {
	t.Helper()
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatalf("open store for DDL read: %v", err)
	}
	defer func() { _ = db.Close() }()
	var ddl string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&ddl); err != nil {
		t.Fatalf("read DDL of %s: %v", table, err)
	}
	return ddl
}
