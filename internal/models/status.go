// Package models holds the business entities and the state machines that govern
// them.
package models

import "strings"

// Order statuses, in lifecycle order.
//
// The legacy system stored three parallel strings on every order: statusCode
// (machine-readable), status (Indonesian) and statusAlias (English). All three
// were written by hand at each of the ~46 update sites, so they regularly
// disagreed. Here statusCode is the only stored value and the display strings
// are derived, which makes disagreement impossible.
const (
	OrderDraft           = "draft"
	OrderSubmitted       = "submitted"
	OrderApproved        = "approved"
	OrderReadyToPlan     = "readyToPlan"
	OrderAssigned        = "assigned"
	OrderInTransit       = "inTransit"
	OrderDelivered       = "delivered"
	OrderCompleted       = "completed"
	OrderCancelled       = "cancelled"
	OrderCancelRequested = "cancelRequested"
	OrderRejected        = "rejected"
)

// Shipment statuses. These track the driver app's flow step by step.
//
// In the monolith, `ttd-loading`, `checklist-loading` and `start-loading` all
// routed to one handler, so the three were indistinguishable after the fact.
// Each is a distinct state here.
const (
	ShipmentAssigned          = "assigned"
	ShipmentToLoading         = "toLoading"
	ShipmentAtLoading         = "atLoading"
	ShipmentLoadingApproved   = "loadingApproved"
	ShipmentLoading           = "loading"
	ShipmentLoaded            = "loaded"
	ShipmentToUnloading       = "toUnloading"
	ShipmentAtUnloading       = "atUnloading"
	ShipmentUnloadingApproved = "unloadingApproved"
	ShipmentUnloading         = "unloading"
	ShipmentUnloaded          = "unloaded"
	ShipmentFinished          = "finished"
	ShipmentCancelled         = "cancelled"
)

// Agreement statuses.
//
// pendingApproval and superseded were added with versioning. They are states of
// a VERSION, not of a contract: at any moment one version of a lineage is
// active and the rest are superseded, rejected or waiting on a decision.
const (
	AgreementDraft     = "draft"
	AgreementSubmitted = "submitted"

	// AgreementPendingApproval is a version waiting on a Sales Manager. The
	// version it supersedes stays active meanwhile — an amendment under
	// discussion must not stop work being ordered at the agreed price.
	AgreementPendingApproval = "pendingApproval"

	AgreementActive   = "active"
	AgreementRejected = "rejected"

	// AgreementSuperseded is a version a later one replaced. Distinct from
	// expired, which is a term that ran out: a superseded version may still be
	// within its dates, and orders priced against it stay valid.
	AgreementSuperseded = "superseded"

	AgreementExpired   = "expired"
	AgreementCancelled = "cancelled"
)

// Revision kinds, from the PRD.
const (
	// RevisionRenewal extends a contract whose term has ended. A new version
	// with new dates, referring back.
	RevisionRenewal = "renewal"

	// RevisionUpdate changes price or terms mid-term. The one that needs a
	// Sales Manager's approval, because it alters a bargain already struck.
	RevisionUpdate = "update"
)

// Invoice statuses.
const (
	InvoiceDraft     = "draft"
	InvoiceIssued    = "issued"
	InvoiceSubmitted = "submitted"
	InvoiceVerified  = "verified"
	InvoicePaid      = "paid"
	InvoiceCancelled = "cancelled"
)

// Roles, repeated here rather than imported so the business rules below read as
// one unit.
const (
	RoleSuperadmin   = "superadmin"
	RoleAdmin        = "admin"
	RoleShipper      = "shipper"
	RoleTransporter  = "transporter"
	RoleDriver       = "driver"
	RoleManager      = "manager"
	RoleWarehousePic = "warehousePic"
)

// Transition describes one legal move in a state machine.
type Transition struct {
	// To is the state being entered.
	To string
	// AllowedRoles are the roles permitted to make this move. Empty means any
	// authenticated participant in the order.
	AllowedRoles []string
}

