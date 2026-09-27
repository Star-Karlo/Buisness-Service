package services

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/karlo/business-service/internal/models"
)

// Which rate prices an order when the lane lookup found nothing.
//
// This is the path every agreement-backed order actually takes, because a
// warehouse's city is typed by hand in MyWarehouse ("SEMARANG CITY") while an
// agreement's comes from a picker ("KOTA SEMARANG"), so the two rarely agree
// and FindRate rarely matches. The rule used to be "exactly one rate", which
// quietly refused every order under a multi-shipment contract the moment those
// started storing one rate per lane.
func TestRateFallback(t *testing.T) {
	price := func(v int64) decimal.Decimal { return decimal.NewFromInt(v) }
	str := func(s string) *string { return &s }
	perTruk := str("per-truk")

	origin, destination := uuid.NewString(), uuid.NewString()
	order := &models.Order{OriginWarehouseID: &origin, DestinationWarehouseID: &destination}

	cases := []struct {
		name      string
		rates     []models.AgreementRate
		wantPrice int64
		wantNone  bool
	}{
		{
			name:      "one rate is the contract's price",
			rates:     []models.AgreementRate{{Price: price(5_000_000), PricingTypeID: perTruk}},
			wantPrice: 5_000_000,
		},
		{
			name: "multi-shipment: nine lanes, one tarif",
			rates: []models.AgreementRate{
				{Price: price(5_000_000), PricingTypeID: perTruk, OriginCityID: str("KOTA JAKARTA PUSAT"), DestinationCityID: str("KOTA SEMARANG")},
				{Price: price(5_000_000), PricingTypeID: perTruk, OriginCityID: str("KOTA JAKARTA PUSAT"), DestinationCityID: str("KOTA SALATIGA")},
				{Price: price(5_000_000), PricingTypeID: perTruk, OriginCityID: str("KOTA BEKASI"), DestinationCityID: str("KOTA SEMARANG")},
			},
			wantPrice: 5_000_000,
		},
		{
			name: "lanes priced differently must match the lane",
			rates: []models.AgreementRate{
				{Price: price(5_000_000), PricingTypeID: perTruk, OriginCityID: str("KOTA JAKARTA PUSAT")},
				{Price: price(7_500_000), PricingTypeID: perTruk, OriginCityID: str("KOTA BEKASI")},
			},
			wantNone: true,
		},
		{
			name: "same figure, different pricing type is not the same price",
			rates: []models.AgreementRate{
				{Price: price(5_000_000), PricingTypeID: perTruk},
				{Price: price(5_000_000), PricingTypeID: str("per-kg")},
			},
			wantNone: true,
		},
		{
			name: "a warehouse-specific rate wins over the shared price",
			rates: []models.AgreementRate{
				{Price: price(5_000_000), PricingTypeID: perTruk},
				{Price: price(9_000_000), PricingTypeID: perTruk, OriginWarehouseID: &origin, DestinationWarehouseID: &destination},
			},
			wantPrice: 9_000_000,
		},
		{name: "no rates at all", rates: nil, wantNone: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rateByWarehouseOrOnlyRate(&models.Agreement{Rates: c.rates}, order)
			if c.wantNone {
				if got != nil {
					t.Fatalf("priced the order at %s; the lane has to decide when the rates disagree", got.Price)
				}
				return
			}
			if got == nil {
				t.Fatal("no rate: an order under this agreement would be refused as uncovered")
			}
			if !got.Price.Equal(price(c.wantPrice)) {
				t.Errorf("price = %s, want %d", got.Price, c.wantPrice)
			}
		})
	}
}
