package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/fieldconfig"
	"github.com/karlo/business-service/internal/models"
	notificationv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/notification/v1"
	"github.com/karlo/business-service/internal/platform/query"
	"github.com/karlo/business-service/internal/repository"
	"github.com/shopspring/decimal"
)

// BillingService owns agreements and invoices.
//
// Note the boundary: this service produces the freight bill. The separate
// accounting service owns the ledger and payment reconciliation, and reads
// these invoices over gRPC.
type BillingService struct {
	agreements *repository.AgreementRepository
	invoices   *repository.InvoiceRepository
	orders     *repository.OrderRepository
	auth       *clients.Auth
	notifier   clients.Notifier

	// dispatch resolves warehouse coordinates and holds the route cache, so an
	// agreement's lanes can be measured. Attached after construction for the
	// same reason as on OrderService: both need the order repository this
	// service also owns, and a constructor argument would be a cycle.
	dispatch *DispatchService

	// fields enforces the company's form configuration. Attached after
	// construction rather than injected, the same way OrderService takes
	// dispatch: nil-checked at use, so a deployment that has not wired it
	// still creates agreements — without the per-company check.
	fields *FieldConfigService
}

// WithFieldConfig attaches the per-company field check.
func (s *BillingService) WithFieldConfig(f *FieldConfigService) { s.fields = f }

// WithDispatch attaches route planning, so agreement lanes can be measured.
func (s *BillingService) WithDispatch(d *DispatchService) { s.dispatch = d }

func NewBillingService(
	agreements *repository.AgreementRepository,
	invoices *repository.InvoiceRepository,
	orders *repository.OrderRepository,
	auth *clients.Auth,
	notifier clients.Notifier,
) *BillingService {
	return &BillingService{
		agreements: agreements,
		invoices:   invoices,
		orders:     orders,
		auth:       auth,
		notifier:   notifier,
	}
}

// ---------------------------------------------------------------------------
// Agreements
// ---------------------------------------------------------------------------

// CreateAgreementInput describes a new contract.
type CreateAgreementInput struct {
	TransporterCompanyID uuid.UUID
	// CustomerCompanyID is set when the TRANSPORTER records the agreement —
	// the revamp's MyAgreement flow, where a transporter's planner enters
	// the contract struck with one of its customers. The actor is then the
	// transporter side and this is the shipper side; the agreement takes
	// effect immediately, since the party that would approve it is the one
	// entering it.
	CustomerCompanyID *uuid.UUID
	ValidFrom         time.Time
	ValidUntil        time.Time
	PaymentTypeID     string
	CurrencyID        string
	Detail            map[string]interface{}
	Rates             []RateInput

	// Customers are the clients this agreement covers.
	//
	// Empty means it covers only the counterparty it was struck with, which is
	// the ordinary single-client agreement. A framework contract names several.
	Customers []CustomerInput

	// Present names the payload keys the caller actually supplied, for the
	// per-company field check. Built by the handler from the raw JSON rather
	// than inferred here: "supplied" and "non-zero" are different questions,
	// and a struct cannot tell an omitted price from a price of nought.
	Present map[string]bool
}

// CustomerInput is one client an agreement covers.
type CustomerInput struct {
	CompanyID uuid.UUID
	// Label is what this client is called on this contract, when it differs
	// from the company's own name.
	Label string
}

// RateInput is one price line.
type RateInput struct {
	// CustomerCompanyID narrows this lane to one client. Zero means every
	// customer the agreement covers, which is the ordinary case.
	CustomerCompanyID *uuid.UUID

	// Warehouse ids make this a warehouse-level lane — the only kind that can
	// be routed, because a city pair has no coordinates to measure between.
	OriginWarehouseID      string
	DestinationWarehouseID string

	OriginCityID      string
	DestinationCityID string

	// Kecamatan. Empty for the companies that price city to city, which is
	// most of them.
	OriginDistrictID      string
	DestinationDistrictID string

	TruckTypeID   string
	PricingTypeID string
	Price         decimal.Decimal
	MinQuantity   *decimal.Decimal
	LeadTimeHours *int
}

