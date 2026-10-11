// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

// Package interop runs the simulated charge point against central systems
// written by others: mobilityhouse/ocpp's (Python, peer/csms.py, in a
// container; skipped unless SIGMA_OCPP_INTEROP=1 and Docker answers) and
// lorenzodonini/ocpp-go's (in process).
package interop

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/cB-Sigma/ocpp"
	"github.com/cB-Sigma/ocpp-sim/sim"
)

// connect dials the central system until its published port answers.
func connect(t *testing.T, cp *sim.ChargePoint) {
	t.Helper()
	var err error
	for try := 0; try < 50; try++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = cp.Connect(ctx)
		cancel()
		if err == nil {
			t.Cleanup(cp.Disconnect)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("connect: %v", err)
}

// The charge point simulator of each version against mobilityhouse/ocpp's
// central system: boot, heartbeat, authorization, a local session with
// meter values, a remote start and stop, reset, configuration or
// variables, and schema-validation errors in both directions.
func TestMobilityhouseCentralSystem(t *testing.T) {
	needDocker(t)
	for _, v := range []string{ocpp.V16, ocpp.V201, ocpp.V21} {
		t.Run(v, func(t *testing.T) {
			port := freePort(t)
			p := startPeer(t, "-p", fmt.Sprintf("127.0.0.1:%d:9000", port), image, "python", "-u", "csms.py")
			p.expect(60*time.Second, "listening")
			id := "SIM" + strconv.Itoa(port)
			cp := sim.New(sim.Config{URL: fmt.Sprintf("ws://127.0.0.1:%d/ocpp/", port), ID: id, Version: v, Connectors: 1})
			connect(t, cp)
			p.expect(10*time.Second, "connected", v)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			if st, err := cp.Boot(ctx); err != nil || st != "Accepted" {
				t.Fatalf("boot: %q %v", st, err)
			}
			p.expect(5*time.Second, "recv", `"BootNotification"`)
			if err := cp.Heartbeat(ctx); err != nil {
				t.Fatalf("heartbeat: %v", err)
			}
			if st, err := cp.Authorize(ctx, "TAG-1"); err != nil || st != "Accepted" {
				t.Fatalf("authorize: %q %v", st, err)
			}
			tx, st, err := cp.Start(ctx, 1, "TAG-1", 1000)
			if err != nil || st != "Accepted" || tx == "" {
				t.Fatalf("start: %q %q %v", tx, st, err)
			}
			if err := cp.Meter(ctx, 1, 1500, 7400, [3]float64{230, 231, 229}); err != nil {
				t.Fatalf("meter values: %v", err)
			}
			if err := cp.Stop(ctx, 1, 2000, "Local"); err != nil {
				t.Fatalf("stop: %v", err)
			}
			if v == ocpp.V16 {
				p.expect(5*time.Second, "recv", `"StartTransaction"`, `"idTag": "TAG-1"`)
				p.expect(5*time.Second, "recv", `"MeterValues"`, `"Voltage"`)
				p.expect(5*time.Second, "recv", `"StopTransaction"`, `"meterStop": 2000`)
			} else {
				p.expect(5*time.Second, "recv", `"TransactionEvent"`, `"Started"`)
				p.expect(5*time.Second, "recv", `"TransactionEvent"`, `"Updated"`, `"Voltage"`)
				p.expect(5*time.Second, "recv", `"TransactionEvent"`, `"Ended"`)
			}

			// Remote start and stop.
			if v == ocpp.V16 {
				kind, ev := p.call("call", "RemoteStartTransaction", map[string]any{"connectorId": 1, "idTag": "TAG-2"})
				if kind != "result" || payloadOf(ev)["status"] != "Accepted" {
					t.Fatalf("RemoteStartTransaction: %s %v", kind, ev)
				}
				p.expect(5*time.Second, "recv", `"StartTransaction"`, `"idTag": "TAG-2"`)
			} else {
				kind, ev := p.call("call", "RequestStartTransaction", map[string]any{"evseId": 1, "remoteStartId": 7,
					"idToken": map[string]any{"idToken": "TAG-2", "type": "ISO14443"}})
				if kind != "result" || payloadOf(ev)["status"] != "Accepted" {
					t.Fatalf("RequestStartTransaction: %s %v", kind, ev)
				}
				p.expect(5*time.Second, "recv", `"TransactionEvent"`, `"RemoteStart"`, `"remoteStartId": 7`)
			}
			deadline := time.Now().Add(5 * time.Second)
			for cp.Transaction(1) == "" && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			tx = cp.Transaction(1)
			if v == ocpp.V16 {
				n, _ := strconv.Atoi(tx)
				kind, ev := p.call("call", "RemoteStopTransaction", map[string]any{"transactionId": n})
				if kind != "result" || payloadOf(ev)["status"] != "Accepted" {
					t.Fatalf("RemoteStopTransaction %s: %s %v", tx, kind, ev)
				}
				p.expect(5*time.Second, "recv", `"StopTransaction"`, `"reason": "Remote"`)
			} else {
				kind, ev := p.call("call", "RequestStopTransaction", map[string]any{"transactionId": tx})
				if kind != "result" || payloadOf(ev)["status"] != "Accepted" {
					t.Fatalf("RequestStopTransaction %s: %s %v", tx, kind, ev)
				}
				p.expect(5*time.Second, "recv", `"TransactionEvent"`, `"Ended"`, `"Remote"`)
			}

			// Reset and configuration (1.6) or variables (2.x).
			resetType := "Soft"
			if v != ocpp.V16 {
				resetType = "Immediate"
			}
			if kind, ev := p.call("call", "Reset", map[string]any{"type": resetType}); kind != "result" || payloadOf(ev)["status"] != "Accepted" {
				t.Fatalf("Reset: %s %v", kind, ev)
			}
			if v == ocpp.V16 {
				kind, ev := p.call("call", "GetConfiguration", map[string]any{"key": []string{"HeartbeatInterval", "NoSuchKey"}})
				if kind != "result" {
					t.Fatalf("GetConfiguration: %s %v", kind, ev)
				}
				keys, _ := payloadOf(ev)["configurationKey"].([]any)
				unknown, _ := payloadOf(ev)["unknownKey"].([]any)
				if len(keys) != 1 || keys[0].(map[string]any)["key"] != "HeartbeatInterval" || keys[0].(map[string]any)["value"] != "300" ||
					len(unknown) != 1 || unknown[0] != "NoSuchKey" {
					t.Errorf("GetConfiguration answer %v", payloadOf(ev))
				}
				kind, ev = p.call("call", "GetConfiguration", map[string]any{})
				if keys, _ := payloadOf(ev)["configurationKey"].([]any); kind != "result" || len(keys) == 0 {
					t.Errorf("GetConfiguration of every key: %s %v", kind, ev)
				}
			} else {
				cv := map[string]any{"component": map[string]any{"name": "OCPPCommCtrlr"}, "variable": map[string]any{"name": "HeartbeatInterval"}}
				set := map[string]any{"component": cv["component"], "variable": cv["variable"], "attributeValue": "120"}
				kind, ev := p.call("call", "SetVariables", map[string]any{"setVariableData": []any{set}})
				if kind != "result" {
					t.Fatalf("SetVariables: %s %v", kind, ev)
				}
				kind, ev = p.call("call", "GetVariables", map[string]any{"getVariableData": []any{cv}})
				res, _ := payloadOf(ev)["getVariableResult"].([]any)
				if kind != "result" || len(res) != 1 || res[0].(map[string]any)["attributeStatus"] != "Accepted" || res[0].(map[string]any)["attributeValue"] != "120" {
					t.Errorf("GetVariables: %s %v", kind, ev)
				}
			}

			// Schema-validation errors: a request mobilityhouse's schema
			// check refuses ...
			bad := map[string]any{"connectorId": "one", "status": "Available", "errorCode": "NoError"}
			if v != ocpp.V16 {
				bad = map[string]any{"evseId": "one", "connectorId": 1, "connectorStatus": "Available", "timestamp": time.Now().UTC().Format(time.RFC3339)}
			}
			_, err = cp.Call(ctx, "StatusNotification", bad)
			var ce *ocpp.CallError
			if !errors.As(err, &ce) || ce.Code != "TypeConstraintViolation" {
				t.Errorf("a StatusNotification with a string connector: %v, want a TypeConstraintViolation CALLERROR", err)
			}
			if len(cp.Violations()) == 0 {
				t.Error("the simulator did not record its own invalid request")
			}
			if err := cp.Heartbeat(ctx); err != nil {
				t.Errorf("heartbeat after the CALLERROR: %v", err)
			}
			// ... and one mobilityhouse sends past its own check, which the
			// simulator records as a violation of the central system.
			before := len(cp.Violations())
			p.call("rawcall", "Reset", map[string]any{"type": "Sometimes"})
			if len(cp.Violations()) <= before {
				t.Error("the simulator did not record the central system's invalid Reset")
			}
		})
	}
}
