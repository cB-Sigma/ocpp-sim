# Provenance

## How the code was written

Written for SigmaM2M, a oneM2M platform, in Go, by the author of the ocpp
module and from the same OCPP documents. No code was ported or copied
from another implementation. The simulator was first written next to
SigmaM2M's OCPP adapter to test it without chargers, moved with the
protocol core into the ocpp module on 2026-10-06, and moved out of it
into this module on 2026-10-11 (with `git mv`), together with
`cmd/ocppsim` and the small test central system `internal/minicsms`, so
the library does not depend on its simulator.

On 2026-10-11 mobilityhouse/ocpp 2.1.0's central system and
lorenzodonini/ocpp-go v0.19.0's 1.6 central system and 2.0.1 CSMS were
run against the simulator (`interop`). Two simulator bugs were fixed the
same day:

- GetConfiguration answered every key whatever the request asked, and
  never `unknownKey`; it now answers the asked keys, lists the unknown
  ones in `unknownKey`, and every key (sorted) when none is asked
  (`TestGetConfigurationAskedKeys`; found while writing the
  mobilityhouse test).
- A 2.x station recorded its transaction only after the CSMS had
  answered the Started event, so a RequestStopTransaction sent as soon
  as the event was read (ocpp-go's CSMS does) was refused; the station
  now knows the transaction it names before the event goes out
  (`TestTransactionKnownWhenStartedIsSent`).

## Sources

| Area (files) | Followed | Licence of the source | Taken |
|---|---|---|---|
| Simulator (sim/) | OCPP 1.6 edition 2, OCPP-J and OCPP-S 1.6, the 1.6 security whitepaper, OCPP 2.0.1 edition 4 and 2.1 edition 2, through the ocpp module | CC BY-ND 4.0 | facts only |
| Command (cmd/ocppsim) | none (flags are our own) | | |
| Test central system (internal/minicsms) | the ocpp module's API | | |
| Interop peer (interop/peer/csms.py) | our own script calling mobilityhouse/ocpp 2.1.0 (MIT), installed from PyPI at test time | MIT | nothing copied |

## Tests

| Test | Source |
|---|---|
| sim/, cmd/ocppsim, example_test.go | the simulator against the test central system on the ocpp module, written from the same documents; not independent |
| interop/ | mobilityhouse/ocpp 2.1.0's central system (1.6, 2.0.1, 2.1) and ocpp-go v0.19.0's central system (1.6) and CSMS (2.0.1) against the simulator |

All charger identities, id tags and keys are made-up demo values.
