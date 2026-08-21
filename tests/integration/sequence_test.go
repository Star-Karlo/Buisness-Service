//go:build integration

package integration

import (
	"sync"
	"testing"

	"github.com/karlo/business-service/internal/repository"
)

// TestNumberSequenceIsCollisionFreeUnderConcurrency is the test that justifies
// replacing the legacy "uniqid" collection.
//
// That implementation read the current value, added one, and wrote it back, so
// two requests arriving together could both read the same number and both
// believe they had reserved it — producing two orders with the same
// human-facing number, which is a support problem rather than a crash.
//
// The replacement is a single atomic statement. This test asserts that: 200
// concurrent callers must receive 200 distinct numbers.
func TestNumberSequenceIsCollisionFreeUnderConcurrency(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	repo := repository.NewOrderRepository(db)

	const callers = 200

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make([]int64, 0, callers)
		errs    = make([]error, 0)
	)

	// A start barrier, so the callers contend rather than trickling in.
	start := make(chan struct{})

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			n, err := repo.NextNumber(ctx(), "order", "2026")

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, n)
		}()
	}

	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d callers failed, first: %v", len(errs), errs[0])
	}
	if len(results) != callers {
		t.Fatalf("got %d results, want %d", len(results), callers)
	}

	seen := make(map[int64]bool, callers)
	for _, n := range results {
		if seen[n] {
			t.Fatalf("number %d was handed out more than once", n)
		}
		seen[n] = true
	}

	// The values must also be exactly 1..N with no gaps: a gap would mean a
	// reservation was lost, which shows up later as a missing invoice number
	// during an audit.
	for i := int64(1); i <= callers; i++ {
		if !seen[i] {
			t.Errorf("number %d was never handed out", i)
		}
	}
}

// TestNumberSequenceScopesAreIndependent confirms that orders, invoices and
// agreements each get their own counter, and that a new year restarts.
func TestNumberSequenceScopesAreIndependent(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	repo := repository.NewOrderRepository(db)

	orderFirst, err := repo.NextNumber(ctx(), "order", "2026")
	if err != nil {
		t.Fatalf("order sequence: %v", err)
	}
	invoiceFirst, err := repo.NextNumber(ctx(), "invoice", "2026")
	if err != nil {
		t.Fatalf("invoice sequence: %v", err)
	}
	nextYear, err := repo.NextNumber(ctx(), "order", "2027")
	if err != nil {
		t.Fatalf("next year: %v", err)
	}

	if orderFirst != 1 || invoiceFirst != 1 || nextYear != 1 {
		t.Errorf("each scope and period should start at 1; got order=%d invoice=%d 2027=%d",
			orderFirst, invoiceFirst, nextYear)
	}

	second, err := repo.NextNumber(ctx(), "order", "2026")
	if err != nil {
		t.Fatalf("second order number: %v", err)
	}
	if second != 2 {
		t.Errorf("the order sequence should continue at 2, got %d", second)
	}
}
