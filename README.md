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

Transitions are **compare-and-set**: the update applies only if the order is
still in the expected state, so two managers pressing approve at once cannot
both succeed. An integration test asserts exactly one winner out of twenty.

Refused here where the monolith allowed it: a driver approving an order, a
shipper assigning a driver, an order skipping draft to completed, a transporter
marking their own invoice paid.

## Money is exact

Amounts are `decimal.Decimal`, never `float64`. PPN is *added* and PPH23 is
*withheld*; both are computed in one place with the signs pinned by test.


## Layout

```
cmd/server/           entrypoint
internal/
  config/             environment loading; no defaults for security controls
  models/             domain types
  repository/         the only code that talks to the database
  services/           business rules
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
| `make test-integration` | integration tests (needs a database; see above) |
| `make lint` | golangci-lint |
| `make proto` | regenerate the gRPC bindings |
| `make swagger` | regenerate the OpenAPI document |
| `make docker` | build the container image |

## API documentation

`make run`, then open **http://localhost:5003/swagger/index.html**.

The browser is served only outside production: the document describes every
endpoint and response shape, which is the reconnaissance an attacker would
otherwise have to guess at. A test asserts it 404s when `ENVIRONMENT=production`.
