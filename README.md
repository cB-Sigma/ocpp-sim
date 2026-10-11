# ocpp-sim

A simulated OCPP charge point (1.6J, OCPP-S 1.6, 2.0.1 and 2.1) for testing central systems without a charger; not affiliated with the Open Charge Alliance.

[![Go Reference](https://pkg.go.dev/badge/github.com/cB-Sigma/ocpp-sim.svg)](https://pkg.go.dev/github.com/cB-Sigma/ocpp-sim) [![test](https://github.com/cB-Sigma/ocpp-sim/actions/workflows/test.yml/badge.svg)](https://github.com/cB-Sigma/ocpp-sim/actions/workflows/test.yml) [![Licence: Apache-2.0](https://img.shields.io/badge/licence-Apache--2.0-blue.svg)](LICENSE) [![Go](https://img.shields.io/badge/go-1.26.0-00ADD8.svg)](go.mod)

One `ChargePoint` connects to a central system in the version you
choose, boots, reports its connectors, authorizes, runs sessions with
meter values and answers the central system's commands. Every message it
sends and receives is checked against the Open Charge Alliance's JSON
schemas, and what does not pass is recorded (`Violations`), so a test
notices a central system that drifts from them. It is built on
[ocpp](https://github.com/cB-Sigma/ocpp). Written for SigmaM2M, a oneM2M
platform, whose CSMS tests run against it; it has no dependency on it.

## What is simulated

| Feature | Status |
|---|---|
| OCPP-J 1.6, 2.0.1 and 2.1; OCPP-S 1.6 (strict about the WSDLs unless `SOAPLenient`) | yes |
| Boot, status of each connector, heartbeat, authorize | yes |
| Sessions with meter values: StartTransaction, MeterValues, StopTransaction (1.6); TransactionEvent Started, Updated, Ended (2.x) | yes |
| Answers to remote start and stop, reset, availability, configuration (1.6, the asked keys and `unknownKey`) and variables (2.x), triggers, smart charging profiles and composite schedules, firmware updates with status notifications, diagnostics and logs, reservations, local lists, certificates, device model reports | yes |
| 2.1 SEND and periodic event streams; signed requests (taken as is, not verified) | yes |
| Basic authentication (security profiles 1 and 2), TLS with a `tls.Config` (profiles 2 and 3) | yes |
| Fault switches: answer an action with a chosen status (`Answer`), firmware update failures, going offline (`Disconnect`) | yes |
| Schema check of every message in and out, recorded in `Violations` | yes |
| A real charger's timing, an EV's charging curve, ISO 15118 | no |

## Install

```
go get github.com/cB-Sigma/ocpp-sim@latest
```

Requires Go 1.26.0 or later. The command:

```
go install github.com/cB-Sigma/ocpp-sim/cmd/ocppsim@latest
```

## Command line

```
ocppsim -url ws://127.0.0.1:8887/ocpp/ -id CP1 -version 2.1 -session
ocppsim -soap -url http://127.0.0.1:8887/ocpp16s/ -id CP1 -version 1.6
```

| Flag | Default | Meaning |
|---|---|---|
| `-url` | `ws://127.0.0.1:8887/ocpp/` | central system endpoint without the identity |
| `-id` | `SIM1` | charge point identity |
| `-version` | `1.6` | 1.6, 2.0.1 or 2.1 |
| `-password` | | basic authentication password (security profiles 1 and 2) |
| `-insecure` | false | do not verify the central system's TLS certificate (test setups) |
| `-connectors` | 2 | connectors (EVSEs in 2.x) |
| `-session` | false | run one charging session after booting |
| `-tag` | `DEMO0001` | id tag of the session |
| `-heartbeat` | the central system's interval | heartbeat interval |
| `-for` | until interrupted | stop after this long |
| `-soap` | false | speak OCPP-S 1.6; `-url` is the Central System service URL |
| `-soap-listen` | `127.0.0.1:0` | OCPP-S: address of the charger's Charge Point service |
| `-soap-from` | `http://<soap-listen>/` | OCPP-S: the address given in From |
| `-soap-lenient` | false | OCPP-S: take messages that do not follow the WSDLs |

## In Go tests

```go
cp := sim.New(sim.Config{URL: "ws://127.0.0.1:8887/ocpp/", ID: "CP1", Version: ocpp.V201})
_ = cp.Connect(ctx)
defer cp.Disconnect()
status, _ := cp.Boot(ctx)                     // "Accepted"
tx, _, _ := cp.Start(ctx, 1, "TAG1", 1000)    // a session on EVSE 1
_ = cp.Meter(ctx, 1, 1500, 7400, [3]float64{230, 230, 230})
_ = cp.Stop(ctx, 1, 2000, "Local")
r, _ := cp.Wait(ctx, "RequestStartTransaction") // a command from the central system
for _, v := range cp.Violations() { t.Error(v) }  // messages outside the schemas
```

`Example` in example_test.go runs the simulator against a small central
system built from the ocpp module alone.

## Documentation

Reference: https://pkg.go.dev/github.com/cB-Sigma/ocpp-sim

| Package | What it does |
|---|---|
| `sim` | `New(Config)`, `ChargePoint` (`Connect`, `Disconnect`, `Boot`, `Heartbeat`, `Status`, `StatusAt`, `Authorize`, `Start`, `Meter`, `Stop`, `Transaction`, `Call`, `Send`, `Raw`, `Answer`, `Received`, `Wait`, `Violations`, `SignCertificate`, `NotifyEvent`, `OpenStream`, `StreamValues`, firmware and reservation helpers) |
| `cmd/ocppsim` | runs one charge point of any version, OCPP-J or OCPP-S |
| `internal/minicsms` | the small central system, on the ocpp module's `csms` package, that the tests run the simulator against |
| `interop` (nested module, tests only) | mobilityhouse/ocpp's central system (in a container), ocpp-go's and seanlee0923/ocpp's (in process) against the simulator |

Changes per version are in [CHANGELOG.md](CHANGELOG.md).

## Testing

```
go test -race ./...
cd interop && go test -v ./...                          # ocpp-go and seanlee0923/ocpp, in process (Go 1.26.5)
cd interop && SIGMA_OCPP_INTEROP=1 go test -v ./...     # and mobilityhouse/ocpp, needs Docker
```

Tests carry `// Covers: SIG-...` comments: requirement IDs of the
SigmaM2M project's compliance report, with no meaning outside it; the
tests added for the first release carry none.

### Interop tested against

- mobilityhouse/ocpp 2.1.0's central system (2026-10-11,
  python:3.12.10-slim in Docker Desktop 29.4.3 on arm64; `interop`): for
  1.6, 2.0.1 and 2.1 the simulator booted, sent a heartbeat, authorized,
  ran a session with meter values (StartTransaction, MeterValues,
  StopTransaction; TransactionEvent Started, Updated, Ended), all of
  which mobilityhouse's schema check accepted; it answered
  RemoteStartTransaction and RemoteStopTransaction (1.6) or
  RequestStartTransaction and RequestStopTransaction (2.x) and started
  and stopped the session asked for, then Reset, GetConfiguration with a
  key list (1.6) or SetVariables and GetVariables (2.x), each answer
  accepted by mobilityhouse's schema check. A StatusNotification with a
  string connector, sent on purpose, came back as TypeConstraintViolation
  and the connection carried on; a Reset outside the enumeration, sent
  by mobilityhouse past its own check, was recorded in `Violations`.
- lorenzodonini/ocpp-go v0.19.0's 1.6 central system and 2.0.1 CSMS
  (2026-10-11, in process; `interop`): the same session and commands,
  read by ocpp-go's handlers with the values sent; no message of
  ocpp-go's fell outside the OCA schemas. ocpp-go answers the
  deliberately invalid StatusNotification with FormationViolation (1.6)
  and FormatViolation (2.0.1). Its 2.0.1 CSMS sends
  RequestStopTransaction as soon as it has read a Started event, which
  found a simulator bug (below).
- Bugs found and fixed for v0.1.0: GetConfiguration with keys answered
  every key and no `unknownKey` (found while writing the mobilityhouse
  test; `TestGetConfigurationAskedKeys`); a 2.x station recorded its
  transaction only after the Started event was answered, so it refused
  a RequestStopTransaction that came at once
  (`TestTransactionKnownWhenStartedIsSent`).
- SigmaM2M's CSMS is tested against this simulator for every version,
  OCPP-S included, and runs the OCA part 6 CSMS test cases with it.
- seanlee0923/ocpp v0.3.0's 2.1 CSMS (2026-10-12, in process;
  `interop`): the simulator booted, reported both connectors, sent a
  heartbeat and a SEND (NotifyPeriodicEventStream, three values) that
  seanlee's typed SEND handler decoded, and answered its Reset; no
  message of seanlee's fell outside the OCA schemas. Passed.
- Not yet: OCPP-S against another central system (SteVe speaks it; not
  run yet), security profiles 2 and 3 against another implementation.

## Demo values

Every charger identity, id tag, certificate and key in this module is
made up or generated when the tests run. Do not use them for real
chargers.

## Related repositories

- [ocpp](https://github.com/cB-Sigma/ocpp): the OCPP protocol core this
  simulator is built on.

## Status

v0.x: the API may change before v1.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Security

See [SECURITY.md](SECURITY.md).

## Licence

Apache-2.0 (see [LICENSE](LICENSE) and [NOTICE](NOTICE)). Copyright 2026
Abhinav Gautam. OCPP and Open Charge Alliance are names of the Open
Charge Alliance; they are used only to say which protocol this module
simulates. This module is not made, endorsed, certified or supported by
the Open Charge Alliance.
