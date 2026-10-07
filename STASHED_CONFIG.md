# Stashed per-company configuration

Customisations switched **off** by a business decision, kept here so they can
be switched back on without rediscovering what they were. Nothing is deleted:
every field below still exists in `internal/fieldconfig/catalog.go`, and every
form still renders it when the field is enabled.

A company's answers live in `company_field_config`. Changing one is a
configuration change, not a deploy.

---

## MAST — warehouse lanes on Single Shipment and Multi Shipment

**Stashed 7 Oct 2026.** MAST's ordinary contracts go back to the general flow:
lanes named by city, as every other company writes them. Multi Customer is
**not** affected and keeps its warehouse lanes, its allowance and its billing
split.

| Field | Was | Now |
|---|---|---|
| `lanes.loadingPoints` | `optional` | `hidden` |
| `lanes.unloadingPoints` | `optional` | `hidden` |

Left enabled, because Multi Customer depends on them:

| Field | State |
|---|---|
| `multiCustomers` | `optional` |
| `billingSplit` | `optional` |
| `allowance.upfrontPercent` | `optional` |

Company: **MAST**, `36780ff5-257c-4dc9-8e20-3556ac6643b6`.

### To restore

Set both `lanes.*` fields back to `optional` for that company — in the console
at **/t/form-config** as a MAST administrator (entity *Agreement*), or as Karlo
staff with `X-Acting-For` set to the company id. No deploy, no migration.

### What the switch actually changes

The agreement form asks `isEnabled(agreementFields, 'lanes.loadingPoints')` to
decide whether a contract's lanes are warehouses or cities. With it off, Single
Shipment and Multi Shipment show the city Initial/Destination Route fields, and
their payload carries `initialRoutes`/`destinationRoutes` instead of
`loadingPoints`/`unloadingPoints`.

Multi Customer is deliberately independent of it. Each customer names its own
two warehouses whatever the company's usual style, so the form tests
`lanesAreWarehouses` — the company flag **or** a multi-customer contract —
wherever the answer means "these lanes are warehouses". Reading the company
flag alone dropped Customer 1's warehouses from the payload of every
multi-customer contract written by a company that prices its ordinary ones city
to city.

### Why hiding the field does not refuse a Multi Customer contract

`hidden` REFUSES a field that is sent anyway, and a multi-customer contract
DOES send `loadingPoints` — Customer 1's own lanes live there. Hiding the field
would therefore have refused every multi-customer contract the moment it was
stashed.

So the server no longer counts those warehouses towards `lanes.*` when the
contract is multi-customer: such a contract names warehouses because of what it
IS, not because the company chose to price lanes that way. The rule is the same
one the form applies, on both sides. `TestHiddenWarehouseLanesRefuseOnlyOrdinaryContracts`
holds it: with the option hidden, a multi-customer contract carrying warehouses
is accepted and an ordinary one is refused.

### Worth knowing before restoring or re-stashing

- Agreements already written keep whatever they stored. A MAST single-shipment
  contract written while this was on still holds `detail.loadingPoints`; the
  form will not send those on a renewal while the field is hidden, so the
  renewal records city routes instead. That is the point of the stash, but it
  is a one-way door for that version — the previous version keeps its lanes.
- A third-party client posting `loadingPoints` on an ORDINARY contract while
  the option is hidden will be refused, which is the intent: the console stops
  sending them, so only something bypassing it would hit this.
