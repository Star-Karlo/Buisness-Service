package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/models"
	notificationv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/notification/v1"
	"github.com/karlo/business-service/internal/repository"
	"github.com/karlo/business-service/internal/routing"
)

// How long a handover code lives, and how many digits it has.
//
// Ten minutes is the working window: the driver types the PIC's number, the PIC
// finds their phone, reads it out, the driver types it back. Long enough for
// that to happen at a loading bay in the rain, short enough that a code
// overheard earlier in the day is useless.
const (
	handoverCodeTTL    = 10 * time.Minute
	handoverCodeDigits = 6
)

// HandoverService issues the unloading code and checks both uses of it.
//
// The code goes to the DRIVER. The driver enters it in K-Trip to begin
// unloading, and reads it out to the receiving PIC, who enters the same code
// on Web-Field to open the audit. What that demonstrates is that the two
// people were standing together at the gate — which is the thing a photograph
// cannot show, and which a code mailed only to the warehouse never showed
// either. See migrations/000013_webfield.up.sql.
type HandoverService struct {
	shipments *repository.ShipmentRepository
	orders    *repository.OrderRepository
	handovers *repository.HandoverRepository
	dispatch  *DispatchService
	notifier  clients.Notifier
	// lifecycle takes the "start unloading" step once the code is confirmed
	// and records the field cargo check; pods shows the field page the
	// unloading POD's state. Both optional (tests).
	lifecycle *ShipmentService
	pods      *repository.PodRepository
	// consoleBaseURL builds the Web-Field link the PIC gets on WhatsApp.
	consoleBaseURL string
}

// WithDriverFlow wires the shipment lifecycle and POD register in.
func (s *HandoverService) WithDriverFlow(lifecycle *ShipmentService, pods *repository.PodRepository, consoleBaseURL string) *HandoverService {
	s.lifecycle, s.pods, s.consoleBaseURL = lifecycle, pods, strings.TrimRight(consoleBaseURL, "/")
	return s
}

// FieldURL is the PIC's Web-Field page for a handover.
//
// A deep link into the same page the PIC reaches by typing the order number,
// carrying the session token so a PIC who was sent the link does not type it.
// The code is still required before the token does anything.
func (s *HandoverService) FieldURL(token string) string {
	if s.consoleBaseURL == "" {
		return ""
	}
	return s.consoleBaseURL + "/webfield?token=" + token
}

// FieldLink is the dedicated link the receiving PIC is sent.
//
// It carries the order number and the code, so tapping it lands on the order
// sheet with nothing to type. That is a deliberate trade: the code in the link
// is no longer evidence that the PIC stood next to the driver, and what still
// is, is the driver having entered the same code in K-Trip first — the page
// refuses to open the audit until they have.
func (s *HandoverService) FieldLink(orderNumber, code string) string {
	if s.consoleBaseURL == "" {
		return ""
	}
	return s.consoleBaseURL + "/webfield?order=" + url.QueryEscape(orderNumber) + "&code=" + url.QueryEscape(code)
}

// picForUnloading works out who to tell, without asking the driver.
//
// The planner already named a PIC for each point when the order was placed,
// so the driver's screen has no business asking again at the gate. The order's
// own choice wins; the site's default PIC is the fallback for orders placed
// before the field existed.
func (s *HandoverService) picForUnloading(ctx context.Context, order *models.Order) (name, phone string) {
	if order == nil {
		return "", ""
	}
	if pic, ok := order.Detail["unloadingPic"].(map[string]interface{}); ok {
		name, _ = pic["name"].(string)
		phone, _ = pic["phone"].(string)
	}
	if strings.TrimSpace(phone) == "" && order.DestinationWarehouseID != nil {
		if wh, err := s.dispatch.Warehouse(ctx, *order.DestinationWarehouseID); err == nil {
			if strings.TrimSpace(name) == "" {
				name = wh.GetPicName()
			}
			phone = wh.GetPicPhone()
		}
	}
	return strings.TrimSpace(name), strings.TrimSpace(phone)
}

