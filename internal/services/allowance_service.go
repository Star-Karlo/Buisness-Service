package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/repository"
)

// TollRatePerKm is what a tolled kilometre is estimated at, in rupiah.
//
// An estimate, and labelled as one everywhere it surfaces. Indonesian toll
// tariffs vary by road, by vehicle class and by gate, so a single rate cannot
// be right; what it can be is close enough to sanity-check a figure somebody is
// about to hand a driver in cash. The alternative — showing nothing — means the
// number is typed against no reference at all, which is what happens today.
//
// It is a constant rather than configuration on purpose. Making it tunable
// invites it to be treated as authoritative, and the honest fix when this is
// not good enough is a real tariff table, not a better guess.
const TollRatePerKm = 1350

// AllowanceService owns uang sangu.
//
// There is no calculator here, deliberately. The advance is entered by hand —
// by sales or by finance, whichever the company decided, which is a permission
// question rather than a code question. What this service contributes is the
// evidence: both distances and a toll estimate, so the figure is typed against
// something rather than guessed.
type AllowanceService struct {
	orders     *repository.OrderRepository
	allowances *repository.AllowanceRepository
	routes     *repository.OrderRouteRepository
}

func NewAllowanceService(
	orders *repository.OrderRepository,
	allowances *repository.AllowanceRepository,
	routes *repository.OrderRouteRepository,
) *AllowanceService {
	return &AllowanceService{orders: orders, allowances: allowances, routes: routes}
}

// Evidence is what the system knows about the journey, for someone deciding the
// advance.
type Evidence struct {
	// HaulDistanceMeters is warehouse to warehouse — the revenue journey.
	HaulDistanceMeters *int `json:"haulDistanceMeters,omitempty"`

	// ApproachDistanceMeters is the truck's run to the loading point. Separate
	// because it is unpaid repositioning and is the part a driver is most
	// likely to be short-changed on.
	ApproachDistanceMeters *int `json:"approachDistanceMeters,omitempty"`

	TotalDistanceMeters int `json:"totalDistanceMeters"`

	// TollDistanceMeters is how much of the journey runs on roads known to be
	// tolled. Stretches with no toll data are excluded rather than assumed
	// free, so the estimate can be low but is never invented.
	TollDistanceMeters int `json:"tollDistanceMeters"`

	TollEstimate decimal.Decimal `json:"tollEstimate"`

	// TollDataComplete is false when part of the route had no toll data.
	// Surfaced so a low estimate is visibly a floor rather than a figure.
	TollDataComplete bool `json:"tollDataComplete"`
}

// View is the allowance plus the evidence behind it.
type View struct {
	Allowance *models.OrderAllowance `json:"allowance,omitempty"`
	Evidence  Evidence               `json:"evidence"`

	// Editable is false once the advance is finalised. Returned rather than
	// inferred client-side so the button state and the server's answer cannot
	// disagree.
	Editable bool `json:"editable"`
}

// Get returns the current advance and the evidence for it.
func (s *AllowanceService) Get(ctx context.Context, actor Actor, orderID uuid.UUID) (*View, error) {
	if _, err := s.orders.FindByID(ctx, actor.CompanyID, orderID); err != nil {
		return nil, err
	}

	evidence, err := s.evidence(ctx, orderID)
	if err != nil {
		return nil, err
	}

	view := &View{Evidence: evidence, Editable: true}

	allowance, err := s.allowances.FindByOrder(ctx, orderID)
	switch {
	case err == nil:
		view.Allowance = allowance
		view.Editable = allowance.FinalisedAt == nil
	case errors.Is(err, repository.ErrNotFound):
		// No advance set yet. Not an error: the screen shows the evidence and
		// an empty form.
	default:
		return nil, err
	}

	return view, nil
}

