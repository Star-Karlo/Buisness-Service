package services

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/models"
	notificationv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/notification/v1"
	"github.com/karlo/business-service/internal/repository"
)

// Web-Field: the receiving PIC's side of the unloading.
//
// The PIC arrives at the page with no account and no link — by typing the
// order number, or by scanning the QR the driver shows — and proves they are
// standing with the driver by entering the code the DRIVER holds. That is the
// whole security model: the order number is not a secret (it is printed on
// the surat jalan), the code is, and the code lives on the driver's phone.
//
// Once in, the PIC does two things the console used to do afterwards: records
// what was actually received against what was ordered, and finalises the
// manifest. Finalising is not reversible from the field page.

// fieldCodeWindow is how long after the code was sent the PIC may still use
// it. Longer than the driver's own ten minutes: the driver confirms at the
// gate, and the count that follows can take an hour on a full trailer, with
// the PIC opening the page when the last pallet is off.
const fieldCodeWindow = 12 * time.Hour

// FieldLookup is what the landing page learns from an order number: enough to
// confirm the right truck is at the gate, and nothing that is not already on
// the paperwork in the PIC's hand.
type FieldLookup struct {
	OrderNumber    string `json:"orderNumber"`
	ShipmentStatus string `json:"shipmentStatus"`
	Truck          string `json:"truck,omitempty"`
	Driver         string `json:"driver,omitempty"`
	Destination    string `json:"destination,omitempty"`
	// Ready says a code may be entered now. False before the driver has
	// confirmed the code in K-Trip, which is the state the design's yellow
	// banner describes.
	Ready   bool   `json:"ready"`
	Message string `json:"message,omitempty"`
}

// FieldLookup resolves an order number for the Web-Field landing page.
//
// It answers only for an order whose truck is actually at an unloading point
// with a code in play. An order number that exists but is a week old gets the
// same answer as one that does not exist at all, so the endpoint cannot be
// walked to enumerate a company's orders.
func (s *HandoverService) FieldLookup(ctx context.Context, orderNumber string) (*FieldLookup, error) {
	order, shipment, row, err := s.fieldResolve(ctx, orderNumber)
	if err != nil {
		return nil, err
	}

	out := &FieldLookup{OrderNumber: order.OrderNumber, ShipmentStatus: shipment.StatusCode}
	s.fillParties(ctx, shipment, order, &out.Truck, &out.Driver, &out.Destination)

	switch {
	case row.VerifiedAt == nil:
		out.Message = "Belum bisa verifikasi muatan. Pastikan driver sudah memasukkan Kode OTP di aplikasi K-Trip."
	case time.Since(row.SentAt) > fieldCodeWindow:
		out.Message = "Kode untuk order ini sudah kedaluwarsa. Minta driver meminta kode baru di K-Trip."
	default:
		out.Ready = true
	}
	return out, nil
}

// FieldSignIn exchanges the driver's code for a field token.
//
// The token is the session: it is what every later call carries, and it is
// issued only to somebody who could read the code off the driver's phone.
func (s *HandoverService) FieldSignIn(ctx context.Context, orderNumber, code string, picUserID *uuid.UUID) (string, error) {
	_, _, row, err := s.fieldResolve(ctx, orderNumber)
	if err != nil {
		return "", err
	}
	if row.VerifiedAt == nil {
		return "", fmt.Errorf("%w: driver belum memasukkan Kode OTP di K-Trip", ErrValidation)
	}
	if time.Since(row.SentAt) > fieldCodeWindow {
		return "", fmt.Errorf("%w: kode untuk order ini sudah kedaluwarsa", ErrValidation)
	}
	if row.FieldAttempts >= row.MaxAttempts {
		return "", fmt.Errorf("%w: terlalu banyak percobaan; minta driver meminta kode baru", ErrValidation)
	}
	if row.FieldToken == nil || *row.FieldToken == "" {
		return "", fmt.Errorf("%w: order ini tidak punya halaman Web-Field", ErrValidation)
	}

	sum := sha256.Sum256([]byte(strings.TrimSpace(code)))
	if subtle.ConstantTimeCompare(sum[:], row.CodeHash) != 1 {
		if err := s.handovers.RecordFieldAttempt(ctx, row.ID); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: kode tidak sesuai", ErrValidation)
	}

	if err := s.handovers.MarkFieldVerified(ctx, row.ID, picUserID); err != nil {
		return "", err
	}
	return *row.FieldToken, nil
}