// orderTransitions is the complete order state machine. A move that is not
// listed here cannot happen, which is the property the monolith lacked: there,
// any caller could PUT any status onto any order.
// NormaliseRole maps a token's role onto the machines' vocabulary. The token
// carries the tenant's own role NAME — "Driver", "Administrator", "Owner" —
// and only the ones that spell a persona differently in case ("Driver") are
// personas at all. Anything else is returned untouched; the services then
// resolve it from which side of the order the caller stands on.
func NormaliseRole(role string) string {
	for _, known := range []string{RoleSuperadmin, RoleAdmin, RoleShipper, RoleTransporter, RoleDriver, RoleManager, RoleWarehousePic} {
		if strings.EqualFold(role, known) {
			return known
		}
	}
	return role
}

var orderTransitions = map[string][]Transition{
	OrderDraft: {
		{To: OrderSubmitted, AllowedRoles: []string{RoleShipper, RoleAdmin, RoleSuperadmin}},
		{To: OrderCancelled, AllowedRoles: []string{RoleShipper, RoleAdmin, RoleSuperadmin}},
	},
	OrderSubmitted: {
		{To: OrderApproved, AllowedRoles: []string{RoleTransporter, RoleManager, RoleAdmin, RoleSuperadmin}},
		{To: OrderRejected, AllowedRoles: []string{RoleTransporter, RoleManager, RoleAdmin, RoleSuperadmin}},
		{To: OrderCancelled, AllowedRoles: []string{RoleShipper, RoleAdmin, RoleSuperadmin}},
	},
	OrderApproved: {
		{To: OrderReadyToPlan, AllowedRoles: []string{RoleTransporter, RoleManager, RoleAdmin, RoleSuperadmin}},
		// The console's "Penugasan Pengemudi" tab assigns straight from
		// approved; readyToPlan stays as an optional explicit planning step.
		{To: OrderAssigned, AllowedRoles: []string{RoleTransporter, RoleManager, RoleAdmin, RoleSuperadmin}},
		{To: OrderCancelRequested, AllowedRoles: []string{RoleShipper}},
		{To: OrderCancelled, AllowedRoles: []string{RoleAdmin, RoleSuperadmin}},
	},
	OrderReadyToPlan: {
		{To: OrderAssigned, AllowedRoles: []string{RoleTransporter, RoleManager, RoleAdmin, RoleSuperadmin}},
		{To: OrderCancelRequested, AllowedRoles: []string{RoleShipper}},
		{To: OrderCancelled, AllowedRoles: []string{RoleAdmin, RoleSuperadmin}},
	},
	OrderAssigned: {
		// The move to inTransit is driven by the shipment machine, not by a
		// user, so it carries no role.
		{To: OrderInTransit},
		{To: OrderReadyToPlan, AllowedRoles: []string{RoleTransporter, RoleManager, RoleAdmin, RoleSuperadmin}},
		{To: OrderCancelRequested, AllowedRoles: []string{RoleShipper}},
		{To: OrderCancelled, AllowedRoles: []string{RoleAdmin, RoleSuperadmin}},
	},
	OrderInTransit: {
		{To: OrderDelivered},
		{To: OrderCancelled, AllowedRoles: []string{RoleAdmin, RoleSuperadmin}},
	},
	OrderDelivered: {
		{To: OrderCompleted, AllowedRoles: []string{RoleShipper, RoleWarehousePic, RoleAdmin, RoleSuperadmin}},
	},
	OrderCancelRequested: {
		{To: OrderCancelled, AllowedRoles: []string{RoleTransporter, RoleManager, RoleAdmin, RoleSuperadmin}},
		// A refused cancellation returns the order to where it came from. The
		// caller supplies the prior state; see CanTransitionOrder.
		{To: OrderReadyToPlan, AllowedRoles: []string{RoleTransporter, RoleManager, RoleAdmin, RoleSuperadmin}},
		{To: OrderAssigned, AllowedRoles: []string{RoleTransporter, RoleManager, RoleAdmin, RoleSuperadmin}},
	},
	// Terminal states.
	OrderCompleted: {},
	OrderCancelled: {},
	OrderRejected:  {},
}

