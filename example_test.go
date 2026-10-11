// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package ocppsim_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/cB-Sigma/ocpp"
	"github.com/cB-Sigma/ocpp-sim/internal/minicsms"
	"github.com/cB-Sigma/ocpp-sim/sim"
)

// A 2.0.1 charging station boots, runs a session and answers a command of
// the central system; every message passes the OCA schemas.
func Example() {
	csms := minicsms.New() // a small central system on the ocpp module
	srv := httptest.NewServer(csms)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cp := sim.New(sim.Config{URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ocpp/", ID: "CP1", Version: ocpp.V201})
	if err := cp.Connect(ctx); err != nil {
		fmt.Println(err)
		return
	}
	defer cp.Disconnect()
	status, _ := cp.Boot(ctx)
	fmt.Println("boot:", status)
	_, _, _ = cp.Start(ctx, 1, "TAG1", 1000)
	_ = cp.Meter(ctx, 1, 1500, 7400, [3]float64{230, 230, 230})
	_ = cp.Stop(ctx, 1, 2000, "Local")
	res, _ := csms.Conn("CP1").Call(ctx, "Reset", map[string]any{"type": "OnIdle"})
	fmt.Println("reset:", string(res))
	fmt.Println("schema violations:", len(cp.Violations()))
	// Output:
	// boot: Accepted
	// reset: {"status":"Accepted"}
	// schema violations: 0
}
