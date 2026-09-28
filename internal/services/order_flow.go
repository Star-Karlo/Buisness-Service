package services

import (
	"fmt"

	"github.com/karlo/business-service/internal/models"
)

// Single-shipment and multi-shipment orders are two flows, not one flow with
// a count.
//
// They share every screen name and every status, and that is exactly why they
// were easy to conflate. What differs is what a step means. On a
// single-shipment order there is one load, one unload, one set of documents,
// one plan; "Mulai Bongkar" is the unloading. On a multi-shipment order each
// shipment is a pair of points with its own cargo and its own documents, the
// planner may choose the order the points are visited in, and "Mulai Bongkar"
// is one of several — the order does not finish until the last one is signed.
//
// Both are the GENERAL flow: neither belongs to a customer. Per-customer
// differences are differences of DATA — which agreement, which fields an
// order must carry, what a document is called — and they attach on top of
// whichever of these two flows the order is running. They are not a third
// flow, and nothing below should ever branch on a company.
const (
	FlowSingle = "single"
	FlowMulti  = "multi"
)

// OrderFlow says which of the two an order is running.
//
// Read from the shape of the order rather than a stored flag, so an order
// cannot claim one flow and carry the points of the other. One pair of points
// is the single-shipment flow; that is also what every order placed before
// multi-shipment existed looks like, which is why those keep working
// untouched.
func OrderFlow(order *models.Order) string {
	if ShipmentCount(order) > 1 {
		return FlowMulti
	}
	return FlowSingle
}

// ShipmentCount is how many shipments the order carries.
//
// The point lists are meant to be the same length — index k of each is the
// k-th shipment — so the count is that length. The longer of the two is taken
// so a half-filled wizard still reports the number of shipments the planner
// is describing rather than silently dropping one.
func ShipmentCount(order *models.Order) int {
	loads := len(warehouseList(order.Detail, "loadingPoints"))
	unloads := len(warehouseList(order.Detail, "unloadingPoints"))
	n := loads
	if unloads > n {
		n = unloads
	}
	if n == 0 {
		return 1
	}
	return n
}

// stopRef is one entry of the planner's visit order: a kind and a 0-based
// index into that kind's point list.
type stopRef struct {
	Kind  string
	Index int
}

// visitOrder is the order the points are visited in.
//
// The default is every loading point, then every unloading point: load the
// whole truck, then deliver it. A planner who wants Muat 1 → Bongkar 1 →
// Muat 2 → Bongkar 2 says so in Allocate, and that choice is stored on the
// order because it describes the trip, not a point.
//
// An order whose stored sequence does not describe its points — a point
// added after the sequence was chosen, an index out of range, a point named
// twice or not at all — falls back to the default rather than routing a truck
// through a list that has a hole in it.
func visitOrder(order *models.Order, loads, unloads int) []stopRef {
	def := make([]stopRef, 0, loads+unloads)
	for i := 0; i < loads; i++ {
		def = append(def, stopRef{Kind: models.StopLoad, Index: i})
	}
	for i := 0; i < unloads; i++ {
		def = append(def, stopRef{Kind: models.StopUnload, Index: i})
	}

	chosen := storedSequence(order.Detail)
	if len(chosen) == 0 {
		return def
	}
	if err := validateSequence(chosen, loads, unloads); err != nil {
		return def
	}
	return chosen
}

// storedSequence reads [{type, index}] as the console writes it. "type" is the
// wizard's own word for it, kept so the stored value reads the same on both
// sides; "muat" and "bongkar" are what a planner sees on screen.
func storedSequence(detail models.JSONB) []stopRef {
	raw, ok := detail["stopSequence"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]stopRef, 0, len(raw))
	for _, v := range raw {
		m, ok := v.(map[string]interface{})
		if !ok {
			return nil
		}
		kind := ""
		switch m["type"] {
		case "muat", models.StopLoad:
			kind = models.StopLoad
		case "bongkar", models.StopUnload:
			kind = models.StopUnload
		default:
			return nil
		}
		idx, ok := m["index"].(float64) // JSON numbers arrive as float64
		if !ok {
			return nil
		}
		out = append(out, stopRef{Kind: kind, Index: int(idx)})
	}
	return out
}

// validateSequence holds the visit order to the one rule the journey cannot
// break: a shipment's goods must be picked up before they are dropped off.
//
// It also insists the sequence name every point exactly once, because a
// sequence that skips a point would route the truck past a delivery and a
// sequence that repeats one would ask for the same POD twice.
func validateSequence(seq []stopRef, loads, unloads int) error {
	if len(seq) != loads+unloads {
		return fmt.Errorf("%w: stopSequence must list all %d points, got %d",
			ErrValidation, loads+unloads, len(seq))
	}
	seenLoad := make(map[int]bool, loads)
	seenUnload := make(map[int]bool, unloads)
	for _, r := range seq {
		switch r.Kind {
		case models.StopLoad:
			if r.Index < 0 || r.Index >= loads || seenLoad[r.Index] {
				return fmt.Errorf("%w: stopSequence names loading point %d twice or not at all",
					ErrValidation, r.Index+1)
			}
			seenLoad[r.Index] = true
		case models.StopUnload:
			if r.Index < 0 || r.Index >= unloads || seenUnload[r.Index] {
				return fmt.Errorf("%w: stopSequence names unloading point %d twice or not at all",
					ErrValidation, r.Index+1)
			}
			seenUnload[r.Index] = true
			// Index k of each list is shipment k+1, so the load that must
			// come first is the one with the same index.
			if r.Index < loads && !seenLoad[r.Index] {
				return fmt.Errorf("%w: Bongkar %d cannot come before Muat %d — "+
					"a shipment must be loaded before it is unloaded",
					ErrValidation, r.Index+1, r.Index+1)
			}
		}
	}
	return nil
}

// ValidateStopSequence is the same rule at the edge, so a planner who reorders
// the points is told which move is impossible instead of having the choice
// quietly ignored.
func ValidateStopSequence(order *models.Order) error {
	seq := storedSequence(order.Detail)
	if len(seq) == 0 {
		return nil
	}
	return validateSequence(seq,
		len(warehouseList(order.Detail, "loadingPoints")),
		len(warehouseList(order.Detail, "unloadingPoints")))
}
