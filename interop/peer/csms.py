# Copyright 2026 Abhinav Gautam
# SPDX-License-Identifier: Apache-2.0
#
# A central system on mobilityhouse/ocpp (MIT) for OCPP 1.6, 2.0.1 and 2.1,
# for the interop tests. It answers the charge point's requests, prints
# "PEER <event> <json>" lines, and sends the CALLs it reads on standard
# input:
#
#   call <Action> <json payload, camelCase>      validated by mobilityhouse
#   rawcall <Action> <json payload, camelCase>   sent without its validation
#   quit
#
# mobilityhouse/ocpp checks every message in and out against the JSON
# schemas it ships for each version.

import asyncio
import dataclasses
import datetime
import json
import sys

import websockets

from ocpp.charge_point import camel_to_snake_case, snake_to_camel_case
from ocpp.exceptions import OCPPError
from ocpp.routing import on
from ocpp.v16 import ChargePoint as CP16
from ocpp.v16 import call as call16
from ocpp.v16 import call_result as result16
from ocpp.v201 import ChargePoint as CP201
from ocpp.v201 import call as call201
from ocpp.v201 import call_result as result201
from ocpp.v21 import ChargePoint as CP21
from ocpp.v21 import call as call21
from ocpp.v21 import call_result as result21


def emit(name, obj):
    print("PEER", name, json.dumps(obj, default=str), flush=True)


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


TX = [41]


def next_tx():
    TX[0] += 1
    return TX[0]


def answer16(action, kw):
    if action == "BootNotification":
        return dict(current_time=now(), interval=300, status="Accepted")
    if action == "Heartbeat":
        return dict(current_time=now())
    if action == "Authorize":
        return dict(id_tag_info={"status": "Accepted"})
    if action == "StartTransaction":
        return dict(transaction_id=next_tx(), id_tag_info={"status": "Accepted"})
    if action == "DataTransfer":
        return dict(status="UnknownVendorId")
    return {}


def answer2x(action, kw):
    if action == "BootNotification":
        return dict(current_time=now(), interval=300, status="Accepted")
    if action == "Heartbeat":
        return dict(current_time=now())
    if action == "Authorize":
        return dict(id_token_info={"status": "Accepted"})
    if action == "TransactionEvent" and kw.get("id_token"):
        return dict(id_token_info={"status": "Accepted"})
    if action == "DataTransfer":
        return dict(status="UnknownVendorId")
    return {}


ACTIONS16 = ["BootNotification", "Heartbeat", "Authorize", "StartTransaction", "StopTransaction",
             "MeterValues", "StatusNotification", "FirmwareStatusNotification",
             "DiagnosticsStatusNotification", "DataTransfer"]
ACTIONS2X = ["BootNotification", "Heartbeat", "Authorize", "TransactionEvent", "MeterValues",
             "StatusNotification", "FirmwareStatusNotification", "LogStatusNotification",
             "NotifyEvent", "NotifyReport", "SecurityEventNotification", "DataTransfer"]


def handler(action, answer):
    async def h(self, **kw):
        emit("recv", {"action": action, "payload": snake_to_camel_case(kw)})
        return getattr(self.results, action)(**answer(action, kw))
    # mobilityhouse routes by the function's name, so each needs its own.
    h.__name__ = "on_" + action
    return on(action)(h)


def central(base, calls, results, actions, answer):
    attrs = {"calls": calls, "results": results}
    for a in actions:
        attrs["on_" + a] = handler(a, answer)
    return type("Central" + base.__module__.replace(".", "_"), (base,), attrs)


CLASSES = {
    "ocpp1.6": central(CP16, call16, result16, ACTIONS16, answer16),
    "ocpp2.0.1": central(CP201, call201, result201, ACTIONS2X, answer2x),
    "ocpp2.1": central(CP21, call21, result21, ACTIONS2X, answer2x),
}

CURRENT = {}


def plain(obj):
    if dataclasses.is_dataclass(obj):
        obj = dataclasses.asdict(obj)
    if isinstance(obj, dict):
        return {k: plain(v) for k, v in obj.items() if v is not None}
    if isinstance(obj, list):
        return [plain(v) for v in obj]
    return obj


async def on_connect(ws):
    cp_id = ws.request.path.rstrip("/").split("/")[-1]
    proto = ws.subprotocol
    cp = CLASSES[proto](cp_id, ws)
    CURRENT["cp"] = cp
    emit("connected", {"id": cp_id, "subprotocol": proto})
    try:
        await cp.start()
    except websockets.exceptions.ConnectionClosed:
        pass
    emit("closed", {"id": cp_id})


async def command(line):
    verb, _, rest = line.partition(" ")
    action, _, js = rest.partition(" ")
    cp = CURRENT.get("cp")
    if cp is None:
        emit("error", {"action": action, "code": "NoChargePoint", "description": ""})
        return
    try:
        payload = getattr(cp.calls, action)(**camel_to_snake_case(json.loads(js or "{}")))
        res = await cp.call(payload, suppress=False, skip_schema_validation=(verb == "rawcall"))
        emit("result", {"action": action, "payload": snake_to_camel_case(plain(res))})
    except OCPPError as e:
        emit("error", {"action": action, "code": e.code, "description": str(e.description)})
    except Exception as e:  # noqa: BLE001 - reported to the test
        emit("error", {"action": action, "code": type(e).__name__, "description": str(e)})


async def main():
    async with websockets.serve(on_connect, "0.0.0.0", 9000,
                                subprotocols=["ocpp2.1", "ocpp2.0.1", "ocpp1.6"]):
        emit("listening", {"port": 9000})
        loop = asyncio.get_running_loop()
        while True:
            line = await loop.run_in_executor(None, sys.stdin.readline)
            line = line.strip()
            if not line or line == "quit":
                return
            await command(line)


asyncio.run(main())
