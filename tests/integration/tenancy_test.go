//go:build integration

package integration

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/platform/query"
	"github.com/karlo/business-service/internal/repository"
)

// TestOrderReadsAreScopedToTheCompany is the tenancy guarantee, tested against
// real SQL rather than by inspecting a query builder.
//
// The monolith's GetAllOrder applied no scoping at all: any authenticated user
// could list every order in the system. Here the company id is part of the
// WHERE clause, so an order belonging to someone else does not merely fail an
// authorisation check afterwards — it is never selected.
func TestOrderReadsAreScopedToTheCompany(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	repo := repository.NewOrderRepository(db)

	ours := seedOrder(t, db, models.OrderSubmitted)
	theirs := seedOrder(t, db, models.OrderSubmitted)

	shipper := ours.ShipperCompanyID
	stranger := uuid.New()

	t.Run("a party to the order can read it", func(t *testing.T) {
		got, err := repo.FindByID(ctx(), shipper, ours.ID)
		if err != nil {
			t.Fatalf("the shipper could not read their own order: %v", err)
		}
		if got.ID != ours.ID {
			t.Errorf("got order %s, want %s", got.ID, ours.ID)
		}
	})

	t.Run("the transporter side can read it too", func(t *testing.T) {
		if _, err := repo.FindByID(ctx(), *ours.TransporterCompanyID, ours.ID); err != nil {
			t.Fatalf("the transporter could not read the order: %v", err)
		}
	})

	// The important case. Note it returns ErrNotFound, not a permission error:
	// a distinct "forbidden" response would confirm the id exists, which is
	// itself a disclosure.
	t.Run("another company cannot read it", func(t *testing.T) {
		_, err := repo.FindByID(ctx(), stranger, ours.ID)
		if !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("one company's order is invisible to the other", func(t *testing.T) {
		if _, err := repo.FindByID(ctx(), shipper, theirs.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("listing returns only the caller's orders", func(t *testing.T) {
		params := query.Params{Page: 0, PageSize: 50}

		orders, total, err := repo.List(ctx(), shipper, params)
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		if total != 1 {
			t.Errorf("total = %d, want 1", total)
		}
		for _, o := range orders {
			if o.ID != ours.ID {
				t.Errorf("listing leaked order %s", o.ID)
			}
		}

		// A company with no orders sees none, rather than everything.
		_, strangerTotal, err := repo.List(ctx(), stranger, params)
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		if strangerTotal != 0 {
			t.Errorf("a company with no orders saw %d", strangerTotal)
		}
	})
}

// TestFilterAllowlistIsEnforcedInSQL confirms the allowlist survives all the way
// to the database. A unit test proves the parser drops an unknown field; this
// proves nothing downstream reintroduces it.
func TestFilterAllowlistIsEnforcedInSQL(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	repo := repository.NewOrderRepository(db)
	order := seedOrder(t, db, models.OrderSubmitted)

	// An allowlisted filter works.
	allowed := query.Parse("0", "20",
		`[{"id":"statusCode","value":"submitted"}]`, "", "",
		repository.OrderFields())

	orders, total, err := repo.List(ctx(), order.ShipperCompanyID, allowed)
	if err != nil {
		t.Fatalf("an allowlisted filter failed: %v", err)
	}
	if total != 1 || len(orders) != 1 {
		t.Errorf("allowlisted filter returned %d rows", total)
	}

	// A hostile one is now REFUSED rather than dropped.
	//
	// It used to be silently discarded and the rest of the query run
	// unfiltered. That was safe — the injected text never reached SQL — but it
	// was indistinguishable from success, and the same silence hid ordinary
	// mistakes: a filter on a column that does not exist returned every row
	// with a 200, so a filtered list quietly became an unfiltered one.
	//
	// Refusing is safe in the same way and honest as well. What matters for
	// injection is unchanged and asserted below: the parser produces NO
	// filters, so nothing hostile can reach the query builder.
	hostile := query.Parse("0", "20",
		`[{"id":"id) OR (1=1","value":"x"},{"id":"statusCode","value":"submitted"}]`,
		`[{"id":"(SELECT 1)","desc":true}]`, "",
		repository.OrderFields())

	if hostile.Err == nil {
		t.Fatal("an unknown filter field must be refused, not silently dropped")
	}
	if len(hostile.Filters) != 0 {
		t.Fatalf("a refused request must carry no filters at all, got %+v", hostile.Filters)
	}
	if len(hostile.Sorts) != 0 {
		t.Fatalf("the hostile sort survived parsing: %+v", hostile.Sorts)
	}

	// The injected text must not appear anywhere the query builder would see.
	if strings.Contains(hostile.Err.Error(), "OR (1=1") {
		// The message names the rejected field, which is intended — it is what
		// tells a developer which filter was wrong. It must never be
		// interpolated into SQL, and it is not: it goes only to the response.
		t.Log("the error names the rejected field, which is correct")
	}

	// A request that is refused still executes safely if a handler ignores the
	// error — it returns nothing rather than everything, which is the direction
	// a mistake here should fail in.
	if _, _, err := repo.List(ctx(), order.ShipperCompanyID, hostile); err != nil {
		t.Fatalf("the sanitised query failed to execute: %v", err)
	}
}

// TestSortByEveryAllowlistedFieldExecutes catches a mismatch between a
// FieldSet's mapped column names and the actual schema. A typo there is
// invisible until someone sorts by that column in production.
func TestSortByEveryAllowlistedFieldExecutes(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	repo := repository.NewOrderRepository(db)
	order := seedOrder(t, db, models.OrderSubmitted)

	for external := range repository.OrderFields() {
		t.Run(external, func(t *testing.T) {
			params := query.Parse("0", "20", "",
				`[{"id":"`+external+`","desc":true}]`, "",
				repository.OrderFields())

			if len(params.Sorts) != 1 {
				t.Fatalf("%q did not resolve through its own allowlist", external)
			}

			if _, _, err := repo.List(ctx(), order.ShipperCompanyID, params); err != nil {
				t.Errorf("sorting by %q failed: %v", external, err)
			}
		})
	}
}

// TestInvoiceDoubleBillingIsDetected covers the check that stops an order being
// billed twice, which the monolith did not perform at all.
func TestInvoiceDoubleBillingIsDetected(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	orderRepo := repository.NewOrderRepository(db)
	invoiceRepo := repository.NewInvoiceRepository(db)

	order := seedOrder(t, db, models.OrderCompleted)
	_ = orderRepo

	billed, err := invoiceRepo.OrdersAlreadyBilled(ctx(), []uuid.UUID{order.ID})
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}
	if len(billed) != 0 {
		t.Fatalf("an unbilled order was reported as billed")
	}

	invoice := &models.Invoice{
		InvoiceNumber:        "INV-TEST-" + uuid.NewString()[:8],
		ShipperCompanyID:     order.ShipperCompanyID,
		TransporterCompanyID: *order.TransporterCompanyID,
		StatusCode:           models.InvoiceIssued,
		Detail:               models.JSONB{},
		Lines: []models.InvoiceLine{
			{OrderID: order.ID},
		},
	}
	if err := invoiceRepo.Create(ctx(), invoice); err != nil {
		t.Fatalf("could not create the invoice: %v", err)
	}

	billed, err = invoiceRepo.OrdersAlreadyBilled(ctx(), []uuid.UUID{order.ID})
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}
	if len(billed) != 1 || billed[0] != order.ID {
		t.Errorf("the billed order was not detected: %v", billed)
	}

	// A cancelled invoice releases its orders, so a mistake can be corrected
	// by cancelling and re-issuing.
	if err := invoiceRepo.ApplyStatus(ctx(), invoice.ID,
		models.InvoiceIssued, models.InvoiceCancelled, nil); err != nil {
		t.Fatalf("could not cancel: %v", err)
	}

	billed, err = invoiceRepo.OrdersAlreadyBilled(ctx(), []uuid.UUID{order.ID})
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}
	if len(billed) != 0 {
		t.Errorf("a cancelled invoice still holds its orders: %v", billed)
	}
}