// evidence reads the planned legs and works out what they imply.
func (s *AllowanceService) evidence(ctx context.Context, orderID uuid.UUID) (Evidence, error) {
	legs, err := s.routes.ListByOrder(ctx, orderID)
	if err != nil {
		return Evidence{}, err
	}

	ev := Evidence{TollDataComplete: true}

	for _, leg := range legs {
		if leg.Cache == nil {
			continue
		}
		distance := leg.Cache.DistanceMeters

		switch leg.Leg {
		case models.LegHaul:
			d := distance
			ev.HaulDistanceMeters = &d
		case models.LegApproach:
			d := distance
			ev.ApproachDistanceMeters = &d
		}

		ev.TotalDistanceMeters += distance
		ev.TollDistanceMeters += leg.Cache.TollDistanceMeters

		// A route that crosses a toll road but reports no tolled distance had
		// gaps in the data. Saying so is the difference between "no tolls on
		// this route" and "we could not tell".
		if leg.Cache.HasToll && leg.Cache.TollDistanceMeters == 0 {
			ev.TollDataComplete = false
		}
	}

	ev.TollEstimate = decimal.NewFromInt(int64(ev.TollDistanceMeters)).
		Div(decimal.NewFromInt(1000)).
		Mul(decimal.NewFromInt(TollRatePerKm)).
		Round(0)

	return ev, nil
}

// SaveAllowanceInput is a submitted advance.
type SaveAllowanceInput struct {
	Components []models.AllowanceComponent
	CurrencyID string
	Note       string
	// Reason is required when revising an advance that is already finalised.
	Reason string
}

// Save records the advance.
//
// The total is computed from the components rather than accepted from the
// caller. A client-supplied total that disagreed with its own lines would be
// unarguable later, and the disagreement would only be found by someone adding
// them up by hand.
//
// The distances are snapshotted here, not read at display time. A reroute after
// this point changes the live distance, and it must not silently restate what
// somebody was paid against.
func (s *AllowanceService) Save(ctx context.Context, actor Actor, orderID uuid.UUID, in SaveAllowanceInput) (*View, error) {
	if _, err := s.orders.FindByID(ctx, actor.CompanyID, orderID); err != nil {
		return nil, err
	}

	total := decimal.Zero
	components := make(models.JSONArray, 0, len(in.Components))
	for _, c := range in.Components {
		if c.Code == "" {
			return nil, fmt.Errorf("%w: every allowance line needs a code", ErrValidation)
		}
		if c.Amount.IsNegative() {
			return nil, fmt.Errorf("%w: %s is negative", ErrValidation, c.Label)
		}
		total = total.Add(c.Amount)
		components = append(components, map[string]interface{}{
			"code": c.Code, "label": c.Label,
			"amount": c.Amount.String(), "note": c.Note,
		})
	}

	evidence, err := s.evidence(ctx, orderID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	row := &models.OrderAllowance{
		OrderID:                orderID,
		HaulDistanceMeters:     evidence.HaulDistanceMeters,
		ApproachDistanceMeters: evidence.ApproachDistanceMeters,
		TollEstimate:           &evidence.TollEstimate,
		Components:             components,
		Total:                  total,
		EnteredByUserID:        &actor.UserID,
		EnteredAt:              &now,
	}
	if in.CurrencyID != "" {
		row.CurrencyID = &in.CurrencyID
	}
	if in.Note != "" {
		row.Note = &in.Note
	}

	if err := s.allowances.Save(ctx, row, in.Reason, actor.UserID); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, fmt.Errorf("%w: this advance is finalised; a change needs a reason", ErrValidation)
		}
		return nil, err
	}

	return s.Get(ctx, actor, orderID)
}

// Finalise commits the advance to the driver.
func (s *AllowanceService) Finalise(ctx context.Context, actor Actor, orderID uuid.UUID) (*View, error) {
	if _, err := s.orders.FindByID(ctx, actor.CompanyID, orderID); err != nil {
		return nil, err
	}
	if err := s.allowances.Finalise(ctx, orderID, actor.UserID); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, fmt.Errorf("%w: there is no open advance to finalise", ErrValidation)
		}
		return nil, err
	}
	return s.Get(ctx, actor, orderID)
}

// History returns every superseded version.
func (s *AllowanceService) History(ctx context.Context, actor Actor, orderID uuid.UUID) ([]models.OrderAllowanceRevision, error) {
	if _, err := s.orders.FindByID(ctx, actor.CompanyID, orderID); err != nil {
		return nil, err
	}
	return s.allowances.History(ctx, orderID)
}