// CreateAgreement records a negotiated contract.
func (s *BillingService) CreateAgreement(ctx context.Context, actor Actor, in CreateAgreementInput) (*models.Agreement, error) {
	if in.ValidUntil.Before(in.ValidFrom) {
		return nil, fmt.Errorf("%w: validUntil cannot be before validFrom", ErrValidation)
	}
	shipperID, transporterID := actor.CompanyID, in.TransporterCompanyID
	status := models.AgreementSubmitted
	if in.CustomerCompanyID != nil {
		shipperID, transporterID = *in.CustomerCompanyID, actor.CompanyID
		status = models.AgreementActive
	}
	if transporterID == shipperID {
		return nil, fmt.Errorf("%w: a company cannot contract with itself", ErrValidation)
	}
	if len(in.Rates) == 0 {
		return nil, fmt.Errorf("%w: an agreement needs at least one rate", ErrValidation)
	}

	// The company's own form configuration. Checked HERE rather than only in
	// the browser, because the form is not the only caller: the API is public
	// to anyone holding a token, and a rule enforced only by the page that
	// renders it is not a rule.
	if s.fields != nil {
		if err := s.fields.Validate(ctx, actor.CompanyID,
			fieldconfig.EntityAgreement, in.Present); err != nil {
			return nil, err
		}
	}
	for i, r := range in.Rates {
		if r.Price.IsNegative() {
			return nil, fmt.Errorf("%w: rate %d has a negative price", ErrValidation, i)
		}
	}

	number, err := s.agreementNumber(ctx, transporterID, shipperID)
	if err != nil {
		return nil, err
	}

	agreement := &models.Agreement{
		AgreementNumber:      number,
		ShipperCompanyID:     shipperID,
		TransporterCompanyID: transporterID,
		CreatedByUserID:      actor.UserID,
		StatusCode:           status,
		ValidFrom:            in.ValidFrom,
		ValidUntil:           in.ValidUntil,
		Detail:               models.JSONB(in.Detail),
	}
	setIfNotEmpty(&agreement.PaymentTypeID, in.PaymentTypeID)
	setIfNotEmpty(&agreement.CurrencyID, in.CurrencyID)
	if agreement.Detail == nil {
		agreement.Detail = models.JSONB{}
	}

	for _, r := range in.Rates {
		rate := models.AgreementRate{
			Price:         r.Price,
			MinQuantity:   r.MinQuantity,
			LeadTimeHours: r.LeadTimeHours,
		}
		setIfNotEmpty(&rate.OriginWarehouseID, r.OriginWarehouseID)
		setIfNotEmpty(&rate.DestinationWarehouseID, r.DestinationWarehouseID)
		setIfNotEmpty(&rate.OriginCityID, r.OriginCityID)
		setIfNotEmpty(&rate.DestinationCityID, r.DestinationCityID)
		setIfNotEmpty(&rate.OriginDistrictID, r.OriginDistrictID)
		setIfNotEmpty(&rate.DestinationDistrictID, r.DestinationDistrictID)
		setIfNotEmpty(&rate.TruckTypeID, r.TruckTypeID)
		setIfNotEmpty(&rate.PricingTypeID, r.PricingTypeID)
		setIfNotEmpty(&rate.CurrencyID, in.CurrencyID)
		rate.CustomerCompanyID = r.CustomerCompanyID
		agreement.Rates = append(agreement.Rates, rate)
	}

	// Who the agreement covers. The counterparty is always one of them —
	// omitting it would make the ordinary single-client agreement cover nobody,
	// and every lookup by customer would miss it.
	covered := map[uuid.UUID]string{shipperID: ""}
	for _, c := range in.Customers {
		if c.CompanyID != uuid.Nil {
			covered[c.CompanyID] = c.Label
		}
	}
	for id, label := range covered {
		entry := models.AgreementCustomer{CustomerCompanyID: id}
		if label != "" {
			entry.Label = &label
		}
		agreement.Customers = append(agreement.Customers, entry)
	}

	if err := s.agreements.Create(ctx, agreement); err != nil {
		return nil, err
	}

	// Measure the lanes. Done after the write and not inside it: a contract
	// with an unrouted lane is still a contract, and refusing one because MAPID
	// was briefly unwell would lose a sale to somebody else's outage.
	s.RouteLanes(ctx, agreement.ID)

	s.notifier.Notify(ctx, clients.Event{
		Type:    notificationv1.EventType_EVENT_TYPE_AGREEMENT_CREATED,
		Subject: clients.Subject{ID: agreement.ID.String(), Type: "agreement"},
		// Tell the other side: the transporter when a shipper submitted, the
		// customer when the transporter recorded it.
		Audience: func() *notificationv1.Audience {
			if in.CustomerCompanyID != nil {
				return clients.ToCompanyRoles(shipperID.String(), models.RoleShipper, models.RoleManager)
			}
			return clients.ToCompanyRoles(transporterID.String(), models.RoleTransporter, models.RoleManager)
		}(),
		ActorID:        actor.UserID.String(),
		IdempotencyKey: "agreement-created:" + agreement.ID.String(),
		Params:         map[string]interface{}{"agreementNumber": agreement.AgreementNumber},
	})

	return agreement, nil
}

