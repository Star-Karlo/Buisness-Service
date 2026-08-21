package clients

import (
	"context"
	"fmt"

	"github.com/karlo/business-service/internal/platform/authctx"
	authv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/auth/v1"
	masterdatav1 "github.com/karlo/business-service/internal/platform/genproto/karlo/masterdata/v1"
	"github.com/karlo/business-service/internal/platform/grpcutil"
	"google.golang.org/grpc"
)

// Auth is the authentication service client.
type Auth struct {
	conn   *grpc.ClientConn
	client authv1.AuthServiceClient
}

func NewAuth(target, serviceName, serviceToken string) (*Auth, error) {
	conn, err := grpcutil.Dial(grpcutil.DialConfig{
		Service: serviceName, Target: target, ServiceToken: serviceToken,
	})
	if err != nil {
		return nil, err
	}
	return &Auth{conn: conn, client: authv1.NewAuthServiceClient(conn)}, nil
}

func (a *Auth) Close() error { return a.conn.Close() }

// ValidateToken satisfies authctx.RemoteValidator, for API-key callers.
func (a *Auth) ValidateToken(ctx context.Context, token string) (authctx.Principal, error) {
	resp, err := a.client.ValidateToken(ctx, &authv1.ValidateTokenRequest{Token: token})
	if err != nil {
		return authctx.Principal{}, fmt.Errorf("clients: validate token: %w", err)
	}
	if !resp.GetValid() {
		return authctx.Principal{}, fmt.Errorf("clients: %s", resp.GetReason())
	}

	user := resp.GetUser()
	p := authctx.Principal{
		UserID:    user.GetId(),
		Role:      user.GetRole(),
		CompanyID: user.GetCompanyId(),
		ParentID:  user.GetParentId(),
	}
	if perm := user.GetPermission(); len(perm) > 0 {
		p.Permission = make(map[string]map[string]bool, len(perm))
		for module, actions := range perm {
			p.Permission[module] = actions.GetActions()
		}
	}
	return p, nil
}

// CompanySettings fetches the business toggles that govern order rules:
// whether cancellation needs validation, whether completion is geofenced, the
// tax percentages used on invoices.
func (a *Auth) CompanySettings(ctx context.Context, companyID string) (*authv1.CompanySettings, error) {
	resp, err := a.client.GetCompany(ctx, &authv1.GetCompanyRequest{Id: companyID})
	if err != nil {
		return nil, fmt.Errorf("clients: get company: %w", err)
	}
	settings := resp.GetCompany().GetSettings()
	if settings == nil {
		// A company with no stored settings uses the platform defaults rather
		// than zero values, which would silently set both tax rates to nought.
		return &authv1.CompanySettings{
			PpnPercentage:   0.02,
			Pph23Percentage: 0.11,
		}, nil
	}
	return settings, nil
}

// GetUser resolves one user.
func (a *Auth) GetUser(ctx context.Context, id string) (*authv1.User, error) {
	resp, err := a.client.GetUser(ctx, &authv1.GetUserRequest{Id: id})
	if err != nil {
		return nil, fmt.Errorf("clients: get user: %w", err)
	}
	return resp.GetUser(), nil
}

// MasterData is the master data service client.
type MasterData struct {
	conn   *grpc.ClientConn
	client masterdatav1.MasterDataServiceClient
}

func NewMasterData(target, serviceName, serviceToken string) (*MasterData, error) {
	conn, err := grpcutil.Dial(grpcutil.DialConfig{
		Service: serviceName, Target: target, ServiceToken: serviceToken,
	})
	if err != nil {
		return nil, err
	}
	return &MasterData{conn: conn, client: masterdatav1.NewMasterDataServiceClient(conn)}, nil
}

func (m *MasterData) Close() error { return m.conn.Close() }

// GetTruck resolves a vehicle.
func (m *MasterData) GetTruck(ctx context.Context, id string) (*masterdatav1.Truck, error) {
	resp, err := m.client.GetTruck(ctx, &masterdatav1.GetTruckRequest{Id: id})
	if err != nil {
		return nil, fmt.Errorf("clients: get truck: %w", err)
	}
	return resp.GetTruck(), nil
}

// GetWarehouse resolves a location, including its geofence radius.
func (m *MasterData) GetWarehouse(ctx context.Context, id string) (*masterdatav1.Warehouse, error) {
	resp, err := m.client.GetWarehouse(ctx, &masterdatav1.GetWarehouseRequest{Id: id})
	if err != nil {
		return nil, fmt.Errorf("clients: get warehouse: %w", err)
	}
	return resp.GetWarehouse(), nil
}

// DriverIsPairedWithTruck reports whether a driver may take a given truck.
//
// The business service must not assume the pairing; the master data service is
// the authority on which drivers a truck is assigned to. The legacy
// assign-driver endpoint wrote both ids with no check at all, so an order could
// be assigned to a driver who had no access to that vehicle.
func (m *MasterData) DriverIsPairedWithTruck(ctx context.Context, driverID, truckID string) (bool, error) {
	resp, err := m.client.GetTrucksByDriver(ctx, &masterdatav1.GetTrucksByDriverRequest{DriverId: driverID})
	if err != nil {
		return false, fmt.Errorf("clients: trucks by driver: %w", err)
	}
	for _, t := range resp.GetTrucks() {
		if t.GetId() == truckID {
			return true, nil
		}
	}
	return false, nil
}

// ValidateCatalogRefs checks a set of catalogue references before a write.
func (m *MasterData) ValidateCatalogRefs(ctx context.Context, refs []*masterdatav1.CatalogRef) error {
	if len(refs) == 0 {
		return nil
	}

	resp, err := m.client.ValidateReferences(ctx, &masterdatav1.ValidateReferencesRequest{Refs: refs})
	if err != nil {
		return fmt.Errorf("clients: validate references: %w", err)
	}
	if !resp.GetValid() {
		var names []string
		for _, ref := range resp.GetInvalid() {
			names = append(names, ref.GetKind().String()+"="+ref.GetId())
		}
		return fmt.Errorf("clients: unknown master data references: %v", names)
	}
	return nil
}