// fieldResolve finds the handover behind an order number.
func (s *HandoverService) fieldResolve(ctx context.Context, orderNumber string) (*models.Order, *models.Shipment, *models.ShipmentHandover, error) {
	number := strings.ToUpper(strings.TrimSpace(orderNumber))
	notFound := fmt.Errorf("%w: nomor order tidak ditemukan atau belum sampai di titik bongkar", repository.ErrNotFound)
	if number == "" {
		return nil, nil, nil, notFound
	}
	order, err := s.orders.FindByNumber(ctx, number)
	if err != nil {
		return nil, nil, nil, notFound
	}
	shipment, err := s.shipments.FindByOrder(ctx, order.ID)
	if err != nil {
		return nil, nil, nil, notFound
	}
	row, err := s.handovers.LatestForStage(ctx, shipment.ID, "unloading")
	if err != nil {
		return nil, nil, nil, notFound
	}
	return order, shipment, row, nil
}

func (s *HandoverService) fillParties(ctx context.Context, shipment *models.Shipment, order *models.Order, truck, driver, destination *string) {
	if shipment.TruckID != nil {
		if t, err := s.dispatch.masterdata.GetTruck(ctx, *shipment.TruckID); err == nil {
			*truck = t.GetPoliceNumber()
		}
	}
	if shipment.DriverID != nil {
		if d, err := s.dispatch.masterdata.GetDriver(ctx, *shipment.DriverID); err == nil {
			*driver = d.GetFullName()
		}
	}
	if order.DestinationWarehouseID != nil {
		if wh, err := s.dispatch.Warehouse(ctx, *order.DestinationWarehouseID); err == nil {
			*destination = wh.GetName()
		}
	}
}

// FieldAuditIn is the PIC's count.
type FieldAuditIn struct {
	Matches  bool
	Note     string
	PICName  string
	WeightKg *float64
	VolumeM3 *float64
	Quantity *float64
}

// RecordFieldAudit stores what the PIC actually received.
//
// The ordered figures are never overwritten by this: the two sets sit side by
// side, which is what makes a claim arguable later.
func (s *HandoverService) RecordFieldAudit(ctx context.Context, token string, in FieldAuditIn) (*FieldView, error) {
	row, shipment, err := s.fieldByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if shipment.ManifestFinalizedAt != nil {
		return nil, fmt.Errorf("%w: manifest order ini sudah difinalisasi", ErrValidation)
	}
	if !in.Matches && strings.TrimSpace(in.Note) == "" {
		return nil, fmt.Errorf("%w: catatan wajib diisi bila data tidak sesuai", ErrValidation)
	}

	if err := s.lifecycle.RecordCargoCheck(ctx, shipment, CargoCheckInput{
		Stage: "unloading", Matches: in.Matches, Note: in.Note, Via: "field",
	}); err != nil {
		return nil, err
	}

	fields := map[string]interface{}{}
	putMoney(fields, "unloading_audit_weight_kg", in.WeightKg)
	putMoney(fields, "unloading_audit_volume_m3", in.VolumeM3)
	putMoney(fields, "unloading_audit_quantity", in.Quantity)
	if len(fields) > 0 {
		if err := s.shipments.UpdateFields(ctx, shipment.ID, fields); err != nil {
			return nil, err
		}
	}
	if name := strings.TrimSpace(in.PICName); name != "" {
		_ = s.handovers.SetPICName(ctx, row.ID, name)
	}

	s.notifyField(ctx, shipment, notificationv1.EventType_EVENT_TYPE_FIELD_AUDIT_RECORDED, map[string]interface{}{
		"picName": pickName(in.PICName, row.PICName),
		"result":  map[bool]string{true: "sesuai", false: "tidak sesuai"}[in.Matches],
	}, "field-audit:"+shipment.ID.String())

	return s.Field(ctx, token)
}

// FieldFinalizeIn closes the manifest.
type FieldFinalizeIn struct {
	PICName string
	Note    string
}

// FinalizeField is the PIC's closing act: the count is settled.
//
// Deliberately one-way from this page. The console can still reject the POD —
// that is the paperwork — but nobody at the gate can quietly revise what was
// counted once the driver has been let go.
func (s *HandoverService) FinalizeField(ctx context.Context, token string, in FieldFinalizeIn) (*FieldView, error) {
	row, shipment, err := s.fieldByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if shipment.UnloadingCargoCheckedAt == nil {
		return nil, fmt.Errorf("%w: audit data belum diisi", ErrValidation)
	}
	if shipment.ManifestFinalizedAt != nil {
		return s.Field(ctx, token)
	}

	name := pickName(in.PICName, row.PICName)
	fields := map[string]interface{}{
		"manifest_finalized_at": time.Now(),
		"manifest_finalized_by": name,
	}
	if note := strings.TrimSpace(in.Note); note != "" {
		fields["manifest_finalized_note"] = note
	}
	if err := s.shipments.UpdateFields(ctx, shipment.ID, fields); err != nil {
		return nil, err
	}

	s.notifyField(ctx, shipment, notificationv1.EventType_EVENT_TYPE_FIELD_MANIFEST_FINALIZED, map[string]interface{}{
		"picName": name,
	}, "field-final:"+shipment.ID.String())

	return s.Field(ctx, token)
}

