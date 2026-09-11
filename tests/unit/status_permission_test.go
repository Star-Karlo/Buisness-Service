package unit

import (
	"testing"

	"github.com/karlo/business-service/internal/models"
)

// TestEveryOrderStatusHasAPermission makes sure no status can be reached
// without someone having decided who may reach it.
//
// The maps fail closed, so a status added without an entry is refused rather
// than allowed. That is the safe direction, but it is still a bug — it would
// silently break a legitimate transition. This catches it at test time instead.
func TestEveryOrderStatusHasAPermission(t *testing.T) {
	for _, status := range []string{
		models.OrderDraft,
		models.OrderSubmitted,
		models.OrderApproved,
		models.OrderRejected,
		models.OrderReadyToPlan,
		models.OrderAssigned,
		models.OrderInTransit,
		models.OrderDelivered,
		models.OrderCompleted,
		models.OrderCancelRequested,
		models.OrderCancelled,
	} {
		if _, ok := models.PermissionForOrderStatus(status); !ok {
			t.Errorf("order status %q has no permission mapped, so nobody can reach it", status)
		}
	}

	for _, status := range []string{
		models.ShipmentAssigned,
		models.ShipmentToLoading,
		models.ShipmentAtLoading,
		models.ShipmentLoadingApproved,
		models.ShipmentLoading,
		models.ShipmentLoaded,
		models.ShipmentToUnloading,
		models.ShipmentAtUnloading,
		models.ShipmentUnloadingApproved,
		models.ShipmentUnloading,
		models.ShipmentUnloaded,
		models.ShipmentFinished,
		models.ShipmentCancelled,
	} {
		if _, ok := models.PermissionForShipmentStatus(status); !ok {
			t.Errorf("shipment status %q has no permission mapped, so nobody can reach it", status)
		}
	}
}

// TestDestructiveStatusesNeedTheirOwnPermission pins the distinctions that
// motivated the maps.
//
// The bug was that the status endpoints consulted no permission at all: a
// member holding order.read and order.create, and deliberately not
// order.cancel, could cancel any of their company's orders. Only the
// company-side and role rules applied, and those say nothing about permission
// keys. Cancelling must not share a permission with creating, and approving
// must not share one with either.
func TestDestructiveStatusesNeedTheirOwnPermission(t *testing.T) {
	cancel, _ := models.PermissionForOrderStatus(models.OrderCancelled)
	submit, _ := models.PermissionForOrderStatus(models.OrderSubmitted)
	approve, _ := models.PermissionForOrderStatus(models.OrderApproved)

	if cancel != "order.cancel" {
		t.Errorf("cancelling an order must need order.cancel, got %q", cancel)
	}
	if cancel == submit {
		t.Error("cancelling must not be reachable with the permission that submits")
	}
	if approve == submit || approve == cancel {
		t.Error("approving must need a permission of its own")
	}

	// Approving what was loaded is the warehouse's decision and is what makes
	// the count binding, so it cannot ride on ordinary shipment progress.
	advance, _ := models.PermissionForShipmentStatus(models.ShipmentLoaded)
	verify, _ := models.PermissionForShipmentStatus(models.ShipmentLoadingApproved)
	if advance == verify {
		t.Error("approving loading must not be reachable with the permission that advances a shipment")
	}

	// An unmapped status is refused, not allowed.
	if _, ok := models.PermissionForOrderStatus("somethingInvented"); ok {
		t.Error("an unknown status must not resolve to a permission")
	}
}

// TestOfferAndRefusalUseTheSameRule pins the invariant behind filtering the
// transitions list.
//
// Clients render action buttons directly from /transitions, so the question
// "what may I do" and the question "may I do this" have to be answered from one
// place. When they were answered from two, a member without order.cancel was
// offered a Cancel button that returned 403 — which reads as a broken system
// rather than as a permission they were never given.
//
// Both paths now resolve through PermissionForOrderStatus, so this checks that
// every status the machine can reach HAS an answer there. A status missing from
// the map is filtered out of the offer AND refused on the write, which is
// consistent but silently removes a legitimate action — so it must not happen.
func TestOfferAndRefusalUseTheSameRule(t *testing.T) {
	for _, from := range []string{
		models.OrderDraft, models.OrderSubmitted, models.OrderApproved,
		models.OrderReadyToPlan, models.OrderAssigned, models.OrderInTransit,
		models.OrderDelivered, models.OrderCancelRequested,
	} {
		for _, role := range []string{
			models.RoleShipper, models.RoleTransporter, models.RoleDriver,
			models.RoleWarehousePic, models.RoleAdmin, models.RoleSuperadmin,
		} {
			for _, to := range models.NextOrderStates(from, role) {
				if _, ok := models.PermissionForOrderStatus(to); !ok {
					t.Errorf("the state machine offers %s -> %s for %s, but no "+
						"permission is mapped for %q: it would be filtered out "+
						"of the transitions list and refused on the write",
						from, to, role, to)
				}
			}
		}
	}

	for _, from := range []string{
		models.ShipmentAssigned, models.ShipmentToLoading, models.ShipmentAtLoading,
		models.ShipmentLoadingApproved, models.ShipmentLoading, models.ShipmentLoaded,
		models.ShipmentToUnloading, models.ShipmentAtUnloading,
		models.ShipmentUnloadingApproved, models.ShipmentUnloading,
		models.ShipmentUnloaded,
	} {
		for _, role := range []string{
			models.RoleShipper, models.RoleTransporter, models.RoleDriver,
			models.RoleWarehousePic, models.RoleAdmin, models.RoleSuperadmin,
		} {
			for _, to := range models.NextShipmentStates(from, role) {
				if _, ok := models.PermissionForShipmentStatus(to); !ok {
					t.Errorf("the state machine offers shipment %s -> %s for %s, "+
						"but no permission is mapped for %q", from, to, role, to)
				}
			}
		}
	}
}