func NewHandoverService(
	shipments *repository.ShipmentRepository,
	orders *repository.OrderRepository,
	handovers *repository.HandoverRepository,
	dispatch *DispatchService,
	notifier clients.Notifier,
) *HandoverService {
	return &HandoverService{
		shipments: shipments, orders: orders, handovers: handovers,
		dispatch: dispatch, notifier: notifier,
	}
}

// IssueResult is a freshly issued code.
//
// The code IS returned here, to the driver who asked for it, because the
// driver is now its recipient: they show it to the PIC. What is never
// returned is a code to anyone else — the field endpoints hand back a token,
// never digits.
type IssueResult struct {
	Handover *models.ShipmentHandover
	Code     string
	// PICLink is the same link the PIC was sent: order number and code in the
	// query, so tapping it opens the audit with nothing to type. On the
	// driver's screen it is what they forward when the message did not
	// arrive.
	PICLink string
}

// Issue creates the unloading code and delivers it to the driver.
func (s *HandoverService) Issue(ctx context.Context, actor Actor, shipmentID uuid.UUID, picName, picWhatsApp string) (*IssueResult, error) {
	shipment, err := s.shipments.FindByID(ctx, shipmentID)
	if err != nil {
		return nil, err
	}
	if shipment.DriverUserID == nil || *shipment.DriverUserID != actor.UserID {
		return nil, fmt.Errorf("%w: only the assigned driver may request a handover code", ErrForbidden)
	}

	// Who receives the link. The driver's app no longer asks: the planner
	// named a PIC for the unloading point when the order was placed, and
	// asking again at the gate only invites a wrong number. An explicit
	// argument still wins, for the case where the named PIC is not the one
	// standing there.
	order, _ := s.orders.FindByIDForService(ctx, shipment.OrderID)
	if strings.TrimSpace(picName) == "" && strings.TrimSpace(picWhatsApp) == "" {
		picName, picWhatsApp = s.picForUnloading(ctx, order)
	}

	// An unreachable PIC is not an error: they can still walk up to the
	// driver and read the code off their screen.
	number := ""
	if strings.TrimSpace(picWhatsApp) != "" {
		if number, err = normaliseWhatsApp(picWhatsApp); err != nil {
			return nil, err
		}
	}

	code, err := generateCode()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(code))

	// The field token opens the PIC's cargo-check page. Issued with the
	// code so the PIC has both from one message; 32 random bytes, like the
	// tracking link.
	rawToken := make([]byte, 32)
	if _, err := rand.Read(rawToken); err != nil {
		return nil, fmt.Errorf("handover: field token: %w", err)
	}
	fieldToken := base64.RawURLEncoding.EncodeToString(rawToken)

	row := &models.ShipmentHandover{
		ShipmentID:    shipmentID,
		Stage:         "unloading",
		PICWhatsApp:   number,
		CodeHash:      sum[:],
		CodeRecipient: "driver",
		MaxAttempts:   5,
		SentAt:        time.Now(),
		ExpiresAt:     time.Now().Add(handoverCodeTTL),
		FieldToken:    &fieldToken,
	}
	if picName != "" {
		row.PICName = &picName
	}

	if err := s.handovers.Issue(ctx, row); err != nil {
		return nil, err
	}

	// Sent after the row is committed, not before. A code that was delivered
	// but not stored can never be verified, which strands the driver at the
	// gate; a code stored but not delivered is recoverable by asking again.
	orderNumber := ""
	if order != nil {
		orderNumber = order.OrderNumber
	}
	fieldURL := s.FieldURL(fieldToken)
	// What the PIC is sent: the same page, reached with nothing to type.
	picLink := s.FieldLink(orderNumber, code)

	// To the driver, addressed as a user: that reaches the phone through push
	// and the app's own inbox as well as WhatsApp, so a driver whose WhatsApp
	// is on another handset still has the code in front of them.
	s.notifier.Notify(ctx, clients.Event{
		Type:     notificationv1.EventType_EVENT_TYPE_HANDOVER_CODE,
		Subject:  clients.Subject{ID: shipmentID.String(), Type: "shipment"},
		Audience: clients.ToUsers(shipment.DriverUserID.String()),
		Params: map[string]interface{}{
			"orderNumber": orderNumber,
			"code":        code,
			"minutes":     int(handoverCodeTTL.Minutes()),
			"fieldUrl":    fieldURL,
		},
		IdempotencyKey: "handover:" + row.ID.String(),
	})

	// To the warehouse, so they know a truck is in and can open the audit.
	// Silent while Web-Field is hidden: asking an external PIC to act on a
	// page the flow no longer waits for only invites them to be blamed when
	// nothing happens.
	if WebFieldEnabled {
		s.notifyWarehousePICs(ctx, shipment, order, picLink, number, row.ID.String())
	}

	return &IssueResult{Handover: row, Code: code, PICLink: picLink}, nil
}

