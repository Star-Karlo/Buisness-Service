package clients

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/karlo/business-service/internal/platform/authctx"
	"github.com/karlo/business-service/internal/platform/cache"
	authv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/auth/v1"
	commonv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/common/v1"
	masterdatav1 "github.com/karlo/business-service/internal/platform/genproto/karlo/masterdata/v1"
	"github.com/karlo/business-service/internal/platform/grpcutil"
	"google.golang.org/grpc"
)

// Auth is the authentication service client.
type Auth struct {
	conn   *grpc.ClientConn
	client authv1.AuthServiceClient
	cache  cache.Cache
}

func NewAuth(target, serviceName, serviceToken string, c cache.Cache) (*Auth, error) {
	conn, err := grpcutil.Dial(grpcutil.DialConfig{
		Service: serviceName, Target: target, ServiceToken: serviceToken,
	})
	if err != nil {
		return nil, err
	}
	return &Auth{conn: conn, client: authv1.NewAuthServiceClient(conn), cache: c}, nil
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

	return principalFrom(resp.GetUser()), nil
}

// principalFrom maps the authentication service's user into a principal.
//
// The per-product access map is copied wholesale rather than flattened: this
// service resolves its own product through authctx, and flattening here would
// discard the other product's access from a token that legitimately carries
// both.
func principalFrom(user *authv1.User) authctx.Principal {
	p := authctx.Principal{
		UserID:          user.GetId(),
		CompanyID:       user.GetCompanyId(),
		IsPlatformStaff: user.GetIsPlatformStaff(),
	}

	if access := user.GetAccess(); len(access) > 0 {
		p.Access = make(map[authctx.Product]authctx.ProductAccess, len(access))
		for product, a := range access {
			p.Access[authctx.Product(product)] = authctx.ProductAccess{
				Role:        a.GetRole(),
				Permissions: a.GetPermissions(),
				Features:    a.GetFeatures(),
				// Carried across the wire. Without it an administrator rebuilt
				// from a remote validation arrives with an empty permission
				// list and is refused everywhere — which is what happened.
				GrantsAll: a.GetGrantsAll(),
			}
		}
	}

	return p
}

