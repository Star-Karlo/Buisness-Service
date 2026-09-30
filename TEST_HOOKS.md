# Test hooks to remove before real drivers use this

Everything here exists so the order flow can be walked at a desk, without a
truck, a warehouse or a receiving PIC. Each one weakens a check that protects
real deliveries. Find them all with:

    grep -rn "TEST-HOOK" .

Nothing below is needed by the product. When testing ends, delete each item and
the code it guards, then run the tests — several assert the hook's behaviour and
will fail loudly, which is the point.

---

## 1. `X-Status-Bypass` — the state-machine bypass

**Where:** `internal/handlers/handlers.go` (`statusBypassEnabled`,
`statusBypassRequested`), `internal/services/order_service.go`
(`Actor.StatusBypass`).

**What it does:** a request carrying `X-Status-Bypass: 1` may take steps that
belong to another role, and steps past every geofence check — arrival at a
warehouse, starting to load or unload, and the same checks at each stop of a
multi-point journey. It also waives the receiving PIC's code before unloading.

**Who can use it:** anyone whose token the header is accepted for. It is on
unless `STATUS_BYPASS_ENABLED=false`.

**Why it matters:** with it, a caller can mark a delivery as having happened
from anywhere, in any order, without the receiver confirming anything.

**Turn off without a deploy:** set `STATUS_BYPASS_ENABLED=false`.

**To remove:** delete `statusBypassEnabled`, `statusBypassRequested` and the
`StatusBypass` field, then every `actor.StatusBypass` that reads it. Remove the
console's Mode Uji at the same time (see the platform repo's TEST_HOOKS.md) —
it is the only thing that sends the header.

---

## 2. `000000` — the handover code that always works

**Where:** `internal/services/handover_service.go` (`HandoverTestCode`).

**What it does:** the driver app may type `000000` instead of the code the
receiving PIC was sent, and the handover verifies. A live, unexpired code must
still exist for that stop, so it cannot be used before one is issued.

**Why it matters:** the code is the receiver's proof that the goods reached
them. This lets a driver produce that proof without the receiver.

**To remove:** delete the constant and the `|| code != HandoverTestCode` arm of
the comparison in `Verify`.

---

## 3. `WEBFIELD_ENABLED` — not a bypass, but related

**Where:** `internal/services/webfield_flag.go`.

Off by default. It hides the receiving PIC's Web-Field step rather than
weakening a check, and waives the PIC cargo check while hidden. Keep or remove
as a product decision, not a security one — but the waiver in
`internal/services/pod_service.go` goes with it.

---

## Where the other two live

- Console Mode Uji: `karlo_platform/TEST_HOOKS.md`
- Driver app bypass button: `ktrip_flutter` — `kTestingBypass` in
  `lib/tms/trip/screens/trip_screen.dart`. Note it does NOT use
  `X-Status-Bypass`: it sends the warehouse's own coordinates in place of the
  phone's, so a bypassed arrival is recorded as genuinely inside the fence and
  cannot be told apart afterwards. Turning off `STATUS_BYPASS_ENABLED` does
  not disable it.