// GetAgreement resolves a contract the caller is party to.
func (s *BillingService) GetAgreement(ctx context.Context, actor Actor, id uuid.UUID) (*models.Agreement, error) {
	return s.agreements.FindByID(ctx, actor.CompanyID, id)
}

func (s *BillingService) GetAgreementForService(ctx context.Context, id uuid.UUID) (*models.Agreement, error) {
	return s.agreements.FindByIDForService(ctx, id)
}

// ListAgreements pages the caller's contracts.
func (s *BillingService) ListAgreements(ctx context.Context, actor Actor, p query.Params) ([]models.Agreement, int64, error) {
	rows, total, err := s.agreements.List(ctx, actor.CompanyID, p)
	if err != nil {
		return nil, 0, err
	}
	s.nameCompaniesOnAgreements(ctx, rows)
	return rows, total, nil
}

// nameCompaniesOnAgreements fills in the two company names an agreement holds
// only as ids, so a list does not render a column of UUIDs.
func (s *BillingService) nameCompaniesOnAgreements(ctx context.Context, rows []models.Agreement) {
	if s.auth == nil || len(rows) == 0 {
		return
	}
	ids := make([]string, 0, len(rows)*2)
	for i := range rows {
		ids = append(ids, rows[i].ShipperCompanyID.String(), rows[i].TransporterCompanyID.String())
	}
	names := s.auth.CompanyNames(ctx, ids)
	for i := range rows {
		rows[i].ShipperCompanyName = names[rows[i].ShipperCompanyID.String()]
		rows[i].TransporterCompanyName = names[rows[i].TransporterCompanyID.String()]
	}
}

// DecideAgreement approves or rejects a submitted contract.
func (s *BillingService) DecideAgreement(ctx context.Context, actor Actor, id uuid.UUID, approve bool) error {
	agreement, err := s.agreements.FindByID(ctx, actor.CompanyID, id)
	if err != nil {
		return err
	}

	// Only the receiving side decides.
	if agreement.TransporterCompanyID != actor.CompanyID &&
		actor.Role != models.RoleAdmin && actor.Role != models.RoleSuperadmin {
		return fmt.Errorf("%w: only the transporter may decide this agreement", ErrForbidden)
	}
	if agreement.StatusCode != models.AgreementSubmitted {
		return fmt.Errorf("%w: agreement is %s, not awaiting a decision", ErrTransition, agreement.StatusCode)
	}

	status := models.AgreementRejected
	event := notificationv1.EventType_EVENT_TYPE_AGREEMENT_REJECTED
	if approve {
		status = models.AgreementActive
		event = notificationv1.EventType_EVENT_TYPE_AGREEMENT_APPROVED
	}

	if err := s.agreements.UpdateFields(ctx, id, map[string]interface{}{"status_code": status}); err != nil {
		return err
	}

	s.notifier.Notify(ctx, clients.Event{
		Type:           event,
		Subject:        clients.Subject{ID: id.String(), Type: "agreement"},
		Audience:       clients.ToCompanyRoles(agreement.ShipperCompanyID.String(), models.RoleShipper),
		ActorID:        actor.UserID.String(),
		IdempotencyKey: fmt.Sprintf("agreement:%s:%s", id, status),
		Params:         map[string]interface{}{"agreementNumber": agreement.AgreementNumber},
	})

	return nil
}

// VerifyAgreement records the shipper's explicit sign-off, which companies with
// strict verification require before ordering against it.
func (s *BillingService) VerifyAgreement(ctx context.Context, actor Actor, id uuid.UUID) error {
	agreement, err := s.agreements.FindByID(ctx, actor.CompanyID, id)
	if err != nil {
		return err
	}
	if agreement.ShipperCompanyID != actor.CompanyID {
		return fmt.Errorf("%w: only the shipper may verify this agreement", ErrForbidden)
	}

	now := time.Now()
	return s.agreements.UpdateFields(ctx, id, map[string]interface{}{
		"verified":            true,
		"verified_by_user_id": actor.UserID,
		"verified_at":         now,
	})
}

