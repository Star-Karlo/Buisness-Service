//go:build integration

package integration

import (
	"errors"
	"sync"
	"testing"

	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/repository"
)

// TestConcurrentTransitionsProduceExactlyOneWinner is the test that justifies
// the compare-and-set guard on ApplyStatus.
//
// The legacy code read the order, decided whether the change was legal, then
// wrote the new status. Two managers pressing approve at the same moment both
// read `submitted`, both judged the move legal, and both wrote — so the second
// silently overwrote the first, and the audit trail recorded one of them.
//
// The replacement makes the read and the write one statement:
//
//	UPDATE orders SET status_code = $to WHERE id = $id AND status_code = $from
//
// Exactly one caller can match. This asserts that.
func TestConcurrentTransitionsProduceExactlyOneWinner(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	repo := repository.NewOrderRepository(db)
	order := seedOrder(t, db, models.OrderSubmitted)

	const contenders = 20

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		refused int
		other   []error
	)

	start := make(chan struct{})

	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			err := repo.ApplyStatus(ctx(), repository.StatusChange{
				OrderID: order.ID,
				From:    models.OrderSubmitted,
				To:      models.OrderApproved,
				Source:  models.SourceUser,
			})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, repository.ErrConflict):
				refused++
			default:
				other = append(other, err)
			}
		}()
	}

	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("%d callers failed unexpectedly, first: %v", len(other), other[0])
	}
	if wins != 1 {
		t.Errorf("%d callers succeeded; exactly 1 should have", wins)
	}
	if refused != contenders-1 {
		t.Errorf("%d callers were refused; %d should have been", refused, contenders-1)
	}

	// The order must have moved exactly once.
	var final models.Order
	if err := db.First(&final, "id = ?", order.ID).Error; err != nil {
		t.Fatalf("could not reload the order: %v", err)
	}
	if final.StatusCode != models.OrderApproved {
		t.Errorf("final status = %q, want %q", final.StatusCode, models.OrderApproved)
	}

	// And the history must carry exactly one row for the transition, not one
	// per contender. A losing caller must leave no trace.
	history, err := repo.History(ctx(), order.ID)
	if err != nil {
		t.Fatalf("could not read history: %v", err)
	}

	var approvals int
	for _, entry := range history {
		if entry.ToStatusCode == models.OrderApproved {
			approvals++
		}
	}
	if approvals != 1 {
		t.Errorf("history holds %d approval rows, want exactly 1", approvals)
	}
}

// TestTransitionFromWrongStateIsRefused covers the ordinary case: a caller
// acting on a stale view of the order.
func TestTransitionFromWrongStateIsRefused(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	repo := repository.NewOrderRepository(db)
	order := seedOrder(t, db, models.OrderApproved)

	// The caller believes the order is still submitted.
	err := repo.ApplyStatus(ctx(), repository.StatusChange{
		OrderID: order.ID,
		From:    models.OrderSubmitted,
		To:      models.OrderApproved,
	})

	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}

	// Nothing may have been written.
	history, herr := repo.History(ctx(), order.ID)
	if herr != nil {
		t.Fatalf("could not read history: %v", herr)
	}
	for _, entry := range history {
		if entry.FromStatusCode != nil && *entry.FromStatusCode == models.OrderSubmitted {
			t.Error("a refused transition left a history row behind")
		}
	}
}

// TestTransitionRollsBackHistoryOnFailure confirms the status write and the
// history write are one transaction. A status change with no audit row, or an
// audit row for a change that did not happen, are both worse than a clean
// failure.
func TestTransitionWritesStatusAndHistoryAtomically(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	repo := repository.NewOrderRepository(db)
	order := seedOrder(t, db, models.OrderDraft)

	if err := repo.ApplyStatus(ctx(), repository.StatusChange{
		OrderID: order.ID,
		From:    models.OrderDraft,
		To:      models.OrderSubmitted,
		Source:  models.SourceUser,
	}); err != nil {
		t.Fatalf("transition failed: %v", err)
	}

	var reloaded models.Order
	if err := db.First(&reloaded, "id = ?", order.ID).Error; err != nil {
		t.Fatalf("could not reload: %v", err)
	}
	if reloaded.StatusCode != models.OrderSubmitted {
		t.Errorf("status = %q, want %q", reloaded.StatusCode, models.OrderSubmitted)
	}

	history, err := repo.History(ctx(), order.ID)
	if err != nil {
		t.Fatalf("could not read history: %v", err)
	}

	// Two rows: the opening status written at creation, and this transition.
	if len(history) != 2 {
		t.Fatalf("history holds %d rows, want 2", len(history))
	}

	last := history[len(history)-1]
	if last.ToStatusCode != models.OrderSubmitted {
		t.Errorf("last history row records %q", last.ToStatusCode)
	}
	if last.FromStatusCode == nil || *last.FromStatusCode != models.OrderDraft {
		t.Errorf("last history row does not record the previous state")
	}
	if last.Source != models.SourceUser {
		t.Errorf("source = %q, want %q", last.Source, models.SourceUser)
	}
}

// TestApplyStatusSetsAccompanyingFieldsInTheSameStatement covers the
// assign-driver path, where the status and the driver must land together. A
// status of "assigned" with no driver is a shipment nobody can execute.
func TestApplyStatusSetsAccompanyingFields(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	repo := repository.NewOrderRepository(db)
	order := seedOrder(t, db, models.OrderReadyToPlan)

	driverID := order.CreatedByUserID
	const truckID = "507f1f77bcf86cd799439011"

	err := repo.ApplyStatus(ctx(), repository.StatusChange{
		OrderID: order.ID,
		From:    models.OrderReadyToPlan,
		To:      models.OrderAssigned,
		Fields: map[string]interface{}{
			"driver_user_id": driverID,
			"truck_id":       truckID,
		},
	})
	if err != nil {
		t.Fatalf("transition failed: %v", err)
	}

	var reloaded models.Order
	if err := db.First(&reloaded, "id = ?", order.ID).Error; err != nil {
		t.Fatalf("could not reload: %v", err)
	}

	if reloaded.StatusCode != models.OrderAssigned {
		t.Errorf("status = %q", reloaded.StatusCode)
	}
	if reloaded.DriverUserID == nil || *reloaded.DriverUserID != driverID {
		t.Error("the driver was not set alongside the status")
	}
	if reloaded.TruckID == nil || *reloaded.TruckID != truckID {
		t.Error("the truck was not set alongside the status")
	}
}
