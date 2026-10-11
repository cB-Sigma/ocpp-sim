// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package interop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	slcsms "github.com/seanlee0923/ocpp/csms"
	slprotocol "github.com/seanlee0923/ocpp/protocol"
	slv21 "github.com/seanlee0923/ocpp/v21"

	"github.com/cB-Sigma/ocpp"
	"github.com/cB-Sigma/ocpp-sim/sim"
)

// seanlee0923/ocpp's 2.1 CSMS against the simulator: boot, a heartbeat,
// a SEND (NotifyPeriodicEventStream) its typed SEND handler decodes, and a
// Reset from it.
func TestSeanleeCSMS21(t *testing.T) {
	router := slcsms.NewRouter()
	got := make(chan string, 8)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(slcsms.Handle(router, func(_ context.Context, _ *slcsms.Session, r slv21.BootNotificationRequest) (slv21.BootNotificationConfirmation, error) {
		got <- "BootNotification " + string(r.Reason)
		return slv21.BootNotificationConfirmation{CurrentTime: time.Now().UTC().Format(time.RFC3339), Interval: 300, Status: "Accepted"}, nil
	}))
	must(slcsms.Handle(router, func(_ context.Context, _ *slcsms.Session, _ slv21.HeartbeatRequest) (slv21.HeartbeatConfirmation, error) {
		got <- "Heartbeat"
		return slv21.HeartbeatConfirmation{CurrentTime: time.Now().UTC().Format(time.RFC3339)}, nil
	}))
	must(slcsms.Handle(router, func(_ context.Context, _ *slcsms.Session, r slv21.StatusNotificationRequest) (slv21.StatusNotificationConfirmation, error) {
		got <- fmt.Sprintf("StatusNotification %d %s", r.EVSEID, r.ConnectorStatus)
		return slv21.StatusNotificationConfirmation{}, nil
	}))
	must(slcsms.HandleSend(router, func(_ context.Context, _ *slcsms.Session, r slv21.NotifyPeriodicEventStreamRequest) error {
		got <- fmt.Sprintf("NotifyPeriodicEventStream %d %d", r.ID, len(r.Data))
		return nil
	}))
	connected := make(chan *slcsms.Session, 1)
	srv, err := slcsms.New(slcsms.Config{Router: router, Versions: []slprotocol.Version{slprotocol.OCPP21}, OnConnect: func(s *slcsms.Session) { connected <- s }})
	must(err)
	// Its identity is the whole request path, so it is mounted under /ocpp.
	ts := httptest.NewServer(http.StripPrefix("/ocpp", srv))
	defer ts.Close()

	cp := sim.New(sim.Config{URL: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ocpp/", ID: "SIM21", Version: ocpp.V21})
	must(cp.Connect(t.Context()))
	defer cp.Disconnect()
	var session *slcsms.Session
	select {
	case session = <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("no session")
	}
	if st, err := cp.Boot(t.Context()); err != nil || st != "Accepted" {
		t.Fatalf("boot: %s %v", st, err)
	}
	must(cp.Heartbeat(t.Context()))
	expect := func(want string) {
		t.Helper()
		select {
		case g := <-got:
			if g != want {
				t.Errorf("seanlee got %q, want %q", g, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("seanlee got nothing, want %q", want)
		}
	}
	expect("BootNotification PowerUp")
	expect("StatusNotification 1 Available")
	expect("StatusNotification 2 Available")
	expect("Heartbeat")
	stream := map[string]any{"id": 9, "pending": 0, "basetime": time.Now().UTC().Format(time.RFC3339),
		"data": []any{map[string]any{"t": 0, "v": "11.0"}, map[string]any{"t": 2, "v": "11.4"}, map[string]any{"t": 4, "v": "11.9"}}}
	must(cp.Send(t.Context(), "NotifyPeriodicEventStream", stream))
	expect("NotifyPeriodicEventStream 9 3")

	r, err := slcsms.Call[slv21.ResetRequest, slv21.ResetConfirmation](t.Context(), session, slv21.ResetRequest{Type: "OnIdle"})
	if err != nil || r.Status == "" {
		t.Fatalf("reset: %+v %v", r, err)
	}
	if _, err := cp.Wait(t.Context(), "Reset"); err != nil {
		t.Fatal(err)
	}
	for _, v := range cp.Violations() {
		t.Errorf("outside the OCA schemas: %v", v)
	}
}
