// Package fieldconfig declares which parts of an agreement and an order a
// company may switch on, off, or demand.
//
// The flow is fixed — agreement, order, dispatch, driver, loading, unloading —
// and nothing here can change it. What differs between companies is how much
// detail each step captures, and the two cases that prompted this turn out to
// be one mechanism:
//
//	one company routes city to city, another needs the kecamatan
//	one company names a cargo category, another itemises every component
//
// Both are "is this field required, optional, or absent here". So there is one
// table of answers rather than a routeGranularity setting, an itemDetail
// setting, and whatever the third customer asks for.
//
// The direction of authority matches the permission catalogue in
// authentication, deliberately:
//
//	code --declares--> field_definitions --read by--> configurator and forms
//
// A row in the database cannot invent a field. Inserting one would put a toggle
// in the configurator that saves happily and changes nothing, because no form
// renders it and no handler reads it — a failure with no error to find. What
// the database DOES own is the answer per company, plus wording and ordering,
// none of which should need a deploy.
package fieldconfig

// Requirement is what a company demands of one field.
type Requirement string

const (
	// Required refuses the submission when the field is empty.
	Required Requirement = "required"

	// Optional accepts it either way.
	Optional Requirement = "optional"

	// Hidden removes the field from the form and REFUSES it if sent anyway.
	//
	// Refusing rather than ignoring is the deliberate choice. A hidden field
	// that silently drops its value looks to the caller exactly like one that
	// saved, and the difference only surfaces later as a missing kecamatan on
	// an order somebody swears they filled in.
	Hidden Requirement = "hidden"
)

func (r Requirement) Valid() bool {
	switch r {
	case Required, Optional, Hidden:
		return true
	}
	return false
}

// Entity is a form whose detail is configurable.
type Entity string

const (
	EntityAgreement Entity = "agreement"
	EntityOrder     Entity = "order"
)

// DataType tells a generic form renderer what control to draw. The server does
// not validate against it beyond presence: coercion belongs to the handler that
// binds the payload, and duplicating it here would give two answers to the same
// question.
type DataType string

const (
	TypeString   DataType = "string"
	TypeNumber   DataType = "number"
	TypeBoolean  DataType = "boolean"
	TypeDate     DataType = "date"
	TypeDateTime DataType = "datetime"

	// TypeRef is an id belonging to master data — a city, a cargo type.
	TypeRef DataType = "ref"

	// TypeList is a repeating group, e.g. order line items.
	TypeList DataType = "list"
)

// Field is one configurable input.
type Field struct {
	Entity Entity `json:"entity"`
	// Key is the dotted path into the entity's payload. It is the identity of
	// the field, not a label for it: the form binds to this path and the
	// validator walks it.
	Key      string   `json:"key"`
	DataType DataType `json:"dataType"`

	// Locked marks a field that exists but must never be switched off. An
	// order with no origin warehouse is not a configuration choice.
	//
	// Inverted rather than a Configurable flag so that the zero value is
	// "configurable": a new field added below without thinking about this
	// becomes tunable, which is recoverable, instead of silently frozen for
	// every company, which looks like the configurator is broken.
	Locked bool `json:"locked"`

	// Default is what a company that has never configured this field gets.
	// Sent so a configurator can show what "reset" would restore.
	Default Requirement `json:"default"`

	Group string `json:"group"`
	// Label is the DECLARED wording. Resolved.EffectiveLabel overrides it in
	// the JSON output, so a client reads `label` and gets the company's
	// wording; this one is shadowed deliberately.
	Label string `json:"-"`
	Help  string `json:"help,omitempty"`
	Sort  int    `json:"sort"`
}

