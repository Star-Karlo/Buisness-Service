//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/repository"
	"github.com/karlo/business-service/internal/services"
)

// An order under a contract that FIXED the driver's allowance.
//
// No planner ever enters an advance for such an order: Save refuses one,
// because the contract decided the figures. Save is also the only thing that
// writes an order_allowances row — so none existed, and the two steps that
// read it both failed. Finalising answered "there is no open advance to
// finalise", and reconciliation afterwards had nothing to reconcile against.
//
// Finalising must therefore materialise the contract's figures: the contract
// decides WHAT is paid, the order still records THAT it was paid.
func TestFinaliseUnderContractAllowanceWritesTheAdvance(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	companyID := uuid.New()
	agreementID := uuid.New()
	orderID := uuid.New()
	actorID := uuid.New()
	// Unique per run: this suite does not truncate, and a fixed number would
	// pass once and then collide on the agreement's unique index.
	tag := uuid.NewString()[:8]

	if err := db.Exec(`INSERT INTO agreements
		(id, agreement_number, shipper_company_id, transporter_company_id, created_by_user_id,
		 status_code, valid_from, valid_until, verified, detail, version, root_agreement_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'active', NOW(), NOW() + interval '1 year', TRUE, ?::jsonb, 1, ?, NOW(), NOW())`,
		agreementID, "AGR-PROBE-"+tag, uuid.New(), companyID, actorID,
		`{"allowance":{"total":"500000","upfront":"300000","final":"200000","upfrontPercent":"60"}}`,
		agreementID,
	).Error; err != nil {
		t.Fatalf("agreement: %v", err)
	}

	if err := db.Exec(`INSERT INTO orders
		(id, order_number, shipper_company_id, transporter_company_id, created_by_user_id,
		 agreement_id, detail, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '{}'::jsonb, NOW(), NOW())`,
		orderID, "ORD-PROBE-"+tag, companyID, companyID, actorID, agreementID).Error; err != nil {
		t.Fatalf("order: %v", err)
	}

	svc := services.NewAllowanceService(
		repository.NewOrderRepository(db),
		repository.NewAllowanceRepository(db),
		repository.NewOrderRouteRepository(db),
		nil, nil,
	).WithAgreements(repository.NewAgreementRepository(db))

	actor := services.Actor{CompanyID: companyID, UserID: actorID}

	view, err := svc.Get(ctx, actor, orderID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	t.Logf("BEFORE: snapshot=%v advanceRow=%v editable=%v",
		view.Snapshot != nil, view.Allowance != nil, view.Editable)
	if view.Snapshot == nil {
		t.Fatal("the contract's allowance was not read; the rest of this test proves nothing")
	}

	if _, err := svc.Finalise(ctx, actor, orderID); err != nil {
		t.Fatalf("FINALISE FAILED: %v", err)
	}

	after, err := svc.Get(ctx, actor, orderID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.Allowance == nil {
		t.Fatal("finalised, but no advance row exists — reconciliation will have nothing to read")
	}
	t.Logf("AFTER: total=%s finalised=%v components=%d",
		after.Allowance.Total.String(), after.Allowance.FinalisedAt != nil, len(after.Allowance.Components))
	if after.Allowance.FinalisedAt == nil {
		t.Error("advance row is not finalised")
	}
	if after.Allowance.Total.String() != "500000" {
		t.Errorf("advance total is %s, the contract agreed 500000", after.Allowance.Total.String())
	}
}
