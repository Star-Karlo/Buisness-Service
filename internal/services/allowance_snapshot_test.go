package services

import (
	"testing"

	"github.com/karlo/business-service/internal/models"
)

func agreementWithDetail(d models.JSONB) *models.Agreement {
	return &models.Agreement{AgreementNumber: "AGR-TEST-0001", Detail: d}
}

func TestAContractWithoutAnAgreedAllowanceLeavesItToTheOrder(t *testing.T) {
	for name, a := range map[string]*models.Agreement{
		"no agreement at all": nil,
		"no detail":           agreementWithDetail(nil),
		"no allowance key":    agreementWithDetail(models.JSONB{"something": "else"}),
		// Half-written is worse than absent: it would show a driver a figure
		// the contract never agreed.
		"total only": agreementWithDetail(models.JSONB{
			"allowance": map[string]interface{}{"total": 1000.0},
		}),
		"missing the final share": agreementWithDetail(models.JSONB{
			"allowance": map[string]interface{}{"total": 1000.0, "upfront": 600.0},
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if got := allowanceSnapshotOf(a); got != nil {
				t.Errorf("want no snapshot, got %+v", got)
			}
		})
	}
}

func TestAContractThatAgreedAnAllowanceDecidesIt(t *testing.T) {
	a := agreementWithDetail(models.JSONB{
		"allowance": map[string]interface{}{
			"total": 1023148.0, "upfrontPercent": 60.0,
			"upfront": 613889.0, "final": 409259.0,
		},
	})
	got := allowanceSnapshotOf(a)
	if got == nil {
		t.Fatal("a contract with a complete allowance should decide it")
	}
	if got.AgreementNumber != "AGR-TEST-0001" {
		t.Errorf("agreementNumber = %q — the planner has to be able to see where an uneditable figure came from", got.AgreementNumber)
	}
	for _, c := range []struct {
		name string
		got  models.Money
		want float64
	}{
		{"total", got.Total, 1023148},
		{"upfrontPercent", got.UpfrontPercent, 60},
		{"upfront", got.Upfront, 613889},
		{"final", got.Final, 409259},
	} {
		if !got.Total.IsPositive() || c.got.InexactFloat64() != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// The two shares are read as stored, not recomputed from the percentage:
	// amending the percentage later must not restate what was already paid.
	if sum := got.Upfront.Add(got.Final); sum.InexactFloat64() != 1023148 {
		t.Errorf("upfront + final = %v, want the agreed total", sum)
	}
}

func TestFiguresSurviveArrivingAsStrings(t *testing.T) {
	// JSONB gives back whatever was written: the console may send a decimal
	// as a string to avoid float rounding.
	a := agreementWithDetail(models.JSONB{
		"allowance": map[string]interface{}{
			"total": "1023148.50", "upfront": "613889.10", "final": "409259.40",
		},
	})
	got := allowanceSnapshotOf(a)
	if got == nil {
		t.Fatal("string figures should be read, not discarded")
	}
	if got.Total.String() != "1023148.5" {
		t.Errorf("total = %s, want 1023148.5", got.Total)
	}
}
