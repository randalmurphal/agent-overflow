package provideraccountapp

import (
	"fmt"
	"log"
	"os"
	"time"
)

// audit records credential lifecycle events durably without making audit I/O
// part of the credential transaction's success condition.
func (m *Manager) audit(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	log.Printf("provider accounts: %s", message)
	if m == nil || m.auditPath == "" {
		return
	}
	line := fmt.Sprintf("%s %s\n", time.Now().Format(time.RFC3339), message)
	file, err := os.OpenFile(m.auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("provider accounts: append audit log: %v", err)
		return
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(line); err != nil {
		log.Printf("provider accounts: append audit log: %v", err)
	}
}
