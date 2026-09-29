# Karlo Business Service

**HTTP** `:5003` · **gRPC** `:6003` · **Store** PostgreSQL

The transactional core: agreements, orders, the shipment lifecycle and invoices.

This is where the monolith was weakest — 183 routes resolving to 28 handlers, 46
of them sharing one generic update that wrote whatever the request body
contained. Here every path does one thing, and status changes go through state
machines rather than accepting an arbitrary status string.

## Getting started

```bash
cp .env.example .env          # set DB_PASSWORD, SERVICE_TOKEN, ACCEPTED_SERVICE_TOKENS
make migrate-up
make run
```

Needs the authentication, master data and notification services reachable at the
`*_GRPC_ADDR` addresses, and `JWT_PUBLIC_KEY_FILE` pointing at the
authentication service's public key.

## State machines, not status strings

`PUT /orders/:id/status` takes a destination; the machine in
[`internal/models/status.go`](internal/models/status.go) decides whether it is
legal and whether this role may do it. `GET /orders/:id/transitions` returns the
moves available to *this* caller, so a client renders only buttons that work.

The **permission** is checked in the handler rather than on the route, because
the target status arrives in the request body — one URL covers transitions as
different as submitting an order and cancelling it, and a route guard is fixed
when the route is registered. `PermissionForOrderStatus` and
`PermissionForShipmentStatus` map each target status to the key it needs, and a
status with no entry is refused, so adding one without deciding who may reach
it fails closed. The role rules still apply on top; a caller needs both. The
full mapping is in [`../docs/shared/PERMISSIONS.md`](../docs/shared/PERMISSIONS.md) §5.

Transitions are **compare-and-set**: the update applies only if the order is
still in the expected state, so two managers pressing approve at once cannot
both succeed. An integration test asserts exactly one winner out of twenty.

Refused here where the monolith allowed it: a driver approving an order, a
shipper assigning a driver, an order skipping draft to completed, a transporter
marking their own invoice paid.

## Single-shipment and multi-shipment are two flows

One machine, two flows over it. A single-shipment order has one loading point,
one unloading point, one set of documents; a multi-shipment order pairs its
points — `detail.loadingPoints[k]` with `detail.unloadingPoints[k]` is Shipment
*k+1* — and each pair carries its own cargo and its own paperwork. `order_stops`
holds one row per point with the `shipment_no` that pairs it, `seq` follows the
planner's visit order (`detail.stopSequence`, written by
`PUT /orders/:id/stop-sequence` and its **only** home — migration 000020 dropped
the `orders.stop_sequence` column 000019 had added for it, unpopulated, because
two homes for one value is how an order comes to say one thing while the driver
is sent somewhere else; default every Muat then every Bongkar, a shipment's
Bongkar may never precede its own Muat, and a rebuild stops at 200 points so the
numbering keeps fitting its `SMALLINT`), a POD belongs to a stop, and
`loaded` / `unloaded` are reached only when every stop of that kind is finished.
`OrderFlow` reads which flow an order runs from the shape of the order rather
than a stored flag, so a single-shipment order keeps the untouched two-ended
path.

**The unloading handover is per point too** (000021). `shipment_handovers.stop_id`
names the visit a code hands over, and NULL means the delivery as a whole — the
two-ended journey's handover and every row written before this. Issue and verify
take an optional `stopId`; the PIC the courtesy message goes to is that stop's,
then the order's, then the site's default, because telling Semarang's receiver
the Priok code hands the wrong person a code that opens someone else's handover.
Starting an unloading stop needs that stop's code confirmed, except the
journey's **first** unloading point, which the shipment's stage-level code covers
(`assertStopHandover`, `internal/services/stop_visits.go`); loading stops are
never gated. Each stop reports `handoverVerified` / `handoverVerifiedAt` on the
shipment read, always false on a loading point.

**A line of cargo is numbered too** (000022). `order_items.shipment_no`
(`SMALLINT NOT NULL DEFAULT 1`, indexed `(order_id, shipment_no)`) pairs items
the way stops are paired, because **a stop's plan is its own shipment's items**:
checked against the whole order's weight instead, unloading 2 000 of 4 500 kg at
Semarang reads as 2 500 kg missing rather than as Shipment 1 delivered in full.
`shipmentNo` rides on `orderItemRequest` → `OrderItemInput` → `models.OrderItem`,
absent or below 1 meaning the first. There is **no grouping helper here**:
`ItemsByShipment` was written for *Detail Muatan* and the per-stop E-POD plan,
those screens group in the browser instead, and a rule with two implementations
drifts — so it was dropped (32f2f08) rather than left as a second answer to
"what is a shipment's plan". The column on the item stays; that is the pairing,
and the create path writes it. Note that the console's Internal Order wizard
does **not** populate it: `items[]` at the top level of a create is the
per-company *Itemised cargo* field, off for most companies, so the wizard sends
its lines on `detail.items` only — see
`karlo_platform/REMAINING_WORK.md` §*Data shapes worth knowing*.

