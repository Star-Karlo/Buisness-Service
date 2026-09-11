package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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

	row := &models.ShipmentHandover{
		ShipmentID:  shipmentID,
		Stage:       "unloading",
		PICWhatsApp: number,
		CodeHash:    sum[:],
		MaxAttempts: 5,
		SentAt:      time.Now(),
		ExpiresAt:   time.Now().Add(handoverCodeTTL),
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
	s.notifier.Notify(ctx, clients.Event{
		Type:     notificationv1.EventType_EVENT_TYPE_OTP,
		Subject:  clients.Subject{ID: shipmentID.String(), Type: "shipment"},
		Audience: clients.ToPhone(number),
		Params: map[string]interface{}{
			"code":       code,
			"expiryMins": int(handoverCodeTTL.Minutes()),
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

	sum := sha256.Sum256([]byte(strings.TrimSpace(in.Code)))
	// Constant-time, so the comparison does not leak how much of the code was
	// right through how long it took. Six digits is a small space and a timing
	// oracle would shrink it to a handful of guesses.
	if subtle.ConstantTimeCompare(sum[:], live.CodeHash) != 1 {
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
	return live, nil
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