// agreementFields are the configurable parts of an agreement.
var agreementFields = []Field{
	{Key: "customerId", DataType: TypeRef, Default: Required, Group: "Parties", Label: "Customer", Sort: 10},
	{Key: "agreementType", DataType: TypeString, Default: Required, Group: "Parties", Label: "Agreement type", Help: "Single or multiple shipment", Sort: 20},

	{Key: "validFrom", DataType: TypeDate, Default: Required, Group: "Validity", Label: "Effective date", Sort: 30},
	{Key: "validUntil", DataType: TypeDate, Default: Required, Group: "Validity", Label: "Expiry date", Sort: 40},

	{Key: "route.originCityId", DataType: TypeRef, Default: Required, Group: "Route", Label: "Origin city", Sort: 50},
	{Key: "route.destinationCityId", DataType: TypeRef, Default: Required, Group: "Route", Label: "Destination city", Sort: 60},

	// The kecamatan pair. Hidden by default because most companies price by
	// city, and a field that appears for everyone in order to serve one
	// customer is a field everyone learns to leave blank.
	{Key: "route.originDistrictId", DataType: TypeRef, Default: Hidden, Group: "Route",
		Label: "Origin kecamatan",
		Help:  "Enable when lanes are priced below city level.", Sort: 70},
	{Key: "route.destinationDistrictId", DataType: TypeRef, Default: Hidden, Group: "Route",
		Label: "Destination kecamatan",
		Help:  "Enable when lanes are priced below city level.", Sort: 80},

	{Key: "truckTypeId", DataType: TypeRef, Default: Optional, Group: "Fleet", Label: "Truck type", Sort: 90},

	// The body-by-size matrix: which combinations of body kind and truck size
	// this agreement covers. A grid rather than a list because that is the
	// question — a Wingbox CDD and a Wingbox Tronton are different vehicles at
	// different prices, and a flat list of either axis cannot say which pairs
	// are agreed.
	//
	// Hidden by default. Most agreements name a truck type or leave it open;
	// the matrix is for the companies that price per combination, and a grid
	// of ninety checkboxes on every form is a grid everyone learns to skip.
	{Key: "truckMatrix", DataType: TypeList, Default: Hidden, Group: "Fleet",
		Label: "Truck type matrix",
		Help:  "Enable to agree specific body-and-size combinations rather than a single truck type.", Sort: 95},

	// Optional, as in the revamp's agreement form: a contract may price a
	// lane before the cargo on it is known. The order names the cargo.
	{Key: "cargoTypeId", DataType: TypeRef, Default: Optional, Group: "Cargo", Label: "Cargo type", Sort: 100},
	{Key: "cargoItemIds", DataType: TypeList, Default: Optional, Group: "Cargo", Label: "Cargo items", Sort: 110},

	{Key: "pricingTypeId", DataType: TypeRef, Default: Required, Group: "Commercial", Label: "Pricing type", Sort: 120},
	{Key: "price", DataType: TypeNumber, Default: Required, Group: "Commercial", Label: "Price", Sort: 130},
	{Key: "minQuantity", DataType: TypeNumber, Default: Optional, Group: "Commercial", Label: "Minimum load", Sort: 140},
	{Key: "paymentTypeId", DataType: TypeRef, Default: Optional, Group: "Commercial", Label: "Payment terms", Sort: 150},
	{Key: "termsAndConditions", DataType: TypeString, Default: Optional, Group: "Commercial", Label: "Terms and conditions", Sort: 160},
	{Key: "paymentTermDays", DataType: TypeNumber, Default: Optional, Group: "Commercial", Label: "Payment terms (TOP days)", Sort: 165},
	{Key: "incomeTaxIncluded", DataType: TypeBoolean, Default: Optional, Group: "Commercial", Label: "PPh 23 included in price", Sort: 170},
	{Key: "minLoad", DataType: TypeNumber, Default: Optional, Group: "Commercial", Label: "Minimum load", Sort: 175},
	{Key: "maxLoad", DataType: TypeNumber, Default: Optional, Group: "Commercial", Label: "Maximum load", Sort: 180},
}