**A stop list that the driver has begun is history, not a plan** (d565efc).
`SyncStops` rebuilds the list on every write touching the order's detail, and
`Replace` deletes every row and inserts new ones with new ids — mid-delivery
that discarded each visit's arrival, start and cargo check and, because
`shipment_pods.stop_id` and `shipment_handovers.stop_id` are `SET NULL`,
detached the signed paperwork and the receiver's confirmed code from the visit
they belong to. Two guards at that one seam: `sameJourney` compares the stored
rows against `stopsForOrder` on `seq`, `kind`, `shipment_no` and `warehouse_id`
— what the planner chose, not the recorded progress — and skips the rebuild
when nothing about the journey has changed; and once `firstVisited` finds a stop
with an `arrived_at`, `started_at` or `finished_at`, a list that *would* change
is refused, naming the stop (*perjalanan sudah dimulai di …*).

**Both are the general flow**, and neither belongs to a customer. Per-customer
differences are differences of *data* — which agreement, which fields an order
must carry, what a document is called — attaching on top of whichever flow the
order runs. Nothing here branches on a company. The whole of it is in
[`../docs/business/MODEL.md`](../docs/business/MODEL.md) §`order_stops` and
§*Two flows, both general*; the driver's side is
[`../docs/shared/KTRIP_FLOW.md`](../docs/shared/KTRIP_FLOW.md).

## The shipment's status follows the stops

A two-ended trip's statuses and its two visits are the same events, so the
driver app drives the statuses directly. A journey through several points cannot
work that way: with the visit order Muat 1 → Bongkar 1 → Muat 2 → Bongkar 2 the
truck is unloading at Semarang while a loading point is still outstanding, and
**no single shipment status is true of the point being worked**. The stop rows
are. So the stop events lead and the status is walked along behind them
(fb48641, `internal/services/stop_visits.go`):

- **the POD gates on the STOP**, on a journey with more than two of them.
  `PodService.Submit` resolves the visit first and then requires that work has
  begun there (`started_at`) and that it is not already closed (`finished_at`);
  the *sesuai / tidak sesuai* check comes from `stop.cargo_checked_at` rather
  than the shipment's. A two-ended journey keeps the shipment-level check it
  always had — the POD must agree with `loading` / `unloading`.
- **starting a stop walks the shipment** to the status that describes it —
  `load` ⇒ `loading`, `unload` ⇒ `unloading` — through `walkShipmentTo`, which
  takes one permitted step at a time because the table allows no jumps
  (`toUnloading` cannot reach `unloading` without passing `atUnloading`). A
  shipment already at or past the target is left alone, so a later visit never
  drags it back. A walk that will not move is logged and does not stop the
  driver working; the walk taken by an approval is returned, because that is
  what ends the journey.
- **the walk carries the role of whoever caused it, and their position.** The
  middle of the chain — `loaded → toUnloading → atUnloading → unloading` — is
  the driver's, so a stop event walks as the driver and the geofence check runs;
  without the position forwarded from the stop call the walk was refused for
  having no GPS at a gate the driver was standing at (93fa3e9). An approval
  walks as the system, from a desk, with no position.
- **arrival at unloading is judged against the stop being worked.**
  `assertAtSite` used `orders.destination_warehouse_id`, which is the **last**
  drop-off, so a driver reporting at the first was refused for being exactly
  where they should be. It now prefers `firstUnfinishedStop` of that kind.
- `internal/models/status.go` supplies the ordering the walk needs:
  `shipmentChain` (the order the statuses happen in; `cancelled` is absent — it
  is an end, not a position), `ShipmentStatusAtOrPast` and
  `NextShipmentStatusTowards`, which skips the legacy `loadingApproved` /
  `unloadingApproved` states rather than parking a shipment in one.
  `internal/models/shipment_chain_test.go` pins them, including that every step
  of the chain is walkable by the driver or the system.

**`loaded → toUnloading` is open to the system as well as the driver** (93fa3e9).
The status sheet triggers *Menuju titik bongkar* from the planner's approval of
the loading POD, not from the driver leaving the yard, and approving that POD is
two steps as the system: `loading → loaded`, then `loaded → toUnloading`. The
second was the driver's alone, so **every** approval — two-ended orders as much
as multi-stop ones — marked the POD and then failed on that step, leaving the
shipment parked on `loaded`. The truck's own movements stay the driver's: the
system still cannot say a driver arrived somewhere or began unloading.

**Correct, and it looks wrong:** on an interleaved journey the shipment reads
`loading` while the driver unloads at the first drop-off. The loading stage is
not over until every loading stop's POD is approved, and the driver-role walk
cannot take `loading → loaded` — that step belongs to the approval — so the walk
stops there and logs it. The stop rows carry what is actually happening, and
that is what the console, the driver app and the POD gate read.

