//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/repository"
)

// What a truck is currently carrying, which the planner is shown rather than
// stopped by: booking tomorrow's work while a truck is on today's is the
// point of planning ahead.
//
// This is NOT an assignment guard. AssignDriver deliberately does not call
// it — an earlier version did, and that blocked the planning the business
// wants. It answers "what is this truck on now?" for anything that needs to
// say so, including whoever builds K-Trip's order queue.
func TestActiveOrderForTruckReportsWhatATruckIsCarrying(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := repository.NewOrderRepository(db)

	companyID := uuid.New()
	actorID := uuid.New()
	truckID := "truck-" + uuid.NewString()[:8]

	mkOrder := func(status string, truck string) uuid.UUID {
		id := uuid.New()
		tag := uuid.NewString()[:8]
		if err := db.Exec(`INSERT INTO orders
			(id, order_number, shipper_company_id, transporter_company_id, created_by_user_id,
			 truck_id, status_code, detail, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, '{}'::jsonb, NOW(), NOW())`,
			id, "ORD-"+tag, companyID, companyID, actorID, truck, status).Error; err != nil {
			t.Fatalf("order: %v", err)
		}
		return id
	}

	// The order being planned, and one the truck is already running.
	planning := mkOrder(models.OrderReadyToPlan, "")
	running := mkOrder(models.OrderInTransit, truckID)

	busy, err := repo.ActiveOrderForTruck(ctx, truckID, planning)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if busy == nil {
		t.Fatal("a truck mid-journey reported as carrying nothing")
	}
	if busy.ID != running {
		t.Errorf("found order %s, expected the running one %s", busy.ID, running)
	}

	// An order merely ASSIGNED counts too: the driver app is holding it even
	// though the truck has not moved.
	assignedTruck := "truck-" + uuid.NewString()[:8]
	mkOrder(models.OrderAssigned, assignedTruck)
	if busy, err := repo.ActiveOrderForTruck(ctx, assignedTruck, planning); err != nil || busy == nil {
		t.Errorf("a truck with an assigned-but-not-started order reported as free (err=%v)", err)
	}

	// Finished work does not hold a truck.
	for _, done := range []string{models.OrderCompleted, models.OrderCancelled, models.OrderRejected} {
		freeTruck := "truck-" + uuid.NewString()[:8]
		mkOrder(done, freeTruck)
		busy, err := repo.ActiveOrderForTruck(ctx, freeTruck, planning)
		if err != nil {
			t.Fatalf("lookup %s: %v", done, err)
		}
		if busy != nil {
			t.Errorf("a truck whose only order is %s reported as busy", done)
		}
	}

	// Re-assigning the SAME order must not trip over itself.
	selfTruck := "truck-" + uuid.NewString()[:8]
	self := mkOrder(models.OrderAssigned, selfTruck)
	if busy, err := repo.ActiveOrderForTruck(ctx, selfTruck, self); err != nil || busy != nil {
		t.Errorf("an order blocked its own re-assignment (busy=%v err=%v)", busy, err)
	}

	// No truck means nothing to check.
	if busy, err := repo.ActiveOrderForTruck(ctx, "", planning); err != nil || busy != nil {
		t.Errorf("an empty truck id should find nothing (busy=%v err=%v)", busy, err)
	}
}