// notifyWarehousePICs tells the receiving site that a truck is at the gate,
// and gives them the link that opens its audit.
//
// Twice over, because the two recipients are reached differently: the site's
// PICs who have console accounts get it in-app and as a push, and the number
// the order named gets it on WhatsApp, account or not. A warehouse clerk with
// no login is the common case, and they are the person at the gate.
func (s *HandoverService) notifyWarehousePICs(ctx context.Context, shipment *models.Shipment, order *models.Order, picLink, picNumber, handoverID string) {
	if order == nil || order.DestinationWarehouseID == nil {
		return
	}
	wh, err := s.dispatch.Warehouse(ctx, *order.DestinationWarehouseID)
	if err != nil {
		return
	}
	truck := ""
	if shipment.TruckID != nil {
		if t, err := s.dispatch.masterdata.GetTruck(ctx, *shipment.TruckID); err == nil {
			truck = t.GetPoliceNumber()
		}
	}
	params := map[string]interface{}{
		"truck":       truck,
		"warehouse":   wh.GetName(),
		"orderNumber": order.OrderNumber,
		"fieldUrl":    picLink,
	}
	send := func(audience *notificationv1.Audience, key string) {
		s.notifier.Notify(ctx, clients.Event{
			Type:           notificationv1.EventType_EVENT_TYPE_FIELD_VERIFICATION_PENDING,
			Subject:        clients.Subject{ID: order.ID.String(), Type: "order"},
			Audience:       audience,
			Params:         params,
			IdempotencyKey: key,
		})
	}
	if ids := wh.GetPicUserIds(); len(ids) > 0 {
		send(clients.ToUsers(ids...), "field-pending:"+handoverID)
	}
	if picNumber != "" {
		send(clients.ToPhone(picNumber), "field-pending-wa:"+handoverID)
	}
}

// VerifyInput is the driver's attempt.
type VerifyInput struct {
	Code string
	// Latitude and Longitude are where the driver was. Recorded alongside the
	// verification so the geofence check has a position to judge, and so a
	// dispute has the coordinate rather than only a timestamp.
	Latitude  *float64
	Longitude *float64
}

// Verify checks the code the PIC read out.
// HandoverTestCode is TEMPORARY: a code the driver app may enter instead
// of the one WhatsApped to the PIC, so the unloading flow can be walked
// through without a PIC on hand. It still needs a live, unexpired code to
// have been issued for the shipment, and the attempt is recorded like any
// other. Set it to "" (or delete it) before real deliveries run on this.
const HandoverTestCode = "000000"