func (s *HandoverService) fieldByToken(ctx context.Context, token string) (*models.ShipmentHandover, *models.Shipment, error) {
	row, err := s.handovers.FindByFieldToken(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	// The token alone is not enough to act: it is handed out only after the
	// code, and it stops working the moment the code's window closes.
	if row.FieldVerifiedAt == nil {
		return nil, nil, fmt.Errorf("%w: sesi Web-Field belum diverifikasi dengan Kode OTP", ErrForbidden)
	}
	shipment, err := s.shipments.FindByID(ctx, row.ShipmentID)
	if err != nil {
		return nil, nil, err
	}
	return row, shipment, nil
}

// notifyField tells the transporter what happened at the gate.
func (s *HandoverService) notifyField(ctx context.Context, shipment *models.Shipment, event notificationv1.EventType, params map[string]interface{}, idem string) {
	order, err := s.orders.FindByIDForService(ctx, shipment.OrderID)
	if err != nil {
		return
	}
	params["orderNumber"] = order.OrderNumber
	audience := clients.ToCompanyRoles(order.TransporterCompanyID.String(), "Administrator", "Operation", "Dispatcher")
	s.notifier.Notify(ctx, clients.Event{
		Type:           event,
		Subject:        clients.Subject{ID: order.ID.String(), Type: "order"},
		Audience:       audience,
		Params:         params,
		IdempotencyKey: idem,
	})
}

// FieldInboxRow is one arriving shipment in a signed-in PIC's list.
type FieldInboxRow struct {
	OrderNumber    string     `json:"orderNumber"`
	ShipmentStatus string     `json:"shipmentStatus"`
	Truck          string     `json:"truck,omitempty"`
	Driver         string     `json:"driver,omitempty"`
	Destination    string     `json:"destination,omitempty"`
	ArrivedAt      *time.Time `json:"arrivedAt,omitempty"`
	// Ready mirrors FieldLookup: the driver has confirmed the code, so this
	// row can be opened with the code the driver holds.
	Ready bool `json:"ready"`
}

// FieldInbox lists the shipments arriving at the caller's own sites.
//
// A convenience, not a credential: opening any of these still needs the code
// from the driver's phone. What signing in buys is not having to be told the
// order number over the radio.
func (s *HandoverService) FieldInbox(ctx context.Context, actor Actor) ([]FieldInboxRow, error) {
	shipments, orders, err := s.shipments.ListInboundForCompany(ctx, actor.CompanyID)
	if err != nil {
		return nil, err
	}

	mine := map[string]bool{}
	out := make([]FieldInboxRow, 0, len(shipments))
	for i := range shipments {
		order := orders[i]
		if order.DestinationWarehouseID == nil {
			continue
		}
		whID := *order.DestinationWarehouseID
		isMine, known := mine[whID]
		if !known {
			wh, err := s.dispatch.Warehouse(ctx, whID)
			if err != nil {
				continue
			}
			for _, id := range wh.GetPicUserIds() {
				if id == actor.UserID.String() {
					isMine = true
					break
				}
			}
			mine[whID] = isMine
		}
		if !isMine {
			continue
		}

		row := FieldInboxRow{
			OrderNumber:    order.OrderNumber,
			ShipmentStatus: shipments[i].StatusCode,
			ArrivedAt:      shipments[i].ArrivedUnloadingAt,
		}
		s.fillParties(ctx, &shipments[i], &order, &row.Truck, &row.Driver, &row.Destination)
		if h, err := s.handovers.LatestForStage(ctx, shipments[i].ID, "unloading"); err == nil && h.VerifiedAt != nil {
			row.Ready = time.Since(h.SentAt) <= fieldCodeWindow
		}
		out = append(out, row)
	}
	return out, nil
}

func putMoney(fields map[string]interface{}, column string, v *float64) {
	if v == nil {
		return
	}
	fields[column] = decimal.NewFromFloat(*v)
}

func pickName(typed string, stored *string) string {
	if n := strings.TrimSpace(typed); n != "" {
		return n
	}
	if stored != nil && strings.TrimSpace(*stored) != "" {
		return *stored
	}
	return "PIC gudang"
}
