package services

import (
	masterdatav1 "github.com/karlo/business-service/internal/platform/genproto/karlo/masterdata/v1"
)

// catalogRefs builds the master data references an order carries, skipping the
// ones that were not supplied. It exists so the validation call site stays
// readable as the number of referenced catalogues grows.
func catalogRefs(cargoTypeID, itemTypeID string) []*masterdatav1.CatalogRef {
	var refs []*masterdatav1.CatalogRef

	add := func(kind masterdatav1.CatalogKind, id string) {
		if id != "" {
			refs = append(refs, &masterdatav1.CatalogRef{Kind: kind, Id: id})
		}
	}

	add(masterdatav1.CatalogKind_CATALOG_KIND_CARGO_TYPE, cargoTypeID)
	add(masterdatav1.CatalogKind_CATALOG_KIND_ITEM_TYPE, itemTypeID)

	return refs
}