func (s *HandoverService) Verify(ctx context.Context, actor Actor, shipmentID uuid.UUID, in VerifyInput) (*models.ShipmentHandover, error) {
	shipment, err := s.shipments.FindByID(ctx, shipmentID)
	if err != nil {
		return nil, err
	}
	if shipment.DriverUserID == nil || *shipment.DriverUserID != actor.UserID {
		return nil, fmt.Errorf("%w: only the assigned driver may complete a handover", ErrForbidden)
	}

	live, err := s.handovers.FindLive(ctx, shipmentID, "unloading")
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, fmt.Errorf("%w: no handover code has been requested", ErrValidation)
		}
		return nil, err
	}

	if time.Now().After(live.ExpiresAt) {
		return nil, fmt.Errorf("%w: that code has expired; ask for a new one", ErrValidation)
	}
	if live.Attempts >= live.MaxAttempts {
		return nil, fmt.Errorf("%w: too many attempts; ask for a new code", ErrValidation)
	}

	code := strings.TrimSpace(in.Code)
	sum := sha256.Sum256([]byte(code))
	// Constant-time, so the comparison does not leak how much of the code was
	// right through how long it took. Six digits is a small space and a timing
	// oracle would shrink it to a handful of guesses.
	if subtle.ConstantTimeCompare(sum[:], live.CodeHash) != 1 && code != HandoverTestCode {
		if err := s.handovers.RecordAttempt(ctx, live.ID); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: that code is not right", ErrValidation)
	}

	lat, lon, within := s.geofenceCheck(ctx, shipment, in)

	if err := s.handovers.MarkVerified(ctx, live.ID, lat, lon, within); err != nil {
		return nil, err
	}

	live.VerifiedAt = ptrTime(time.Now())
	live.VerifiedLat, live.VerifiedLon, live.VerifiedWithinGeofence = lat, lon, within

	// The code no longer starts the unloading by itself. The status sheet
	// puts "OTP bongkar terverifikasi" before "Mulai bongkar", so confirming
	// the code leaves the shipment at the gate and the driver's slide is what
	// begins the work — which is also the honest reading: the code proves
	// somebody is there to receive, not that the doors are open.
	//
	// Advance to unloading still requires this verification; see
	// ShipmentService.assertHandoverVerified.
	return live, nil
}

// FieldView is what the PIC's field page shows: enough to know which truck
// is at the gate and what it should be carrying, and where the check stands.
type FieldView struct {
	OrderNumber    string    `json:"orderNumber"`
	ShipmentID     uuid.UUID `json:"shipmentId"`
	ShipmentStatus string    `json:"shipmentStatus"`
	PICName        *string   `json:"picName,omitempty"`
	Truck          string    `json:"truck,omitempty"`
	Driver         string    `json:"driver,omitempty"`
	Destination    string    `json:"destination,omitempty"`

	// The two ends of the journey as the PIC's order sheet shows them, with
	// coordinates: the design puts them on the page because a PIC receiving
	// for several sites needs to see which gate this is.
	Origin           *FieldSite          `json:"origin,omitempty"`
	Drop             *FieldSite          `json:"drop,omitempty"`
	PickupAt         *time.Time          `json:"pickupAt,omitempty"`
	ArrivedAt        *time.Time          `json:"arrivedAt,omitempty"`
	Cargo            map[string]any      `json:"cargo,omitempty"`
	CargoCheck       *FieldCargoCheck    `json:"cargoCheck,omitempty"`
	Pod              *models.ShipmentPod `json:"pod,omitempty"`
	HandoverVerified bool                `json:"handoverVerified"`
	// SessionVerified says the code has been entered on this page, which is
	// what the audit endpoints require. Reported so the page can ask for the
	// code up front instead of drawing buttons that will be refused.
	SessionVerified bool `json:"sessionVerified"`

	// Expected is what the order says should arrive; Actual is what the PIC
	// counted. Kept apart so the audit shows a difference rather than
	// replacing one figure with the other.
	Expected *CargoFigures `json:"expected,omitempty"`
	Actual   *CargoFigures `json:"actual,omitempty"`

	FinalizedAt   *time.Time `json:"finalizedAt,omitempty"`
	FinalizedBy   string     `json:"finalizedBy,omitempty"`
	FinalizedNote string     `json:"finalizedNote,omitempty"`
}

// CargoFigures is one set of counts — ordered or received.
type CargoFigures struct {
	Name     string   `json:"name,omitempty"`
	WeightKg *float64 `json:"weightKg,omitempty"`
	VolumeM3 *float64 `json:"volumeM3,omitempty"`
	Quantity *float64 `json:"quantity,omitempty"`
}

