package transport

import (
	"sync"
	"testing"
	"time"
)

func TestTicketIsSpentExactlyOnce(t *testing.T) {
	book := newTicketBook(8, time.Minute)
	ticket, err := book.mint("session-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	subject, ok := book.consume(ticket)
	if !ok || subject != "session-a" {
		t.Fatalf("consume = (%q, %t), want the minted subject", subject, ok)
	}
	if _, ok := book.consume(ticket); ok {
		t.Fatal("a spent ticket was accepted a second time")
	}
	if book.outstanding() != 0 {
		t.Fatalf("%d tickets outstanding after the only one was spent", book.outstanding())
	}
}

// TestOnlyOneRacerSpendsATicket — single use has to hold under concurrent
// presentation, which is the case a replay actually looks like.
func TestOnlyOneRacerSpendsATicket(t *testing.T) {
	book := newTicketBook(8, time.Minute)
	ticket, err := book.mint("session-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	const racers = 16
	var wg sync.WaitGroup
	won := make(chan string, racers)
	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			if subject, ok := book.consume(ticket); ok {
				won <- subject
			}
		}()
	}
	wg.Wait()
	close(won)
	if got := len(won); got != 1 {
		t.Fatalf("%d racers spent the same ticket", got)
	}
}

func TestTicketLapsesWithItsDeadline(t *testing.T) {
	book := newTicketBook(8, 30*time.Second)
	at := time.UnixMilli(1_700_000_000_000)
	book.now = func() time.Time { return at }

	ticket, err := book.mint("session-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	at = at.Add(31 * time.Second)
	if _, ok := book.consume(ticket); ok {
		t.Fatal("a lapsed ticket was still spendable")
	}
	if book.outstanding() != 0 {
		t.Fatal("a lapsed ticket kept occupying a slot")
	}
}

// TestTicketBookKeepsTheNewest — eviction order is the decision. The
// newest ticket is the one a caller just minted and is about to present;
// dropping it to keep an older one would break the live case to preserve
// an abandoned one.
func TestTicketBookKeepsTheNewest(t *testing.T) {
	const max = 4
	book := newTicketBook(max, 0)
	var minted []string
	for range max * 3 {
		ticket, err := book.mint("")
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		minted = append(minted, ticket)
	}
	if held := book.outstanding(); held > max {
		t.Fatalf("outstanding = %d, want at most %d", held, max)
	}
	if _, ok := book.consume(minted[len(minted)-1]); !ok {
		t.Fatal("the newest ticket was evicted")
	}
	if _, ok := book.consume(minted[0]); ok {
		t.Fatal("the oldest ticket survived past the cap")
	}
}

// TestTicketBookWithoutADeadlineKeepsItsTickets pins the page-ticket
// half: a launcher's fixed `?t=` URL must still work an hour after it was
// written, so a book with ttl == 0 has no deadline at all.
func TestTicketBookWithoutADeadlineKeepsItsTickets(t *testing.T) {
	book := newTicketBook(4, 0)
	at := time.UnixMilli(1_700_000_000_000)
	book.now = func() time.Time { return at }
	ticket, err := book.mint("")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	at = at.Add(72 * time.Hour)
	if _, ok := book.consume(ticket); !ok {
		t.Fatal("a page ticket lapsed; the fixed launcher URL would stop working")
	}
}

func TestTicketBookRefusesTheEmptyToken(t *testing.T) {
	book := newTicketBook(4, time.Minute)
	if _, err := book.mint("session-a"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := book.consume(""); ok {
		t.Fatal("an empty presentation matched an outstanding ticket")
	}
}
