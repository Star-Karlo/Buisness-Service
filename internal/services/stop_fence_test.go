package services

import (
	"context"
	"testing"

	"github.com/karlo/business-service/internal/models"
)

func TestModeUjiStepsPastAStopsFence(t *testing.T) {
	on := true
	enforced := &models.Order{GeofencingEnabled: &on}
	off := false
	relaxed := &models.Order{GeofencingEnabled: &off}
	svc := &ShipmentService{}

	cases := []struct {
		name  string
		actor Actor
		order *models.Order
		want  bool
	}{
		{"a driver on an enforced order is held to the fence",
			Actor{Role: models.RoleDriver}, enforced, true},
		{"Mode Uji steps past it, as it does at the shipment's own steps",
			Actor{Role: models.RoleDriver, StatusBypass: true}, enforced, false},
		{"so does an administrator",
			Actor{Role: models.RoleAdmin}, enforced, false},
		{"an order with geofencing off enforces nothing",
			Actor{Role: models.RoleDriver}, relaxed, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := svc.fenceEnforcedFor(context.Background(), c.actor, c.order); got != c.want {
				t.Errorf("fenceEnforcedFor = %v, want %v", got, c.want)
			}
		})
	}
}
