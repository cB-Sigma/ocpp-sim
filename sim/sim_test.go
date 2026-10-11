// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package sim_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cB-Sigma/ocpp"
	"github.com/cB-Sigma/ocpp-sim/internal/minicsms"
	"github.com/cB-Sigma/ocpp-sim/sim"
)

// The simulator speaks each version to a central system built from the
// library alone, and every message it sends or receives passes the OCA
// schemas.
func TestSimulatorEveryVersion(t *testing.T) {
	csms := minicsms.New()
	srv := httptest.NewServer(csms)
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ocpp/"
	for _, v := range []string{ocpp.V16, ocpp.V201, ocpp.V21} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cp := sim.New(sim.Config{URL: url, ID: "SIM-" + v, Version: v})
		if err := cp.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		if st, err := cp.Boot(ctx); err != nil || st != "Accepted" {
			t.Fatalf("%s: boot %q %v", v, st, err)
		}
		if _, _, err := cp.Start(ctx, 1, "TAG1", 0); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if err := cp.Meter(ctx, 1, 1500, 7000, [3]float64{230, 230, 230}); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if err := cp.Stop(ctx, 1, 3000, "Local"); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		// The central system calls the charger too.
		if _, err := csms.Conn("SIM-"+v).Call(ctx, "ClearCache", map[string]any{}); err != nil {
			t.Fatalf("%s: ClearCache: %v", v, err)
		}
		for _, e := range cp.Violations() {
			t.Errorf("%s: %v", v, e)
		}
		cp.Disconnect()
		cancel()
	}
}

// A 2.x station knows its transaction when its Started event reaches the
// CSMS, so a RequestStopTransaction sent at once is not refused. Found by
// running ocpp-go's CSMS against the simulator: the simulator recorded the
// transaction only after the CSMS had answered the event, and refused the
// stop that ocpp-go sent as soon as it had read it.
func TestTransactionKnownWhenStartedIsSent(t *testing.T) {
	csms := minicsms.New()
	srv := httptest.NewServer(csms)
	defer srv.Close()
	cps := map[string]*sim.ChargePoint{}
	for _, v := range []string{ocpp.V201, ocpp.V21} {
		cps["SIM-TX-"+v] = sim.New(sim.Config{URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ocpp/", ID: "SIM-TX-" + v, Version: v})
	}
	known := make(chan string, 4)
	csms.OnCall = func(id, action string, payload json.RawMessage) {
		if action == "TransactionEvent" && strings.Contains(string(payload), `"Started"`) {
			known <- cps[id].Transaction(1)
		}
	}
	for _, v := range []string{ocpp.V201, ocpp.V21} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cp := cps["SIM-TX-"+v]
		if err := cp.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		tx, _, err := cp.Start(ctx, 1, "TAG1", 0)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if got := <-known; got != tx {
			t.Errorf("%s: while the CSMS read the Started event the station's transaction was %q, want %q", v, got, tx)
		}
		cp.Disconnect()
		cancel()
	}
}

// GetConfiguration with keys answers those keys only and lists the ones
// the charger does not know in unknownKey (OCPP 1.6 clause 5.8). Found
// while writing the mobilityhouse/ocpp interop test: the simulator had
// answered every key and no unknownKey.
func TestGetConfigurationAskedKeys(t *testing.T) {
	csms := minicsms.New()
	srv := httptest.NewServer(csms)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cp := sim.New(sim.Config{URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ocpp/", ID: "SIM-CONF", Version: ocpp.V16})
	if err := cp.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer cp.Disconnect()
	if _, err := cp.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	c := csms.Conn("SIM-CONF")
	if _, err := c.Call(ctx, "ChangeConfiguration", map[string]any{"key": "MeterValueSampleInterval", "value": "60"}); err != nil {
		t.Fatal(err)
	}
	raw, err := c.Call(ctx, "GetConfiguration", map[string]any{"key": []string{"HeartbeatInterval", "NoSuchKey"}})
	if err != nil {
		t.Fatal(err)
	}
	if verr := ocpp.ValidateMessage(ocpp.V16, "GetConfiguration", true, raw); verr != nil {
		t.Fatal(verr)
	}
	if got, want := string(raw), `{"configurationKey":[{"key":"HeartbeatInterval","readonly":false,"value":"300"}],"unknownKey":["NoSuchKey"]}`; got != want {
		t.Errorf("asked keys: %s, want %s", got, want)
	}
	raw, err = c.Call(ctx, "GetConfiguration", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"configurationKey":[{"key":"HeartbeatInterval","readonly":false,"value":"300"},{"key":"MeterValueSampleInterval","readonly":false,"value":"60"}]}`; got != want {
		t.Errorf("every key: %s, want %s", got, want)
	}
}