// ExpireAgreements marks contracts past their validity as expired. Run nightly.
func (s *BillingService) ExpireAgreements(ctx context.Context) (int, error) {
	expired, err := s.agreements.FindExpired(ctx, time.Now())
	if err != nil {
		return 0, err
	}

	var count int
	for i := range expired {
		a := &expired[i]
		if err := s.agreements.UpdateFields(ctx, a.ID, map[string]interface{}{
			"status_code": models.AgreementExpired,
		}); err != nil {
			// One failure must not abandon the rest of the sweep.
			continue
		}
		count++

		s.notifier.Notify(ctx, clients.Event{
			Type:     notificationv1.EventType_EVENT_TYPE_AGREEMENT_EXPIRED,
			Subject:  clients.Subject{ID: a.ID.String(), Type: "agreement"},
			Audience: clients.ToCompanyRoles(a.ShipperCompanyID.String(), models.RoleShipper),
			// Dated, so the nightly run cannot re-notify for the same day.
			IdempotencyKey: fmt.Sprintf("agreement-expired:%s:%s", a.ID, time.Now().Format("2006-01-02")),
			Params:         map[string]interface{}{"agreementNumber": a.AgreementNumber},
		})
	}

	return count, nil
}

// ---------------------------------------------------------------------------
// Invoices
// ---------------------------------------------------------------------------

// CreateInvoiceInput describes a bill covering a set of completed orders.
type CreateInvoiceInput struct {
	ShipperCompanyID uuid.UUID
	OrderIDs         []uuid.UUID
	CurrencyID       string
	Adjustment       decimal.Decimal
	Notes            string
	DueAt            *time.Time
}

// CreateInvoice bills a set of orders.
//
// Three checks matter here, and the legacy system performed none of them: every
// order must be complete, every order must belong to the parties named, and no
// order may already be on a live invoice.
func (s *BillingService) CreateInvoice(ctx context.Context, actor Actor, in CreateInvoiceInput) (*models.Invoice, error) {
	if len(in.OrderIDs) == 0 {
		return nil, fmt.Errorf("%w: an invoice needs at least one order", ErrValidation)
	}

	orders, err := s.orders.FindByIDs(ctx, in.OrderIDs)
	if err != nil {
		return nil, err
	}
	if len(orders) != len(in.OrderIDs) {
		return nil, fmt.Errorf("%w: some orders could not be found", ErrValidation)
	}

	for i := range orders {
		o := &orders[i]
		if o.StatusCode != models.OrderCompleted && o.StatusCode != models.OrderDelivered {
			return nil, fmt.Errorf("%w: order %s is %s and cannot be billed yet",
				ErrValidation, o.OrderNumber, o.StatusCode)
		}
		if o.TransporterCompanyID == nil || *o.TransporterCompanyID != actor.CompanyID {
			return nil, fmt.Errorf("%w: order %s was not carried by your company",
				ErrForbidden, o.OrderNumber)
		}
		if o.ShipperCompanyID != in.ShipperCompanyID {
			return nil, fmt.Errorf("%w: order %s belongs to a different shipper",
				ErrValidation, o.OrderNumber)
		}
	}

	billed, err := s.invoices.OrdersAlreadyBilled(ctx, in.OrderIDs)
	if err != nil {
		return nil, err
	}
	if len(billed) > 0 {
		return nil, fmt.Errorf("%w: %d of these orders are already on an invoice", ErrValidation, len(billed))
	}

	// Tax rates come from the billing company's settings, so a rate change
	// applies to new invoices without touching issued ones.
	settings, err := s.auth.CompanySettings(ctx, actor.CompanyID.String())
	if err != nil {
		return nil, fmt.Errorf("resolve company settings: %w", err)
	}

	number, err := s.nextNumber(ctx, "invoice", "INV")
	if err != nil {
		return nil, err
	}

	invoice := &models.Invoice{
		InvoiceNumber:        number,
		ShipperCompanyID:     in.ShipperCompanyID,
		TransporterCompanyID: actor.CompanyID,
		StatusCode:           models.InvoiceDraft,
		PPNPercentage:        decimal.NewFromFloat(settings.GetPpnPercentage()),
		PPH23Percentage:      decimal.NewFromFloat(settings.GetPph23Percentage()),
		Adjustment:           in.Adjustment,
		Subtotal:             decimal.Zero,
		DueAt:                in.DueAt,
		Detail:               models.JSONB{},
	}
	setIfNotEmpty(&invoice.CurrencyID, in.CurrencyID)
	if in.Notes != "" {
		invoice.Notes = &in.Notes
	}

	for i := range orders {
		o := &orders[i]
		amount := decimal.Zero
		if o.Price != nil {
			amount = *o.Price
		}

		desc := "Order " + o.OrderNumber
		invoice.Lines = append(invoice.Lines, models.InvoiceLine{
			OrderID:     o.ID,
			Description: &desc,
			Quantity:    decimal.NewFromInt(1),
			UnitPrice:   amount,
			Amount:      amount,
		})
		invoice.Subtotal = invoice.Subtotal.Add(amount)
	}

	invoice.Recalculate()

	if err := s.invoices.Create(ctx, invoice); err != nil {
		return nil, err
	}

	return invoice, nil
}

