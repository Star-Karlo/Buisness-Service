package unit

import (
	"testing"

	"github.com/karlo/business-service/internal/models"
)

// TestOrderTransitionsAllowed pins the moves the business genuinely needs.
func TestOrderTransitionsAllowed(t *testing.T) {
	cases := []struct {
		from, to, role string
	}{
		{models.OrderDraft, models.OrderSubmitted, models.RoleShipper},
		{models.OrderSubmitted, models.OrderApproved, models.RoleTransporter},
		{models.OrderSubmitted, models.OrderRejected, models.RoleManager},
		{models.OrderApproved, models.OrderReadyToPlan, models.RoleTransporter},
		{models.OrderReadyToPlan, models.OrderAssigned, models.RoleManager},
		{models.OrderDelivered, models.OrderCompleted, models.RoleWarehousePic},
		// A platform administrator may take any listed transition, so that a
		// stuck order never requires a manual database edit.
		{models.OrderReadyToPlan, models.OrderAssigned, models.RoleAdmin},
		{models.OrderDraft, models.OrderCancelled, models.RoleSuperadmin},
	}

	for _, tc := range cases {
		if err := models.CanTransitionOrder(tc.from, tc.to, tc.role); err != nil {
			t.Errorf("%s -> %s as %s should be allowed: %v", tc.from, tc.to, tc.role, err)
		}
	}
}

// TestOrderTransitionsRefused is the important half: these are the moves the
// monolith permitted because every status change went through one generic
// handler that wrote whatever the request body contained.
func TestOrderTransitionsRefused(t *testing.T) {
	cases := []struct {
		name           string
		from, to, role string
	}{
		{"driver cannot approve an order", models.OrderSubmitted, models.OrderApproved, models.RoleDriver},
		{"shipper cannot assign a driver", models.OrderReadyToPlan, models.OrderAssigned, models.RoleShipper},
		{"cannot skip from draft straight to completed", models.OrderDraft, models.OrderCompleted, models.RoleShipper},
		{"cannot skip approval", models.OrderDraft, models.OrderAssigned, models.RoleTransporter},
		{"cannot reopen a completed order", models.OrderCompleted, models.OrderInTransit, models.RoleShipper},
		{"cannot reopen a cancelled order", models.OrderCancelled, models.OrderDraft, models.RoleShipper},
		{"cannot move backwards from delivered", models.OrderDelivered, models.OrderAssigned, models.RoleTransporter},
		{"unknown source state is refused", "nonsense", models.OrderDraft, models.RoleAdmin},
		{"unknown target state is refused", models.OrderDraft, "nonsense", models.RoleAdmin},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := models.CanTransitionOrder(tc.from, tc.to, tc.role); err == nil {
				t.Errorf("%s -> %s as %s should be refused", tc.from, tc.to, tc.role)
			}
		})
	}
}

// TestShipmentLifecycleInOrder walks the full driver flow, confirming each step
// is reachable only from the one before it.
func TestShipmentLifecycleInOrder(t *testing.T) {
	steps := []struct {
		to   string
		role string
	}{
		{models.ShipmentToLoading, models.RoleDriver},
		{models.ShipmentAtLoading, models.RoleDriver},
		{models.ShipmentLoading, models.RoleDriver},
		{models.ShipmentLoaded, models.RoleDriver},
		{models.ShipmentToUnloading, models.RoleDriver},
		{models.ShipmentAtUnloading, models.RoleDriver},
		{models.ShipmentUnloading, models.RoleDriver},
		{models.ShipmentUnloaded, models.RoleDriver},
		{models.ShipmentFinished, models.RoleWarehousePic},
	}

	current := models.ShipmentAssigned
	for _, step := range steps {
		if err := models.CanTransitionShipment(current, step.to, step.role); err != nil {
			t.Fatalf("%s -> %s as %s should be allowed: %v", current, step.to, step.role, err)
		}
		current = step.to
	}

	if current != models.ShipmentFinished {
		t.Fatalf("lifecycle ended at %s", current)
	}
}