## Money is exact

Amounts are `decimal.Decimal`, never `float64`. PPN is *added* and PPH23 is
*withheld*; both are computed in one place with the signs pinned by test.


## Road routing goes through MAPID, from the server

`POST /api/v1/routing/route` plans a road route between two or more points. It
replaces the Mapbox call the monolith made from the browser, and the reason it
is a backend client at all is not negotiable: **the request carries an API key,
and a key in a public bundle belongs to whoever finds it.**

MAPID wraps GraphHopper, so the request and response shapes are GraphHopper's
and its documentation is the one that applies. Several details were established
by probing the live service rather than read from a document:

- The endpoint is the **root path** — `POST https://routing.mapid.io/?key=…`. `/v4/route` 404s, and GET is not supported.
- Profiles are exactly `car`, `truck`, `motorcycle`, `foot`. **`small_truck` does not exist** despite appearing in GraphHopper's own docs, so the client refuses unknown profiles by name rather than passing them through.
- `points` are `[longitude, latitude]` — GeoJSON order.
- Toll segments come back as `"all"` or `"missing"`. **`"missing"` is not `"free"`**; it is the provider having no data, and treating it as free underprices the journey.
- Avoiding tolls needs `ch.disable: true` plus a custom model, which invalidates the precomputed shortcuts and is measurably slower. On Semarang → Surabaya: 342.5 km / 281 min with tolls, 309.6 km / 301 min avoiding them — shorter and slower, which makes it a commercial decision rather than a better route.
- MAPID returns intermittent 502s. The client makes **at most two attempts**, because a route request reads a road network and writes nothing, so a retry cannot double anything. A 4xx returns immediately.
- **Fares come from a second API**, `POST /v2/route_n_toll/rute` with the routed `LineString` (`Client.TollFor`): prices per golongan I–V and the gates passed. The cache calls it once per freshly computed tolled route and stores the answer in `route_cache.toll` (migration 000011), so a hit never pays for it again; a failed lookup leaves `toll` NULL, which reads as *unknown*, never free. `Cache.Reprice` prices older cached legs on their next read. The allowance evidence sums the gate tariff for the assigned truck's golongan (from its truck type in master data) and reports `tollEstimateSource: "tariff" | "rate" | "none"`, `tollGolongan` and `tollPrices`; the per-km `TollRatePerKm` (1,350) is only the fallback for a tolled leg with no stored fare.

`truck` is the default profile. Planning a lorry's journey on the car profile
produces a route the driver cannot legally take, and the difference is not
visible on a map.

The route is guarded by `order.read` rather than a routing key of its own: a
key everybody would have to hold in order to plan a journey is a key that says
nothing. Configuration is `MAPID_BASE_URL` and `MAPID_KEY`; neither is
required, and an unset key leaves the endpoint answering *"Routing is not
configured on this deployment"* rather than the service refusing to start.

## Orders carry resolved names, at one lookup per distinct id

Order payloads include `originWarehouseName`, `destinationWarehouseName` and
`truckPoliceNumber`, resolved from master data over gRPC on both the list and
the detail path — the same order must not read differently in a table and on
its own page.

Resolution is deduplicated per page: one lookup per **distinct** id, so two
orders running between the same pair of warehouses cost two lookups rather than
four. A failure to resolve is logged and leaves the field blank rather than
failing the read; a name is a convenience, and losing the whole page of orders
because master data is briefly unreachable is the worse outcome.

## Company settings are cached, orders are not

The billing company's settings — tax rates, `finishWithGeofencing`,
`activeAgreementVerifiedOnly` — are read on every order creation, every geofenced
arrival and every invoice, and change perhaps twice a year. Uncached that puts a
cross-service gRPC call on the critical path of the busiest write in the system,
so they are held for **5 minutes**.

Five minutes rather than the catalogues' fifteen because these values decide what
a customer is charged, and a stale PPN rate produces an invoice that is wrong in
a way nobody notices until reconciliation.

Nothing authoritative is cached: order status, invoice totals and reference
validation are read from their owner every time. See `../docs/shared/CACHING.md` for the
full list of what is deliberately left out and why.

## Layout