func money(v *models.Money) *float64 {
	if v == nil {
		return nil
	}
	f, _ := v.Float64()
	return &f
}

func figures(f CargoFigures) *CargoFigures {
	if f.WeightKg == nil && f.VolumeM3 == nil && f.Quantity == nil && f.Name == "" {
		return nil
	}
	return &f
}

// FieldSite is one end of the journey.
type FieldSite struct {
	Name      string  `json:"name,omitempty"`
	Address   string  `json:"address,omitempty"`
	Latitude  float64 `json:"latitude,omitempty"`
	Longitude float64 `json:"longitude,omitempty"`
}

type FieldCargoCheck struct {
	Matches   bool      `json:"matches"`
	Note      string    `json:"note,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// Field resolves the PIC's page by token.
func (s *HandoverService) Field(ctx context.Context, token string) (*FieldView, error) {
	row, err := s.handovers.FindByFieldToken(ctx, token)
	if err != nil {
		return nil, err
	}
	shipment, err := s.shipments.FindByID(ctx, row.ShipmentID)
	if err != nil {
		return nil, err
	}
	order, err := s.orders.FindByIDForService(ctx, shipment.OrderID)
	if err != nil {
		return nil, err
	}
	view := &FieldView{
		OrderNumber:      order.OrderNumber,
		ShipmentID:       shipment.ID,
		ShipmentStatus:   shipment.StatusCode,
		PICName:          row.PICName,
		HandoverVerified: row.VerifiedAt != nil,
		SessionVerified:  row.FieldVerifiedAt != nil,
	}
	if shipment.TruckID != nil {
		if t, err := s.dispatch.masterdata.GetTruck(ctx, *shipment.TruckID); err == nil {
			view.Truck = t.GetPoliceNumber()
		}
	}
	if shipment.DriverID != nil {
		if d, err := s.dispatch.masterdata.GetDriver(ctx, *shipment.DriverID); err == nil {
			view.Driver = d.GetFullName()
		}
	}
	if order.DestinationWarehouseID != nil {
		if wh, err := s.dispatch.Warehouse(ctx, *order.DestinationWarehouseID); err == nil {
			view.Destination = wh.GetName()
			view.Drop = &FieldSite{Name: wh.GetName(), Address: wh.GetAddress(), Latitude: wh.GetLatitude(), Longitude: wh.GetLongitude()}
		}
	}
	if order.OriginWarehouseID != nil {
		if wh, err := s.dispatch.Warehouse(ctx, *order.OriginWarehouseID); err == nil {
			view.Origin = &FieldSite{Name: wh.GetName(), Address: wh.GetAddress(), Latitude: wh.GetLatitude(), Longitude: wh.GetLongitude()}
		}
	}
	view.PickupAt, view.ArrivedAt = order.PickupAt, shipment.ArrivedUnloadingAt
	if order.Detail != nil {
		view.Cargo = map[string]any{}
		for _, k := range []string{"cargoItems", "muatan", "totalBerat", "kuantitas", "totalVolume", "namaMuatan"} {
			if v, ok := order.Detail[k]; ok {
				view.Cargo[k] = v
			}
		}
	}
	if shipment.UnloadingCargoCheckedAt != nil && shipment.UnloadingCargoMatches != nil {
		view.CargoCheck = &FieldCargoCheck{Matches: *shipment.UnloadingCargoMatches, CheckedAt: *shipment.UnloadingCargoCheckedAt}
		if shipment.UnloadingCargoNote != nil {
			view.CargoCheck.Note = *shipment.UnloadingCargoNote
		}
	}
	view.Expected = figures(CargoFigures{
		WeightKg: money(order.WeightKg), VolumeM3: money(order.VolumeM3), Quantity: money(order.Quantity),
	})
	view.Actual = figures(CargoFigures{
		WeightKg: money(shipment.UnloadingAuditWeightKg),
		VolumeM3: money(shipment.UnloadingAuditVolumeM3),
		Quantity: money(shipment.UnloadingAuditQuantity),
	})
	view.FinalizedAt = shipment.ManifestFinalizedAt
	if shipment.ManifestFinalizedBy != nil {
		view.FinalizedBy = *shipment.ManifestFinalizedBy
	}
	if shipment.ManifestFinalizedNote != nil {
		view.FinalizedNote = *shipment.ManifestFinalizedNote
	}
	if s.pods != nil {
		if latest, err := s.pods.Latest(ctx, shipment.ID); err == nil {
			for i := range latest {
				if latest[i].Stage == "unloading" {
					view.Pod = &latest[i]
				}
			}
		}
	}
	return view, nil
}

// geofenceCheck works out whether the driver was at the unloading point.
//
// Recorded, never enforced — the same choice the shipment table already makes.
// A warehouse whose coordinates are wrong, a phone with poor GPS, or a gate two
// hundred metres from the pin would otherwise strand a driver who is standing
// exactly where they should be, with no way to proceed. The record is what a
// dispute needs; blocking is a policy a company can adopt later on data it will
// then have.
func (s *HandoverService) geofenceCheck(ctx context.Context, shipment *models.Shipment, in VerifyInput) (*models.Money, *models.Money, *bool) {
	if in.Latitude == nil || in.Longitude == nil {
		return nil, nil, nil
	}

	lat := decimal.NewFromFloat(*in.Latitude)
	lon := decimal.NewFromFloat(*in.Longitude)

	order, err := s.orders.FindByIDForService(ctx, shipment.OrderID)
	if err != nil || order.DestinationWarehouseID == nil {
		return &lat, &lon, nil
	}

	wh, err := s.dispatch.Warehouse(ctx, *order.DestinationWarehouseID)
	if err != nil {
		return &lat, &lon, nil
	}

	within := routing.WithinGeofence(
		routing.Point{Lon: *in.Longitude, Lat: *in.Latitude},
		routing.Point{Lon: wh.GetLongitude(), Lat: wh.GetLatitude()},
		int(wh.GetGeofenceRadiusMeters()),
	)
	return &lat, &lon, &within
}

// generateCode produces a six-digit code from the cryptographic source.
//
// crypto/rand rather than math/rand: this authorises the completion of a
// delivery, and a seeded generator makes every code on a given instance
// predictable from any one of them.
func generateCode() (string, error) {
	max := big.NewInt(1)
	for i := 0; i < handoverCodeDigits; i++ {
		max.Mul(max, big.NewInt(10))
	}

	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", fmt.Errorf("handover: cannot generate a code: %w", err)
	}
	// Zero-padded, so 000042 is a valid code rather than becoming "42" and
	// failing to match what the PIC reads out.
	return fmt.Sprintf("%0*d", handoverCodeDigits, n), nil
}

var nonDigits = regexp.MustCompile(`[^0-9]`)

// normaliseWhatsApp puts an Indonesian mobile number into the 62 form.
//
// People write the same number as 0812…, +62812…, 62812… and with spaces or
// dashes. WhatsApp addresses one of those, so normalising here is what makes
// "the code never arrived" a real failure rather than a formatting one.
func normaliseWhatsApp(raw string) (string, error) {
	digits := nonDigits.ReplaceAllString(raw, "")

	switch {
	case strings.HasPrefix(digits, "62"):
		// Already international.
	case strings.HasPrefix(digits, "0"):
		digits = "62" + digits[1:]
	case digits == "":
		return "", fmt.Errorf("%w: a WhatsApp number is required", ErrValidation)
	default:
		// A bare 812… with no leading zero, which is how people say it aloud.
		digits = "62" + digits
	}

	// 62 plus 9 to 13 digits covers every Indonesian mobile range. The check is
	// deliberately loose: refusing a valid number strands a delivery, while
	// accepting a wrong one merely means the code goes nowhere and the driver
	// asks again.
	if len(digits) < 11 || len(digits) > 15 {
		return "", fmt.Errorf("%w: %q does not look like a WhatsApp number", ErrValidation, raw)
	}
	return digits, nil
}

func ptrTime(t time.Time) *time.Time { return &t }
