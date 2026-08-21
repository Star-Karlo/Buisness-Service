package unit

import (
	"testing"
	"time"

	"github.com/karlo/business-service/internal/models"
	"github.com/shopspring/decimal"
)

// TestInvoiceRecalculate pins the tax arithmetic. PPN is added to the bill and
// PPH23 is withheld from it; reversing either sign changes what the customer
// owes, so the directions are asserted explicitly rather than by round-tripping
// a total.
func TestInvoiceRecalculate(t *testing.T) {
	cases := []struct {
		name                               string
		subtotal, ppnPct, pph23Pct, adjust string
		wantPPN, wantPPH23, wantTotal      string
	}{
		{
			name:     "standard Indonesian freight rates",
			subtotal: "10000000", ppnPct: "0.02", pph23Pct: "0.11", adjust: "0",
			wantPPN:   "200000",
			wantPPH23: "1100000",
			// 10,000,000 + 200,000 - 1,100,000
			wantTotal: "9100000",
		},
		{
			name:     "no tax",
			subtotal: "5000000", ppnPct: "0", pph23Pct: "0", adjust: "0",
			wantPPN: "0", wantPPH23: "0", wantTotal: "5000000",
		},
		{
			name:     "positive adjustment is added",
			subtotal: "1000000", ppnPct: "0.02", pph23Pct: "0.11", adjust: "50000",
			wantPPN: "20000", wantPPH23: "110000",
			wantTotal: "960000",
		},
		{
			name:     "negative adjustment is subtracted",
			subtotal: "1000000", ppnPct: "0.02", pph23Pct: "0.11", adjust: "-50000",
			wantPPN: "20000", wantPPH23: "110000",
			wantTotal: "860000",
		},
		{
			// The reason this uses exact decimals rather than float64: in
			// binary floating point, 0.1 + 0.2 != 0.3, and freight invoices
			// are summed across many lines before tax is applied.
			name:     "amounts that float64 cannot represent exactly",
			subtotal: "0.30", ppnPct: "0.10", pph23Pct: "0", adjust: "0",
			wantPPN: "0.03", wantPPH23: "0", wantTotal: "0.33",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := &models.Invoice{
				Subtotal:        decimal.RequireFromString(tc.subtotal),
				PPNPercentage:   decimal.RequireFromString(tc.ppnPct),
				PPH23Percentage: decimal.RequireFromString(tc.pph23Pct),
				Adjustment:      decimal.RequireFromString(tc.adjust),
			}
			inv.Recalculate()

			assertDecimal(t, "ppnAmount", inv.PPNAmount, tc.wantPPN)
			assertDecimal(t, "pph23Amount", inv.PPH23Amount, tc.wantPPH23)
			assertDecimal(t, "total", inv.Total, tc.wantTotal)
		})
	}
}

func TestInvoiceRecalculateIsIdempotent(t *testing.T) {
	inv := &models.Invoice{
		Subtotal:        decimal.RequireFromString("1234567.89"),
		PPNPercentage:   decimal.RequireFromString("0.02"),
		PPH23Percentage: decimal.RequireFromString("0.11"),
		Adjustment:      decimal.Zero,
	}

	inv.Recalculate()
	first := inv.Total

	// Recomputing must not compound: the tax is a function of the subtotal, not
	// of the running total.
	inv.Recalculate()
	inv.Recalculate()

	if !inv.Total.Equal(first) {
		t.Errorf("total drifted on repeated calculation: %s then %s", first, inv.Total)
	}
}

func assertDecimal(t *testing.T, field string, got decimal.Decimal, want string) {
	t.Helper()
	expected := decimal.RequireFromString(want)
	if !got.Equal(expected) {
		t.Errorf("%s = %s, want %s", field, got, expected)
	}
}

// TestAgreementIsUsable covers the rules that gate placing an order.
func TestAgreementIsUsable(t *testing.T) {
	day := func(s string) time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatalf("bad test date %q: %v", s, err)
		}
		return d
	}

	base := models.Agreement{
		StatusCode: models.AgreementActive,
		ValidFrom:  day("2026-01-01"),
		ValidUntil: day("2026-12-31"),
		Verified:   false,
	}

	cases := []struct {
		name   string
		mutate func(*models.Agreement)
		now    time.Time
		strict bool
		want   bool
	}{
		{"active and in range", nil, day("2026-06-15"), false, true},
		{"on the first day", nil, day("2026-01-01"), false, true},
		// Validity is inclusive at both ends: an agreement valid until the 31st
		// can still be used on the 31st.
		{"on the last day", nil, day("2026-12-31"), false, true},
		{"the day after expiry", nil, day("2027-01-01"), false, false},
		{"the day before it starts", nil, day("2025-12-31"), false, false},
		{"unverified under strict checking", nil, day("2026-06-15"), true, false},
		{
			"verified under strict checking",
			func(a *models.Agreement) { a.Verified = true },
			day("2026-06-15"), true, true,
		},
		{
			"still a draft",
			func(a *models.Agreement) { a.StatusCode = models.AgreementDraft },
			day("2026-06-15"), false, false,
		},
		{
			"already expired",
			func(a *models.Agreement) { a.StatusCode = models.AgreementExpired },
			day("2026-06-15"), false, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			if tc.mutate != nil {
				tc.mutate(&a)
			}
			if got := a.IsUsable(tc.now, tc.strict); got != tc.want {
				t.Errorf("IsUsable(%s, strict=%v) = %v, want %v",
					tc.now.Format("2006-01-02"), tc.strict, got, tc.want)
			}
		})
	}
}
