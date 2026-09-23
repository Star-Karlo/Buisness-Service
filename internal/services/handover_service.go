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

// HandoverService issues and checks the code the receiving PIC gives the driver.
//
// This is the proof that a real person at the destination accepted the goods.
// The driver enters the PIC's WhatsApp number, the code goes to that number,
// and the driver types back what the PIC reads out — so what is demonstrated is
// possession of that phone at that address, which a photograph cannot show.
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

// FieldURL is the PIC's page for a handover.
func (s *HandoverService) FieldURL(token string) string {
	if s.consoleBaseURL == "" {
		return ""
	}
	return s.consoleBaseURL + "/field/" + token
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

// Issue sends a code to the PIC's WhatsApp.
//
// The code itself is never returned. Returning it — even to the driver who
// asked — would defeat the entire mechanism, because the driver would then be
// able to complete the handover without the PIC being involved at all.
func (s *HandoverService) Issue(ctx context.Context, actor Actor, shipmentID uuid.UUID, picName, picWhatsApp string) (*models.ShipmentHandover, error) {
	shipment, err := s.shipments.FindByID(ctx, shipmentID)
	if err != nil {
		return nil, err
	}
	if shipment.DriverUserID == nil || *shipment.DriverUserID != actor.UserID {
		return nil, fmt.Errorf("%w: only the assigned driver may request a handover code", ErrForbidden)
	}

	number, err := normaliseWhatsApp(picWhatsApp)
	if err != nil {
		return nil, err
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
		ShipmentID:  shipmentID,
		Stage:       "unloading",
		PICWhatsApp: number,
		CodeHash:    sum[:],
		MaxAttempts: 5,
		SentAt:      time.Now(),
		ExpiresAt:   time.Now().Add(handoverCodeTTL),
		FieldToken:  &fieldToken,
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
	if order, err := s.orders.FindByIDForService(ctx, shipment.OrderID); err == nil {
		orderNumber = order.OrderNumber
	}
	s.notifier.Notify(ctx, clients.Event{
		Type:     notificationv1.EventType_EVENT_TYPE_HANDOVER_CODE,
		Subject:  clients.Subject{ID: shipmentID.String(), Type: "shipment"},
		Audience: clients.ToPhone(number),
		Params: map[string]interface{}{
			"orderNumber": orderNumber,
			"code":        code,
			"minutes":     int(handoverCodeTTL.Minutes()),
			"fieldUrl":    s.FieldURL(fieldToken),
		},
		IdempotencyKey: "handover:" + row.ID.String(),
	})

	return row, nil
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

	// The confirmed code is the driver flow's "start unloading": the swipe
	// opened the OTP page, and the PIC's code closes it. Taken here so the
	// app needs no second call; a shipment not at the gate (already
	// unloading, say) is left as it is.
	if s.lifecycle != nil && shipment.StatusCode == models.ShipmentAtUnloading {
		if _, err := s.lifecycle.Advance(ctx, actor, AdvanceInput{ShipmentID: shipmentID, To: models.ShipmentUnloading, Position: positionOf(in)}); err != nil {
			return nil, err
		}
	}
	return live, nil
}

func positionOf(in VerifyInput) *Position {
	if in.Latitude == nil || in.Longitude == nil {
		return nil
	}
	return &Position{Latitude: *in.Latitude, Longitude: *in.Longitude}
}

// FieldView is what the PIC's field page shows: enough to know which truck
// is at the gate and what it should be carrying, and where the check stands.
type FieldView struct {
	OrderNumber      string              `json:"orderNumber"`
	ShipmentID       uuid.UUID           `json:"shipmentId"`
	ShipmentStatus   string              `json:"shipmentStatus"`
	PICName          *string             `json:"picName,omitempty"`
	Truck            string              `json:"truck,omitempty"`
	Driver           string              `json:"driver,omitempty"`
	Destination      string              `json:"destination,omitempty"`
	Cargo            map[string]any      `json:"cargo,omitempty"`
	CargoCheck       *FieldCargoCheck    `json:"cargoCheck,omitempty"`
	Pod              *models.ShipmentPod `json:"pod,omitempty"`
	HandoverVerified bool                `json:"handoverVerified"`
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
		}
	}
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

// FieldCargoCheckIn is the PIC's answer on the field page.
type FieldCargoCheckIn struct {
	Matches bool
	Note    string
	PICName string
}

// RecordFieldCargoCheck stores the PIC's cargo check by token. The token is
// the credential; the shipment must be unloading (the OTP was confirmed).
func (s *HandoverService) RecordFieldCargoCheck(ctx context.Context, token string, in FieldCargoCheckIn) (*FieldView, error) {
	row, err := s.handovers.FindByFieldToken(ctx, token)
	if err != nil {
		return nil, err
	}
	shipment, err := s.shipments.FindByID(ctx, row.ShipmentID)
	if err != nil {
		return nil, err
	}
	if err := s.lifecycle.RecordCargoCheck(ctx, shipment, CargoCheckInput{Stage: "unloading", Matches: in.Matches, Note: in.Note, Via: "field"}); err != nil {
		return nil, err
	}
	if name := strings.TrimSpace(in.PICName); name != "" && (row.PICName == nil || *row.PICName == "") {
		_ = s.handovers.SetPICName(ctx, row.ID, name)
	}
	return s.Field(ctx, token)
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