// shipmentTransitions is the shipment state machine. Each step names the single
// role that may take it: a driver reports movement, a warehouse PIC approves.
var shipmentTransitions = map[string][]Transition{
	ShipmentAssigned: {
		{To: ShipmentToLoading, AllowedRoles: []string{RoleDriver}},
		{To: ShipmentCancelled, AllowedRoles: []string{RoleTransporter, RoleManager, RoleAdmin, RoleSuperadmin}},
	},
	ShipmentToLoading: {
		{To: ShipmentAtLoading, AllowedRoles: []string{RoleDriver}},
		{To: ShipmentCancelled, AllowedRoles: []string{RoleTransporter, RoleManager, RoleAdmin, RoleSuperadmin}},
	},
	ShipmentAtLoading: {
		// The warehouse checks the driver in before loading may begin.
		{To: ShipmentLoadingApproved, AllowedRoles: []string{RoleWarehousePic, RoleAdmin, RoleSuperadmin}},
	},
	ShipmentLoadingApproved: {
		{To: ShipmentLoading, AllowedRoles: []string{RoleDriver}},
	},
	ShipmentLoading: {
		{To: ShipmentLoaded, AllowedRoles: []string{RoleDriver}},
	},
	ShipmentLoaded: {
		{To: ShipmentToUnloading, AllowedRoles: []string{RoleDriver}},
	},
	ShipmentToUnloading: {
		{To: ShipmentAtUnloading, AllowedRoles: []string{RoleDriver}},
	},
	ShipmentAtUnloading: {
		{To: ShipmentUnloadingApproved, AllowedRoles: []string{RoleWarehousePic, RoleAdmin, RoleSuperadmin}},
	},
	ShipmentUnloadingApproved: {
		{To: ShipmentUnloading, AllowedRoles: []string{RoleDriver}},
	},
	ShipmentUnloading: {
		{To: ShipmentUnloaded, AllowedRoles: []string{RoleDriver}},
	},
	ShipmentUnloaded: {
		// The warehouse signs off, which completes the shipment.
		{To: ShipmentFinished, AllowedRoles: []string{RoleWarehousePic, RoleAdmin, RoleSuperadmin}},
	},
	ShipmentFinished:  {},
	ShipmentCancelled: {},
}

// TransitionError explains why a state change was refused.
type TransitionError struct {
	From   string
	To     string
	Role   string
	Reason string
}

func (e *TransitionError) Error() string {
	return "cannot move from " + e.From + " to " + e.To + ": " + e.Reason
}

// CanTransitionOrder reports whether an order may move between two states for a
// caller in the given role.
func CanTransitionOrder(from, to, role string) error {
	return canTransition(orderTransitions, from, to, role)
}

// CanTransitionShipment reports whether a shipment may move between two states.
func CanTransitionShipment(from, to, role string) error {
	return canTransition(shipmentTransitions, from, to, role)
}

func canTransition(machine map[string][]Transition, from, to, role string) error {
	allowed, known := machine[from]
	if !known {
		return &TransitionError{From: from, To: to, Role: role, Reason: "unknown current state"}
	}
	if len(allowed) == 0 {
		return &TransitionError{From: from, To: to, Role: role, Reason: from + " is a final state"}
	}

	for _, t := range allowed {
		if t.To != to {
			continue
		}
		// Platform administrators may take any listed transition. Restricting
		// them would leave stuck orders with no way out but a database edit,
		// which is what the legacy "reset-status-parent" endpoints existed for.
		if len(t.AllowedRoles) == 0 || role == RoleSuperadmin || role == RoleAdmin {
			return nil
		}
		for _, r := range t.AllowedRoles {
			if r == role {
				return nil
			}
		}
		return &TransitionError{From: from, To: to, Role: role, Reason: "role " + role + " may not make this change"}
	}

	return &TransitionError{From: from, To: to, Role: role, Reason: "no such transition"}
}

// NextOrderStates lists the moves available from a state, so a client can
// render only the buttons that will work.
func NextOrderStates(from, role string) []string {
	var out []string
	for _, t := range orderTransitions[from] {
		if canTransition(orderTransitions, from, t.To, role) == nil {
			out = append(out, t.To)
		}
	}
	return out
}