// CompanySettings fetches the business toggles that govern order rules:
// whether cancellation needs validation, whether completion is geofenced, the
// tax percentages used on invoices.
//
// Cached, because this is read on every order creation, every geofenced arrival
// and every invoice, while the settings themselves change perhaps twice a year.
// Uncached it puts a cross-service gRPC call on the critical path of the
// busiest write in the system.
//
// The TTL is short — five minutes — because these values decide what a customer
// is charged. A stale PPN rate produces an invoice that is wrong in a way
// nobody notices until reconciliation, so the window in which that is possible
// is kept small deliberately.
func (a *Auth) CompanySettings(ctx context.Context, companyID string) (*authv1.CompanySettings, error) {
	key := cache.Key("business", "company", "settings", companyID)

	var cached authv1.CompanySettings
	if cache.GetJSON(ctx, a.cache, key, &cached) {
		return &cached, nil
	}

	resp, err := a.client.GetCompany(ctx, &authv1.GetCompanyRequest{Id: companyID})
	if err != nil {
		return nil, fmt.Errorf("clients: get company: %w", err)
	}

	settings := resp.GetCompany().GetSettings()
	if settings == nil {
		// A company with no stored settings uses the platform defaults rather
		// than zero values, which would silently set both tax rates to nought.
		// Not cached: this is a fallback for missing data, and caching it would
		// hide the moment the real settings appear.
		return &authv1.CompanySettings{
			PpnPercentage:   0.02,
			Pph23Percentage: 0.11,
		}, nil
	}

	cache.SetJSON(ctx, a.cache, key, settings, 5*time.Minute)
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

// GetCatalogItem resolves one catalogue entry (a truck type, brand, body …)
// as the company sees it: platform-global entries plus its own.
func (m *MasterData) GetCatalogItem(ctx context.Context, kind masterdatav1.CatalogKind, id, companyID string) (*masterdatav1.CatalogItem, error) {
	resp, err := m.client.GetCatalogItem(ctx, &masterdatav1.GetCatalogItemRequest{Kind: kind, Id: id, CompanyId: companyID})
	if err != nil {
		return nil, fmt.Errorf("clients: get catalog item: %w", err)
	}
	return resp.GetItem(), nil
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

// GetDriver resolves a master-data driver: the phone to reach them on and
// the login they may or may not have.
func (m *MasterData) GetDriver(ctx context.Context, id string) (*masterdatav1.Driver, error) {
	resp, err := m.client.GetDriver(ctx, &masterdatav1.GetDriverRequest{Id: id})
	if err != nil {
		return nil, fmt.Errorf("clients: get driver: %w", err)
	}
	return resp.GetDriver(), nil
}

// ValidateCatalogRefs checks a set of catalogue references before a write.
//
// The company is passed explicitly: a reference to another company's private
// catalogue entry must come back invalid. Omitting it would validate against
// the global catalogues only, which would reject a company's own item types.
func (m *MasterData) ValidateCatalogRefs(ctx context.Context, companyID string, refs []*masterdatav1.CatalogRef) error {
	if len(refs) == 0 {
		return nil
	}

	resp, err := m.client.ValidateReferences(ctx, &masterdatav1.ValidateReferencesRequest{
		Refs:      refs,
		CompanyId: companyID,
	})
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

// CompanyNames resolves company ids to display names.
//
// Agreements and invoices name two companies each, by id. A list rendered from
// those ids shows a column of UUIDs, which is worse than showing nothing — so
// the names are resolved here rather than leaving every client to fetch them
// one row at a time.
//
// Deduplicated: a page of agreements between the same two companies costs two
// lookups, not two per row. Failures are skipped rather than returned, because
// a missing name should leave a blank cell, not take the list down.
// CompanyProfile is the little a caller usually needs about a counterparty.
type CompanyProfile struct {
	Name string
	// Abbreviation is the code that appears in agreement numbers. Empty when
	// the company has none, which callers must handle rather than assume.
	Abbreviation string
}

// CompanyProfiles resolves several companies at once.
//
// Separate from CompanyNames because agreement numbering needs the code as well
// as the name, and a second round trip per company to fetch it would double the
// calls on a path that already runs per agreement created.
func (a *Auth) CompanyProfiles(ctx context.Context, ids []string) map[string]CompanyProfile {
	out := make(map[string]CompanyProfile, len(ids))
	seen := map[string]bool{}

	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		resp, err := a.client.GetCompany(ctx, &authv1.GetCompanyRequest{Id: id})
		if err != nil {
			slog.WarnContext(ctx, "could not resolve a company",
				"company_id", id, "error", err)
			continue
		}
		if c := resp.GetCompany(); c != nil {
			out[id] = CompanyProfile{Name: c.GetName(), Abbreviation: c.GetAbbreviation()}
		}
	}
	return out
}

func (a *Auth) CompanyNames(ctx context.Context, ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	seen := map[string]bool{}

	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		resp, err := a.client.GetCompany(ctx, &authv1.GetCompanyRequest{Id: id})
		if err != nil {
			slog.WarnContext(ctx, "could not resolve a company name",
				"company_id", id, "error", err)
			continue
		}
		if c := resp.GetCompany(); c != nil {
			out[id] = c.GetName()
		}
	}
	return out
}

// ListTrucks is the whole fleet, available or not: the planner's map draws
// every truck the company has, not only the ones it could assign right now.
// Same cap as ListAvailableTrucks, for the same reason.
func (m *MasterData) ListTrucks(ctx context.Context, companyID string) ([]*masterdatav1.Truck, error) {
	const maxFleet = 500

	resp, err := m.client.ListTrucks(ctx, &masterdatav1.ListTrucksRequest{
		CompanyId: companyID,
		// Master data's pages are zero-based. Page 1 was the second page, which
		// for every real fleet is empty — so the planner had no trucks to rank
		// and no IMEI to ask telemetry about, and nothing said why.
		Query: &commonv1.Query{Page: &commonv1.Page{Page: 0, PageSize: maxFleet}},
	})
	if err != nil {
		return nil, fmt.Errorf("clients: list trucks: %w", err)
	}
	return resp.GetTrucks(), nil
}

// ListAvailableTrucks returns the company's trucks that are free to take work.
//
// Availability is filtered at master data rather than here: the fleet is that
// service's to describe, and pulling every truck back to discard most of them
// would grow with the fleet while the useful answer does not.
//
// The page size is a deliberate ceiling rather than a full listing. A planner
// choosing a truck for one order is choosing from the nearest handful; a
// company with more trucks than this has a fleet whose selection needs
// filtering by depot or group, which is a product decision rather than
// something a bigger page silently papers over.
func (m *MasterData) ListAvailableTrucks(ctx context.Context, companyID string) ([]*masterdatav1.Truck, error) {
	const maxCandidates = 500

	resp, err := m.client.ListTrucks(ctx, &masterdatav1.ListTrucksRequest{
		CompanyId: companyID,
		Query: &commonv1.Query{
			Page: &commonv1.Page{Page: 0, PageSize: maxCandidates},
			Filtered: []*commonv1.Filter{
				{Id: "isAvailable", Value: "true", Operator: commonv1.Operator_OPERATOR_EQ},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("clients: list trucks: %w", err)
	}
	return resp.GetTrucks(), nil
}