```
cmd/server/           entrypoint: serve | migrate | archive [--dry-run] | restore <entity> <id>
internal/
  archive/            cold storage: what leaves for Glacier, when, and how it comes back
  config/             environment loading; no defaults for security controls
  models/             domain types
  repository/         the only code that talks to the database
  services/           business rules, incl. the geofence watcher that polls FMS positions
                      (TELEMETRY_BASE_URL — http://tracking.karlo.internal:5006 in prod,
                      FMS's ingest in the same VPC since 2026-09-15)
  storage/            S3: presigned uploads, and the Deep Archive put/get the archiver uses
  telemetry/          FMS tracking client (positions by IMEI)
  routing/            MAPID client behind the Postgres route cache
  handlers/           HTTP
  grpcserver/         gRPC contract implementation
  clients/            outbound gRPC to other services
  routes/             the HTTP surface, one handler per path
  platform/           shared plumbing, vendored (see below)
proto/                gRPC contracts
docs/                 generated OpenAPI document
tests/unit/           no I/O; run always
tests/integration/    real database; build-tagged
```

## Platform documentation

The cross-service documentation — data ownership, business flows, testing and
observability — is **not in this repository**. It describes all four services,
so it lives once in the workspace that holds them side by side, at
`../docs/`, rather than in four drifting copies.

This README covers what is specific to this service.

## About `internal/platform`

This directory is **vendored, not authored here**. It holds the plumbing every
Karlo service shares: RS256 token verification, the structured logger, the
gRPC server and client setup, the query allowlist, and the HTTP response
envelope.

Each service repo carries its own copy so it is fully standalone. The cost is
that a change to shared plumbing — a fix to token verification, say — has to be
applied to all four repos. **`internal/platform/authctx` is security-critical:
a change there must land everywhere.**

The same applies to `proto/`. The contracts are duplicated by design; when one
changes, copy the updated `.proto` into every repo that speaks it and run
`make proto` there. CI fails if the committed bindings do not match the
contracts in the repo, which catches a forgotten regeneration but not a
forgotten copy.

## Commands

| | |
|---|---|
| `make tools` | install buf, the protoc plugins, swag and golangci-lint |
| `make run` | run the service |
| `make test` | unit tests |
| `make test-integration` | integration tests (needs `karlo_business_test`; see below) |
| `make lint` | golangci-lint |
| `make proto` | regenerate the gRPC bindings |
| `make swagger` | regenerate the OpenAPI document |
| `make docker` | build the container image |
| `go run ./cmd/server archive --dry-run` | list what the nightly archiver would move to Glacier; `archive` without the flag does it, `restore order <uuid>` brings one back — see `../docs/business/COLD_STORAGE.md` |

## The integration suite refuses a database that is not a test database

`BUSINESS_TEST_DSN` must name a database containing `_test`, or `testDB` fails
the run before opening a connection. The suite `TRUNCATE`s orders and
agreements, and pointing it at the local development database wiped the seed
once. It is a hard failure rather than a skip, because a skip is how you end up
believing a destructive suite ran. The DSN is redacted before it reaches the
message. `make test-integration-up` creates `karlo_business_test`.

## API documentation

`make run`, then open **http://localhost:5003/swagger/index.html**.

The browser is served only outside production: the document describes every
endpoint and response shape, which is the reconnaissance an attacker would
otherwise have to guess at. A test asserts it 404s when `ENVIRONMENT=production`.

## Production mode and proxies

`IsProduction()` is true for `ENVIRONMENT=production` **or** `ENVIRONMENT=prod`,
case-insensitive. Terraform passes its `environment` variable through as
`prod`, and while only the long form was accepted the deployed task matched
neither — Swagger served, gRPC reflection on, Gin in debug mode, every SQL
statement logged. `internal/config/config_test.go` pins both spellings.

`TRUSTED_PROXIES` is an optional comma-separated list of CIDRs handed to Gin's
`SetTrustedProxies`. Behind the load balancer every request arrives from a VPC
address with the real client in `X-Forwarded-For`, and Gin believes that header
from anyone by default, which lets a caller pick the IP the audit log records.
Terraform sets it from the platform's `trusted_proxy_cidrs` output; unset
locally, Gin's default (trust every proxy) applies and nothing changes.

## CI and deploy

`.github/workflows/ci.yml` runs on every pull request and push: `build`
(build, vet, `go test -race`, golangci-lint, then `govulncheck ./...`),
`contracts`, `integration`, `swagger` and `docker`. `govulncheck` fails the
build on a vulnerability the binary can actually reach; `go.mod` is on
`1.25.14` because that is where the last batch was cleared.

`.github/workflows/deploy.yml` runs only after CI completes successfully on
`main` (`workflow_run`), or by hand (`workflow_dispatch`), and checks out the
commit CI passed (`workflow_run.head_sha`) rather than the tip of `main`. It
used to fire on the push itself, alongside CI, so a red build did not stop a
deploy.

`terraform/alarms.tf` adds three CloudWatch alarms — no healthy target for two
minutes, more than 20 target 5xx in five minutes, CPU above 85% for fifteen
minutes — publishing to the platform's `karlo-<env>-alerts` topic through
`try(local.platform.alerts_topic_arn, "")`. Applied against a platform state
older than the topic, they exist but tell nobody; re-apply after the platform.