// orderFields are the configurable parts of an order.
var orderFields = []Field{
	{Key: "customerId", DataType: TypeRef, Default: Required, Group: "Parties", Label: "Customer", Sort: 10},
	{Key: "agreementId", DataType: TypeRef, Default: Required, Group: "Parties", Label: "Agreement", Sort: 20},

	// Not configurable: an order without a loading point cannot be dispatched,
	// and the driver app has nowhere to send anybody.
	{Key: "originWarehouseId", DataType: TypeRef, Locked: true, Default: Required,
		Group: "Route", Label: "Loading point", Sort: 30},
	{Key: "destinationWarehouseId", DataType: TypeRef, Locked: true, Default: Required,
		Group: "Route", Label: "Unloading point", Sort: 40},

	{Key: "pickupAt", DataType: TypeDateTime, Default: Required, Group: "Schedule", Label: "Estimated loading time", Sort: 50},
	{Key: "expiresAt", DataType: TypeDateTime, Default: Optional, Group: "Schedule", Label: "Order expiry", Sort: 60},

	// Optional: the revamp's Input Order wizard takes the cargo from the
	// agreement's item (detail.muatan) rather than asking again.
	{Key: "cargoTypeId", DataType: TypeRef, Default: Optional, Group: "Cargo", Label: "Cargo type", Sort: 70},

	// The itemisation switch. A company that ships bulk chemicals names the
	// category and stops; a company shipping mixed cartons needs every line.
	// Hidden by default for the same reason as the kecamatan.
	{Key: "items", DataType: TypeList, Default: Hidden, Group: "Cargo",
		Label: "Itemised cargo",
		Help:  "Enable to record every component with its own weight and dimensions.", Sort: 80},

	{Key: "quantity", DataType: TypeNumber, Default: Optional, Group: "Cargo", Label: "Quantity", Sort: 90},
	{Key: "weightKg", DataType: TypeNumber, Default: Optional, Group: "Cargo", Label: "Total weight (kg)", Sort: 100},
	{Key: "volumeM3", DataType: TypeNumber, Default: Optional, Group: "Cargo", Label: "Total volume (m³)", Sort: 110},

	{Key: "referenceNumber", DataType: TypeString, Default: Optional, Group: "Reference", Label: "Customer reference", Sort: 120},
	{Key: "notes", DataType: TypeString, Default: Optional, Group: "Reference", Label: "Notes", Sort: 130},

	// Order-only fields. They have no agreement equivalent, which is the point:
	// an agreement is a standing bargain and these describe one journey under
	// it.
	{Key: "additionalNeeds", DataType: TypeList, Default: Optional, Group: "Requirements",
		Label: "Additional needs", Help: "Insurance, tarpaulin, escort.", Sort: 140},
	{Key: "orderSafety", DataType: TypeRef, Default: Hidden, Group: "Requirements",
		Label: "Order safety", Help: "Standard, GPS tracking, security escort, high value.", Sort: 150},
	{Key: "externalId", DataType: TypeString, Default: Hidden, Group: "Reference",
		Label: "External ID", Help: "Your own system's identifier for this order.", Sort: 160},
	{Key: "warehouseLabel", DataType: TypeString, Default: Hidden, Group: "Reference",
		Label: "Warehouse label", Sort: 170},
}

// Catalog is every declared field, indexed by entity and key.
var Catalog = func() map[Entity]map[string]Field {
	out := map[Entity]map[string]Field{
		EntityAgreement: {},
		EntityOrder:     {},
	}
	add := func(entity Entity, fields []Field) {
		for _, f := range fields {
			f.Entity = entity
			out[entity][f.Key] = f
		}
	}
	add(EntityAgreement, agreementFields)
	add(EntityOrder, orderFields)
	return out
}()

// Declared returns every field for an entity, in presentation order.
func Declared(entity Entity) []Field {
	fields := Catalog[entity]
	out := make([]Field, 0, len(fields))
	for _, f := range fields {
		out = append(out, f)
	}
	sortFields(out)
	return out
}

// Entities is every configurable form, for a configurator that lists them.
func Entities() []Entity { return []Entity{EntityAgreement, EntityOrder} }

func sortFields(f []Field) {
	for i := 1; i < len(f); i++ {
		for j := i; j > 0 && f[j].Sort < f[j-1].Sort; j-- {
			f[j], f[j-1] = f[j-1], f[j]
		}
	}
}