// NextShipmentStates lists the moves available from a shipment state.
func NextShipmentStates(from, role string) []string {
	var out []string
	for _, t := range shipmentTransitions[from] {
		if canTransition(shipmentTransitions, from, t.To, role) == nil {
			out = append(out, t.To)
		}
	}
	return out
}

// IsOrderFinal reports whether an order can no longer change.
func IsOrderFinal(status string) bool {
	t, known := orderTransitions[status]
	return known && len(t) == 0
}

// Domain names the entity a status code belongs to.
//
// Status codes are only unique within a domain: "draft", "submitted",
// "assigned" and "cancelled" each appear on more than one entity, and they do
// not all mean the same thing. A shipment that is "assigned" has a driver; an
// order that is "assigned" has a driver and a truck. Labels are therefore
// looked up per domain rather than from one flat table.
type Domain string

const (
	DomainOrder     Domain = "order"
	DomainShipment  Domain = "shipment"
	DomainAgreement Domain = "agreement"
	DomainInvoice   Domain = "invoice"
)

type label struct{ ID, EN string }

// statusLabels holds the display strings, derived rather than stored. This is
// what stops the three legacy status columns from drifting apart: there is one
// place to change a wording, and no way for a caller to write a status string
// that disagrees with the code beside it.
var statusLabels = map[Domain]map[string]label{
	DomainOrder: {
		OrderDraft:           {"Draft", "Draft"},
		OrderSubmitted:       {"Menunggu Persetujuan", "Awaiting Approval"},
		OrderApproved:        {"Disetujui", "Approved"},
		OrderReadyToPlan:     {"Siap Direncanakan", "Ready to Plan"},
		OrderAssigned:        {"Driver Ditugaskan", "Driver Assigned"},
		OrderInTransit:       {"Dalam Perjalanan", "In Transit"},
		OrderDelivered:       {"Terkirim", "Delivered"},
		OrderCompleted:       {"Selesai", "Completed"},
		OrderCancelled:       {"Order Dibatalkan", "Order Cancelled"},
		OrderCancelRequested: {"Permintaan Pembatalan", "Cancellation Requested"},
		OrderRejected:        {"Ditolak", "Rejected"},
	},
	DomainShipment: {
		ShipmentAssigned:          {"Ditugaskan", "Assigned"},
		ShipmentToLoading:         {"Menuju Muat", "Heading to Loading"},
		ShipmentAtLoading:         {"Tiba di Titik Muat", "Arrived at Loading"},
		ShipmentLoadingApproved:   {"Muat Disetujui", "Loading Approved"},
		ShipmentLoading:           {"Proses Muat", "Loading"},
		ShipmentLoaded:            {"Selesai Muat", "Loaded"},
		ShipmentToUnloading:       {"Menuju Bongkar", "Heading to Unloading"},
		ShipmentAtUnloading:       {"Tiba di Titik Bongkar", "Arrived at Unloading"},
		ShipmentUnloadingApproved: {"Bongkar Disetujui", "Unloading Approved"},
		ShipmentUnloading:         {"Proses Bongkar", "Unloading"},
		ShipmentUnloaded:          {"Selesai Bongkar", "Unloaded"},
		ShipmentFinished:          {"Pengiriman Selesai", "Shipment Finished"},
		ShipmentCancelled:         {"Dibatalkan", "Cancelled"},
	},
	DomainAgreement: {
		AgreementDraft:     {"Draft", "Draft"},
		AgreementSubmitted: {"Menunggu Persetujuan", "Awaiting Approval"},
		AgreementActive:    {"Aktif", "Active"},
		AgreementRejected:  {"Ditolak", "Rejected"},
		AgreementExpired:   {"Kadaluarsa", "Expired"},
		AgreementCancelled: {"Dibatalkan", "Cancelled"},
	},
	DomainInvoice: {
		InvoiceDraft:     {"Draft", "Draft"},
		InvoiceIssued:    {"Diterbitkan", "Issued"},
		InvoiceSubmitted: {"Diajukan", "Submitted"},
		InvoiceVerified:  {"Terverifikasi", "Verified"},
		InvoicePaid:      {"Lunas", "Paid"},
		InvoiceCancelled: {"Dibatalkan", "Cancelled"},
	},
}

