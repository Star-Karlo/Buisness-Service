package services

import (
	"context"

	"github.com/shopspring/decimal"

	"github.com/karlo/business-service/internal/models"
)

// An allowance agreed on the contract rather than worked out per order.
//
// Most contracts leave the driver's allowance to the order, where it is
// computed from that trip's own distance and time, and a planner may revise
// each component. Some contracts fix it instead: a figure agreed once for the
// lane, split into what the driver is paid before leaving and what follows
// after reconciliation. Every order under such a contract reports the same two
// numbers.
//
// This is ONE rule, not a second code path: the allowance is the contract's
// when the contract has one, and computed when it has not. A contract that
// agrees nothing — which is nearly all of them — is unaffected, and no code
// anywhere asks which company it belongs to.

// AllowanceSnapshot is the contract's own figures, as an order reports them.
type AllowanceSnapshot struct {
	// AgreementNumber is shown beside the figures, because the planner's
	// first question about a number they cannot edit is where it came from.
	AgreementNumber string `json:"agreementNumber"`

	Total          models.Money `json:"total"`
	UpfrontPercent models.Money `json:"upfrontPercent"`
	// Upfront is paid before the trip; Final follows reconciliation. Stored
	// rather than recomputed from the percentage so that a contract whose
	// percentage is later amended does not silently restate what was paid.
	Upfront models.Money `json:"upfront"`
	Final   models.Money `json:"final"`
}

// allowanceSnapshotOf reads the contract's agreed allowance, or nil when the
// contract agrees none — which leaves the order to compute its own.
func allowanceSnapshotOf(agreement *models.Agreement) *AllowanceSnapshot {
	if agreement == nil {
		return nil
	}
	raw, ok := agreement.Detail["allowance"].(map[string]interface{})
	if !ok {
		return nil
	}
	total, hasTotal := moneyFromDetail(raw["total"])
	upfront, hasUpfront := moneyFromDetail(raw["upfront"])
	final, hasFinal := moneyFromDetail(raw["final"])
	// All three or none. A half-written snapshot is worse than none: it would
	// show the driver a figure the contract never agreed.
	if !hasTotal || !hasUpfront || !hasFinal {
		return nil
	}
	percent, _ := moneyFromDetail(raw["upfrontPercent"])
	return &AllowanceSnapshot{
		AgreementNumber: agreement.AgreementNumber,
		Total:           total,
		UpfrontPercent:  percent,
		Upfront:         upfront,
		Final:           final,
	}
}

// moneyFromDetail reads a number out of JSONB, where it may have arrived as a
// JSON number or as a string.
func moneyFromDetail(v interface{}) (models.Money, bool) {
	switch n := v.(type) {
	case float64:
		return decimal.NewFromFloat(n), true
	case string:
		m, err := decimal.NewFromString(n)
		if err != nil {
			return models.Money{}, false
		}
		return m, true
	}
	return models.Money{}, false
}

// snapshotFor is the contract's allowance for one order, if it has one.
func (s *AllowanceService) snapshotFor(ctx context.Context, order *models.Order) *AllowanceSnapshot {
	if s.agreements == nil || order.AgreementID == nil {
		return nil
	}
	agreement, err := s.agreements.FindByIDForService(ctx, *order.AgreementID)
	if err != nil {
		return nil
	}
	return allowanceSnapshotOf(agreement)
}
