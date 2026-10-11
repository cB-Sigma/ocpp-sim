# Changelog

All notable changes to this module. Versions follow semantic versioning;
before v1 the API may change in minor versions.

## v0.2.0 (2026-10-12)

Needs ocpp v0.2.0.

- `ChargePoint.Connect` opens its WebSocket with the ocpp module's
  `chargepoint.Dial`; the identity is now escaped into the path (it was
  appended as written).
- The test central system (`internal/minicsms`) is built on the ocpp
  module's `csms` package.
- `cmd/ocppsim`'s test compiles the schemas first, so a short run under
  the race detector no longer times out.
- Interop: seanlee0923/ocpp v0.3.0's 2.1 CSMS against the simulator,
  with a SEND (NotifyPeriodicEventStream); the interop module needs Go
  1.26.5.

## v0.1.0 (2026-10-11)

First release as a module of its own, split from the ocpp module: a
simulated charge point of OCPP 1.6J, OCPP-S 1.6, 2.0.1 and 2.1
(`sim.New`, `ChargePoint`) and `cmd/ocppsim`; needs ocpp v0.1.0.

- Fixed: GetConfiguration answers only the asked keys and lists unknown
  ones in `unknownKey`; with no keys it answers every key, sorted.
- Fixed: a 2.x station knows its transaction before its Started event
  goes out, so an immediate RequestStopTransaction is not refused.
- The test central system takes an `OnCall` hook (internal).
- Interop tests against mobilityhouse/ocpp 2.1.0's central system (in
  Docker) and lorenzodonini/ocpp-go v0.19.0's (in process) in a nested
  test-only module, `interop`.