// StatusLabel returns the Indonesian display string for a status within its
// domain, matching the legacy `status` column.
func StatusLabel(domain Domain, code string) string {
	if l, ok := statusLabels[domain][code]; ok {
		return l.ID
	}
	return code
}

// StatusAlias returns the English display string, matching the legacy
// `statusAlias` column.
func StatusAlias(domain Domain, code string) string {
	if l, ok := statusLabels[domain][code]; ok {
		return l.EN
	}
	return code
}

// KnownStatuses lists every status code in a domain, so a client can render a
// filter without hardcoding the list.
func KnownStatuses(domain Domain) []string {
	codes := statusLabels[domain]
	out := make([]string, 0, len(codes))
	for code := range codes {
		out = append(out, code)
	}
	return out
}

// ---------------------------------------------------------------------------
// Which permission a status change needs
// ---------------------------------------------------------------------------

// The transition endpoints take their target status from the request body, so
// the permission they require is not fixed per route and a route-level guard
// cannot express it: cancelling an order and submitting one arrive at the same
// URL. These maps supply the missing half, and the handlers consult them.
//
// Without them the status endpoints were authenticated but not authorised by
// permission at all. Only the company-side and role rules in
// assertTransitionAuthority applied, and those say nothing about permission
// keys — so a member granted order.read and order.create, and deliberately not
// order.cancel, could still cancel any of their company's orders. order.cancel
// and order.approve existed in the catalogue and were checked nowhere.
//
// A target status missing from a map is refused rather than allowed. Adding a
// status without deciding who may reach it should fail closed.

// orderStatusPermission maps an order's target status to the permission needed
// to move it there.
var orderStatusPermission = map[string]string{
	OrderDraft:           "order.update",
	OrderSubmitted:       "order.create",
	OrderApproved:        "order.approve",
	OrderRejected:        "order.approve",
	OrderReadyToPlan:     "order.readyToPlan",
	OrderAssigned:        "order.assignDriver",
	OrderCancelRequested: "order.cancel",
	OrderCancelled:       "order.cancel",

	// The carrying statuses are reached by the people physically doing the
	// work, which is what shipment.update grants.
	OrderInTransit: "shipment.update",
	OrderDelivered: "shipment.update",
	OrderCompleted: "shipment.update",
}

// shipmentStatusPermission maps a shipment's target status to the permission
// needed to move it there.
//
// The two approval steps are separated from ordinary progress deliberately.
// Advancing a shipment is the driver's job; approving what was loaded or
// unloaded is the warehouse's, and it is the step that makes the count binding.
var shipmentStatusPermission = map[string]string{
	ShipmentAssigned:    "shipment.update",
	ShipmentToLoading:   "shipment.update",
	ShipmentAtLoading:   "shipment.update",
	ShipmentLoading:     "shipment.update",
	ShipmentLoaded:      "shipment.update",
	ShipmentToUnloading: "shipment.update",
	ShipmentAtUnloading: "shipment.update",
	ShipmentUnloading:   "shipment.update",
	ShipmentUnloaded:    "shipment.update",
	ShipmentFinished:    "shipment.update",
	ShipmentCancelled:   "order.cancel",

	ShipmentLoadingApproved:   "shipment.verifyLoading",
	ShipmentUnloadingApproved: "shipment.verifyUnloading",
}

// PermissionForOrderStatus returns the permission needed to move an order to a
// status, and whether that status is one anybody may move an order to.
func PermissionForOrderStatus(to string) (string, bool) {
	key, ok := orderStatusPermission[to]
	return key, ok
}

// PermissionForShipmentStatus returns the permission needed to move a shipment
// to a status, and whether that status is one anybody may move a shipment to.
func PermissionForShipmentStatus(to string) (string, bool) {
	key, ok := shipmentStatusPermission[to]
	return key, ok
}