// GetInvoice resolves a bill the caller is party to.
func (s *BillingService) GetInvoice(ctx context.Context, actor Actor, id uuid.UUID) (*models.Invoice, error) {
	return s.invoices.FindByID(ctx, actor.CompanyID, id)
}

func (s *BillingService) GetInvoiceForService(ctx context.Context, id uuid.UUID) (*models.Invoice, error) {
	return s.invoices.FindByIDForService(ctx, id)
}

// ListInvoices pages the caller's bills.
func (s *BillingService) ListInvoices(ctx context.Context, actor Actor, p query.Params) ([]models.Invoice, int64, error) {
	rows, total, err := s.invoices.List(ctx, actor.CompanyID, p)
	if err != nil {
		return nil, 0, err
	}
	s.nameCompaniesOnInvoices(ctx, rows)
	return rows, total, nil
}

// nameCompaniesOnInvoices does for invoices what nameCompaniesOnAgreements does
// for agreements. A bill naming only ids is unreadable, and on an invoice the
// two sides matter more than anywhere else: it says who owes whom.
func (s *BillingService) nameCompaniesOnInvoices(ctx context.Context, rows []models.Invoice) {
	if s.auth == nil || len(rows) == 0 {
		return
	}
	ids := make([]string, 0, len(rows)*2)
	for i := range rows {
		ids = append(ids, rows[i].ShipperCompanyID.String(), rows[i].TransporterCompanyID.String())
	}
	names := s.auth.CompanyNames(ctx, ids)
	for i := range rows {
		rows[i].ShipperCompanyName = names[rows[i].ShipperCompanyID.String()]
		rows[i].TransporterCompanyName = names[rows[i].TransporterCompanyID.String()]
	}
}

// invoiceTransitions is the invoice state machine. It lives here rather than in
// models because, unlike orders, the rule is about which side of the bill the
// caller is on: the transporter issues and the shipper pays.
var invoiceTransitions = map[string][]string{
	models.InvoiceDraft:     {models.InvoiceIssued, models.InvoiceCancelled},
	models.InvoiceIssued:    {models.InvoiceSubmitted, models.InvoiceCancelled},
	models.InvoiceSubmitted: {models.InvoiceVerified, models.InvoiceIssued, models.InvoiceCancelled},
	models.InvoiceVerified:  {models.InvoicePaid, models.InvoiceCancelled},
	models.InvoicePaid:      {},
	models.InvoiceCancelled: {},
}