// TestShipmentRoleSeparation confirms the driver/warehouse split the monolith
// lost when it aliased scan-code, checklist and approval onto one handler.
func TestShipmentRoleSeparation(t *testing.T) {
	// The warehouse's check-in is no longer a step: an arrived driver starts
	// loading, and "Muat Disetujui" is not reachable from arrival.
	if err := models.CanTransitionShipment(models.ShipmentAtLoading, models.ShipmentLoadingApproved, models.RoleWarehousePic); err == nil {
		t.Error("loading approval must not be a step in the flow any more")
	}
	// Nor sign off their own delivery.
	if err := models.CanTransitionShipment(models.ShipmentUnloaded, models.ShipmentFinished, models.RoleDriver); err == nil {
		t.Error("a driver must not be able to finish their own shipment")
	}
	// And a warehouse PIC does not report movement on the driver's behalf.
	if err := models.CanTransitionShipment(models.ShipmentAssigned, models.ShipmentToLoading, models.RoleWarehousePic); err == nil {
		t.Error("a warehouse PIC must not report the driver's departure")
	}
	// Skipping the loading step entirely must fail.
	if err := models.CanTransitionShipment(models.ShipmentAtLoading, models.ShipmentLoaded, models.RoleDriver); err == nil {
		t.Error("loading must not be skippable")
	}
}

// TestStatusLabelsAreDomainScoped is the regression test for the collision
// found while building this: "draft", "submitted", "assigned" and "cancelled"
// each exist on more than one entity and do not always mean the same thing.
func TestStatusLabelsAreDomainScoped(t *testing.T) {
	// Same code, different entity, different meaning.
	orderAssigned := models.StatusAlias(models.DomainOrder, models.OrderAssigned)
	shipmentAssigned := models.StatusAlias(models.DomainShipment, models.ShipmentAssigned)
	if orderAssigned == shipmentAssigned {
		t.Errorf("order and shipment %q should not share a label (%q)", "assigned", orderAssigned)
	}

	orderCancelled := models.StatusAlias(models.DomainOrder, models.OrderCancelled)
	invoiceCancelled := models.StatusAlias(models.DomainInvoice, models.InvoiceCancelled)
	if orderCancelled == invoiceCancelled {
		t.Errorf("order and invoice %q should not share a label (%q)", "cancelled", orderCancelled)
	}

	// A code from the wrong domain must not resolve to another domain's label.
	if got := models.StatusLabel(models.DomainInvoice, models.OrderInTransit); got != models.OrderInTransit {
		t.Errorf("an order status resolved inside the invoice domain: got %q", got)
	}

	// Every declared status must have a label in its own domain.
	domains := map[models.Domain][]string{
		models.DomainOrder:     models.KnownStatuses(models.DomainOrder),
		models.DomainShipment:  models.KnownStatuses(models.DomainShipment),
		models.DomainAgreement: models.KnownStatuses(models.DomainAgreement),
		models.DomainInvoice:   models.KnownStatuses(models.DomainInvoice),
	}
	for domain, codes := range domains {
		if len(codes) == 0 {
			t.Errorf("domain %s declares no statuses", domain)
		}
		for _, code := range codes {
			if models.StatusLabel(domain, code) == code {
				t.Errorf("%s/%s has no Indonesian label", domain, code)
			}
			if models.StatusAlias(domain, code) == code {
				t.Errorf("%s/%s has no English label", domain, code)
			}
		}
	}
}

func TestNextStatesReflectRole(t *testing.T) {
	shipperNext := models.NextOrderStates(models.OrderReadyToPlan, models.RoleShipper)
	for _, s := range shipperNext {
		if s == models.OrderAssigned {
			t.Error("a shipper must not be offered the assign transition")
		}
	}

	transporterNext := models.NextOrderStates(models.OrderReadyToPlan, models.RoleTransporter)
	var found bool
	for _, s := range transporterNext {
		if s == models.OrderAssigned {
			found = true
		}
	}
	if !found {
		t.Error("a transporter should be offered the assign transition")
	}

	if states := models.NextOrderStates(models.OrderCompleted, models.RoleAdmin); len(states) != 0 {
		t.Errorf("a completed order should offer no transitions, got %v", states)
	}
	if !models.IsOrderFinal(models.OrderCompleted) {
		t.Error("completed should be a final state")
	}
	if models.IsOrderFinal(models.OrderDraft) {
		t.Error("draft should not be a final state")
	}
}
