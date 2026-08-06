package main

import (
	"fmt"
	"log"
	"os"
	"time"
)

// auditAccountEvent records one account-credential lifecycle event — a
// pruned slot, a removed account, a rollback — both to the process log and
// to a durable append-only file in the data dir. The file exists because
// stderr is invisible for a Finder-launched app: a destroyed slot is an
// unrecoverable login (Claude refresh tokens are single-use), and the one
// past incident class ("sign in again" with no evidence trail) was
// undiagnosable precisely because the announcement went nowhere durable.
//
// Best-effort by design: auditing must never fail the operation it
// describes, so a write error is itself only logged.
func (a *App) auditAccountEvent(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	log.Printf("provider accounts: %s", message)
	if a.accountAuditPath == "" {
		return
	}
	line := fmt.Sprintf("%s %s\n", time.Now().Format(time.RFC3339), message)
	file, err := os.OpenFile(a.accountAuditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("provider accounts: append audit log: %v", err)
		return
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(line); err != nil {
		log.Printf("provider accounts: append audit log: %v", err)
	}
}
