package secret

import (
	"net/url"
	"strings"
)

// RedactError answers err's message with token, raw or query-escaped as a
// URL carries it, replaced by Redacted, so the message can be logged. A nil
// error answers "unknown error"; an empty token redacts nothing.
func RedactError(err error, token string) string {
	if err == nil {
		return "unknown error"
	}
	message := err.Error()
	if token == "" {
		return message
	}
	message = strings.ReplaceAll(message, token, Redacted)
	return strings.ReplaceAll(message, url.QueryEscape(token), Redacted)
}
