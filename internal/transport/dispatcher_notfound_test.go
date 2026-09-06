package transport

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

type missingRowApp struct{ wrapped bool }

func (a *missingRowApp) Read() (string, error) {
	if a.wrapped {
		return "", fmt.Errorf("private /home/person/history: %w", sql.ErrNoRows)
	}
	return "", sql.ErrNoRows
}

func TestDispatcherMissingRowsRemainApplicationStateOnEveryOrigin(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		for _, local := range []bool{false, true} {
			t.Run(fmt.Sprintf("wrapped=%v/local=%v", wrapped, local), func(t *testing.T) {
				dispatcher := NewDispatcher()
				if _, err := dispatcher.Register(&missingRowApp{wrapped: wrapped}, RegisterOptions{}); err != nil {
					t.Fatal(err)
				}
				method, failure := resolveLoopback(dispatcher, 0, "Read")
				if failure != nil {
					t.Fatal(failure)
				}
				_, failure = dispatcher.InvokeForOrigin(context.Background(), method, nil, local)
				if failure == nil || failure.Code != ErrCodeNotFound || failure.Message != "The requested item no longer exists." {
					t.Fatalf("missing-row verdict: %+v", failure)
				}
			})
		}
	}
}
