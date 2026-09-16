package services

import (
	"testing"

	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/models"
)

func TestMachineRoleComesFromTheOrderSide(t *testing.T) {
	transporter := uuid.New()
	shipper := uuid.New()
	order := &models.Order{ShipperCompanyID: shipper, TransporterCompanyID: &transporter}

	if got := machineRole(Actor{Role: "Administrator", CompanyID: transporter}, order); got != models.RoleTransporter {
		t.Fatalf("transporter-side tenant admin: got %q", got)
	}
	if got := machineRole(Actor{Role: "Owner", CompanyID: shipper}, order); got != models.RoleShipper {
		t.Fatalf("shipper-side tenant: got %q", got)
	}
	if got := machineRole(Actor{Role: models.RoleAdmin, CompanyID: uuid.New()}, order); got != models.RoleAdmin {
		t.Fatalf("platform staff stay admin: got %q", got)
	}
	if got := machineRole(Actor{Role: models.RoleDriver, CompanyID: transporter}, order); got != models.RoleDriver {
		t.Fatalf("driver keeps persona: got %q", got)
	}
	if got := models.NormaliseRole("Driver"); got != models.RoleDriver {
		t.Fatalf("NormaliseRole(Driver) = %q", got)
	}
	if got := models.NormaliseRole("Administrator"); got != "Administrator" {
		t.Fatalf("unknown names pass through, got %q", got)
	}
}
