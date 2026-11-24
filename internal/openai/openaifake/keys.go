package openaifake

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// A key from Key carries the fake's record of what the sessions opened with
// it did: the speech requests asked, the sessions open, the configurations
// sent, the count per SpeechAnswersOnce text, and whether its live sessions
// are held. Key mints a new key on every call and drops its record when the
// test ends, so no other test reads the record, nor a later run of the same
// test under -count. A session opened with any other key records nothing,
// and the readers refuse such a key.

var (
	minted  atomic.Uint64
	records sync.Map // key → *keyRecord
)

type keyRecord struct {
	mu          sync.Mutex
	asks        []SpeechAsk
	open        int
	configured  int
	answersOnce map[string]int
	held        chan struct{}
	releaseOnce sync.Once
}

// Key returns a key that no other call returns, starting with prefix, whose
// record goes when t ends. A prefix that is a key scripting a misbehaving
// OpenAI (RefusedKey, StallsKey, SpeechConfigStallsKey) scripts the key's
// sessions the same way.
func Key(t testing.TB, prefix string) string {
	key := fmt.Sprintf("%s-%d", prefix, minted.Add(1))
	record := &keyRecord{answersOnce: map[string]int{}}
	records.Store(key, record)
	t.Cleanup(func() {
		records.Delete(key)
		record.release()
	})
	return key
}

// recordOf returns key's record, or nil for a key Key did not return.
func recordOf(key string) *keyRecord {
	found, ok := records.Load(key)
	if !ok {
		return nil
	}
	return found.(*keyRecord)
}

// mustRecord is recordOf for the readers.
func mustRecord(key string) *keyRecord {
	record := recordOf(key)
	if record == nil {
		panic(fmt.Sprintf("openaifake: %q is not a key from openaifake.Key, or its test has ended: nothing records what its sessions did", key))
	}
	return record
}

// Hold makes the live transcription sessions opened with key withhold every
// transcript until release runs, or key's test ends.
func Hold(key string) (release func()) {
	record := mustRecord(key)
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.held != nil {
		panic(fmt.Sprintf("openaifake: %q is held already", key))
	}
	record.held = make(chan struct{})
	return record.release
}

func (r *keyRecord) release() {
	r.mu.Lock()
	held := r.held
	r.mu.Unlock()
	if held != nil {
		r.releaseOnce.Do(func() { close(held) })
	}
}

// heldBy returns the channel whose closing releases key's held sessions,
// or nil when key is not held.
func heldBy(key string) chan struct{} {
	record := recordOf(key)
	if record == nil {
		return nil
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	return record.held
}

// SpeechAsks lists the voice and text every speech session opened with key
// asked for, in order.
func SpeechAsks(key string) []SpeechAsk {
	record := mustRecord(key)
	record.mu.Lock()
	defer record.mu.Unlock()
	return append([]SpeechAsk(nil), record.asks...)
}

// OpenSpeechSessions counts speech sessions opened with key whose
// connection the fake has not yet seen close.
func OpenSpeechSessions(key string) int {
	record := mustRecord(key)
	record.mu.Lock()
	defer record.mu.Unlock()
	return record.open
}

// SpeechSessionsConfigured counts the session.update events sessions
// opened with key sent, accepted or not.
func SpeechSessionsConfigured(key string) int {
	record := mustRecord(key)
	record.mu.Lock()
	defer record.mu.Unlock()
	return record.configured
}
