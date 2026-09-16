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
full mapping is in [`../docs/PERMISSIONS.md`](../docs/PERMISSIONS.md) §5.

Transitions are **compare-and-set**: the update applies only if the order is
still in the expected state, so two managers pressing approve at once cannot
both succeed. An integration test asserts exactly one winner out of twenty.

Refused here where the monolith allowed it: a driver approving an order, a
shipper assigning a driver, an order skipping draft to completed, a transporter
marking their own invoice paid.

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
validation are read from their owner every time. See `../docs/CACHING.md` for the
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