// TransitionInvoice advances a bill.
func (s *BillingService) TransitionInvoice(ctx context.Context, actor Actor, id uuid.UUID, to string) error {
	invoice, err := s.invoices.FindByID(ctx, actor.CompanyID, id)
	if err != nil {
		return err
	}

	allowed, known := invoiceTransitions[invoice.StatusCode]
	if !known {
		return fmt.Errorf("%w: unknown invoice status %q", ErrTransition, invoice.StatusCode)
	}
	var ok bool
	for _, candidate := range allowed {
		if candidate == to {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("%w: an invoice cannot move from %s to %s", ErrTransition, invoice.StatusCode, to)
	}

	isTransporter := invoice.TransporterCompanyID == actor.CompanyID
	isShipper := invoice.ShipperCompanyID == actor.CompanyID
	isPlatformAdmin := actor.Role == models.RoleAdmin || actor.Role == models.RoleSuperadmin

	switch to {
	case models.InvoiceIssued, models.InvoiceSubmitted:
		if !isTransporter && !isPlatformAdmin {
			return fmt.Errorf("%w: only the transporter may issue or submit this invoice", ErrForbidden)
		}
	case models.InvoiceVerified, models.InvoicePaid:
		// The party being billed confirms and pays. Letting the biller mark
		// their own invoice paid is how receivables quietly stop being real.
		if !isShipper && !isPlatformAdmin {
			return fmt.Errorf("%w: only the shipper may verify or settle this invoice", ErrForbidden)
		}
	}

	fields := map[string]interface{}{}
	now := time.Now()
	switch to {
	case models.InvoiceIssued:
		fields["issued_at"] = now
	case models.InvoiceSubmitted:
		fields["submitted_at"] = now
	case models.InvoiceVerified:
		fields["verified_at"] = now
	case models.InvoicePaid:
		fields["paid_at"] = now
	case models.InvoiceCancelled:
		fields["cancelled_at"] = now
	}

	if err := s.invoices.ApplyStatus(ctx, id, invoice.StatusCode, to, fields); err != nil {
		return err
	}

	if event, notify := invoiceEventFor(to); notify {
		audience := clients.ToCompanyRoles(invoice.ShipperCompanyID.String(), models.RoleShipper)
		if to == models.InvoicePaid {
			audience = clients.ToCompanyRoles(invoice.TransporterCompanyID.String(), models.RoleTransporter, models.RoleManager)
		}
		s.notifier.Notify(ctx, clients.Event{
			Type:           event,
			Subject:        clients.Subject{ID: id.String(), Type: "invoice"},
			Audience:       audience,
			ActorID:        actor.UserID.String(),
			IdempotencyKey: fmt.Sprintf("invoice:%s:%s", id, to),
			Params: map[string]interface{}{
				"invoiceNumber": invoice.InvoiceNumber,
				"total":         invoice.Total.String(),
			},
		})
	}

	return nil
}

func invoiceEventFor(status string) (notificationv1.EventType, bool) {
	switch status {
	case models.InvoiceIssued:
		return notificationv1.EventType_EVENT_TYPE_INVOICE_ISSUED, true
	case models.InvoicePaid:
		return notificationv1.EventType_EVENT_TYPE_INVOICE_PAID, true
	default:
		return notificationv1.EventType_EVENT_TYPE_UNSPECIFIED, false
	}
}

// nextNumber builds a per-year sequential document number.
func (s *BillingService) nextNumber(ctx context.Context, scope, prefix string) (string, error) {
	period := time.Now().Format("2006")
	seq, err := s.orders.NextNumber(ctx, scope, period)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%06d", prefix, period, seq), nil
}

// agreementNumber builds AGR-<transporter>-<client>-000001.
//
// The two codes are what make the number worth reading: a counterparty can say
// "AGR-SKI-SMS-000001" over the phone and both sides know which contract and
// with whom. The previous format, AGR-2026-000001, was unique and said nothing.
//
// The sequence is what guarantees uniqueness, NOT the codes — two companies may
// share three letters, and refusing a registration over that would be a
// cosmetic rule with a commercial cost. So the codes are decoration on a unique
// number rather than part of the key.
//
// A company whose code cannot be resolved falls back to the year, which keeps
// the old shape rather than producing AGR--SMS-000001. Numbering must not fail
// because the authentication service was briefly unreachable.
func (s *BillingService) agreementNumber(ctx context.Context, transporterID, clientID uuid.UUID) (string, error) {
	seq, err := s.orders.NextNumber(ctx, "agreement", time.Now().Format("2006"))
	if err != nil {
		return "", err
	}

	profiles := s.auth.CompanyProfiles(ctx, []string{transporterID.String(), clientID.String()})

	code := func(id uuid.UUID) string {
		if p, ok := profiles[id.String()]; ok && p.Abbreviation != "" {
			return p.Abbreviation
		}
		return ""
	}

	transporter, client := code(transporterID), code(clientID)
	if transporter == "" || client == "" {
		slog.WarnContext(ctx, "agreement numbered without company codes",
			"transporterId", transporterID, "clientId", clientID)
		return fmt.Sprintf("AGR-%s-%06d", time.Now().Format("2006"), seq), nil
	}

	return fmt.Sprintf("AGR-%s-%s-%06d", transporter, client, seq), nil
}

var _ = errors.Is
