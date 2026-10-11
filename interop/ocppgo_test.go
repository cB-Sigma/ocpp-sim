// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package interop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	types16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/authorization"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/meter"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/remotecontrol"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
	types2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	"github.com/cB-Sigma/ocpp"
	"github.com/cB-Sigma/ocpp-sim/sim"
)

// waitListening waits until something accepts TCP connections on port.
func waitListening(t *testing.T, port int) {
	t.Helper()
	for try := 0; try < 100; try++ {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nothing listens on %d", port)
}

// result waits for a callback's answer.
func result[T any](t *testing.T, what string, ch chan T, errs chan error) T {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case err := <-errs:
		t.Fatalf("%s: %v", what, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no answer", what)
	}
	var zero T
	return zero
}

// recv waits for what a handler reported.
func recv[T any](t *testing.T, ch chan T) T {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("the central system received nothing")
	}
	var zero T
	return zero
}

// callback returns an ocpp-go callback that sends its answer or error on
// the channels.
func callback[T any](ch chan T, errs chan error) func(T, error) {
	return func(r T, err error) {
		if err != nil {
			errs <- err
			return
		}
		ch <- r
	}
}

// wantCodeOf checks that err is a CALLERROR with one of the codes.
func wantCodeOf(t *testing.T, what string, err error, codes ...string) {
	t.Helper()
	var ce *ocpp.CallError
	if !errors.As(err, &ce) {
		t.Errorf("%s: %v, want a CALLERROR", what, err)
		return
	}
	t.Logf("%s: the peer answered %s (%s)", what, ce.Code, ce.Description)
	for _, c := range codes {
		if ce.Code == c {
			return
		}
	}
	t.Errorf("%s: CALLERROR %s, want one of %v", what, ce.Code, codes)
}

// cs16 is an ocpp-go 1.6 central system handler that reports what it gets.
type cs16 struct {
	starts chan *core.StartTransactionRequest
	stops  chan *core.StopTransactionRequest
	meters chan *core.MeterValuesRequest
	nextTx int
}

func (h *cs16) OnAuthorize(string, *core.AuthorizeRequest) (*core.AuthorizeConfirmation, error) {
	return core.NewAuthorizationConfirmation(types16.NewIdTagInfo(types16.AuthorizationStatusAccepted)), nil
}
func (h *cs16) OnBootNotification(string, *core.BootNotificationRequest) (*core.BootNotificationConfirmation, error) {
	return core.NewBootNotificationConfirmation(types16.NewDateTime(time.Now()), 300, core.RegistrationStatusAccepted), nil
}
func (h *cs16) OnDataTransfer(string, *core.DataTransferRequest) (*core.DataTransferConfirmation, error) {
	return core.NewDataTransferConfirmation(core.DataTransferStatusUnknownVendorId), nil
}
func (h *cs16) OnHeartbeat(string, *core.HeartbeatRequest) (*core.HeartbeatConfirmation, error) {
	return core.NewHeartbeatConfirmation(types16.NewDateTime(time.Now())), nil
}
func (h *cs16) OnMeterValues(_ string, r *core.MeterValuesRequest) (*core.MeterValuesConfirmation, error) {
	h.meters <- r
	return core.NewMeterValuesConfirmation(), nil
}
func (h *cs16) OnStatusNotification(string, *core.StatusNotificationRequest) (*core.StatusNotificationConfirmation, error) {
	return core.NewStatusNotificationConfirmation(), nil
}
func (h *cs16) OnStartTransaction(_ string, r *core.StartTransactionRequest) (*core.StartTransactionConfirmation, error) {
	h.nextTx++
	h.starts <- r
	return core.NewStartTransactionConfirmation(types16.NewIdTagInfo(types16.AuthorizationStatusAccepted), h.nextTx), nil
}
func (h *cs16) OnStopTransaction(_ string, r *core.StopTransactionRequest) (*core.StopTransactionConfirmation, error) {
	h.stops <- r
	return core.NewStopTransactionConfirmation(), nil
}

// The 1.6 charge point simulator against ocpp-go's central system.
func TestOCPPGoCentralSystem16(t *testing.T) {
	port := freePort(t)
	h := &cs16{starts: make(chan *core.StartTransactionRequest, 8), stops: make(chan *core.StopTransactionRequest, 8),
		meters: make(chan *core.MeterValuesRequest, 8), nextTx: 100}
	cs := ocpp16.NewCentralSystem(nil, nil)
	cs.SetCoreHandler(h)
	go cs.Start(port, "/ocpp/{ws}")
	t.Cleanup(cs.Stop)
	waitListening(t, port)

	id := "SIMGO16"
	cp := sim.New(sim.Config{URL: fmt.Sprintf("ws://127.0.0.1:%d/ocpp/", port), ID: id, Version: ocpp.V16, Connectors: 1})
	connect(t, cp)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if st, err := cp.Boot(ctx); err != nil || st != "Accepted" {
		t.Fatalf("boot: %q %v", st, err)
	}
	if err := cp.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if st, err := cp.Authorize(ctx, "TAG-1"); err != nil || st != "Accepted" {
		t.Fatalf("authorize: %q %v", st, err)
	}
	tx, st, err := cp.Start(ctx, 1, "TAG-1", 1000)
	if err != nil || st != "Accepted" || tx != "101" {
		t.Fatalf("start: %q %q %v", tx, st, err)
	}
	if r := recv(t, h.starts); r.IdTag != "TAG-1" || r.MeterStart != 1000 || r.ConnectorId != 1 {
		t.Errorf("ocpp-go read StartTransaction %+v", r)
	}
	if err := cp.Meter(ctx, 1, 1500, 7400, [3]float64{230, 231, 229}); err != nil {
		t.Fatalf("meter values: %v", err)
	}
	if r := recv(t, h.meters); len(r.MeterValue) != 1 || len(r.MeterValue[0].SampledValue) != 5 || r.MeterValue[0].SampledValue[0].Value != "1500" ||
		r.TransactionId == nil || *r.TransactionId != 101 {
		t.Errorf("ocpp-go read MeterValues %+v", r)
	}
	if err := cp.Stop(ctx, 1, 2000, "Local"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if r := recv(t, h.stops); r.TransactionId != 101 || r.MeterStop != 2000 || r.Reason != core.ReasonLocal {
		t.Errorf("ocpp-go read StopTransaction %+v", r)
	}

	errs := make(chan error, 4)
	one := 1
	rs := make(chan *core.RemoteStartTransactionConfirmation, 1)
	if err := cs.RemoteStartTransaction(id, callback(rs, errs), "TAG-2", func(r *core.RemoteStartTransactionRequest) { r.ConnectorId = &one }); err != nil {
		t.Fatal(err)
	}
	if r := result(t, "RemoteStartTransaction", rs, errs); r.Status != types16.RemoteStartStopStatusAccepted {
		t.Errorf("RemoteStartTransaction: %+v", r)
	}
	if r := recv(t, h.starts); r.IdTag != "TAG-2" {
		t.Errorf("remote start: %+v", r)
	}
	deadline := time.Now().Add(5 * time.Second)
	for cp.Transaction(1) != "102" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	rstop := make(chan *core.RemoteStopTransactionConfirmation, 1)
	if err := cs.RemoteStopTransaction(id, callback(rstop, errs), 102); err != nil {
		t.Fatal(err)
	}
	if r := result(t, "RemoteStopTransaction", rstop, errs); r.Status != types16.RemoteStartStopStatusAccepted {
		t.Errorf("RemoteStopTransaction: %+v", r)
	}
	if r := recv(t, h.stops); r.TransactionId != 102 || r.Reason != core.ReasonRemote {
		t.Errorf("remote stop: %+v", r)
	}
	reset := make(chan *core.ResetConfirmation, 1)
	if err := cs.Reset(id, callback(reset, errs), core.ResetTypeSoft); err != nil {
		t.Fatal(err)
	}
	if r := result(t, "Reset", reset, errs); r.Status != core.ResetStatusAccepted {
		t.Errorf("Reset: %+v", r)
	}
	gc := make(chan *core.GetConfigurationConfirmation, 1)
	if err := cs.GetConfiguration(id, callback(gc, errs), []string{"HeartbeatInterval", "NoSuchKey"}); err != nil {
		t.Fatal(err)
	}
	r := result(t, "GetConfiguration", gc, errs)
	if len(r.ConfigurationKey) != 1 || r.ConfigurationKey[0].Key != "HeartbeatInterval" || r.ConfigurationKey[0].Value == nil ||
		*r.ConfigurationKey[0].Value != "300" || len(r.UnknownKey) != 1 || r.UnknownKey[0] != "NoSuchKey" {
		t.Errorf("GetConfiguration: %+v", r)
	}
	for _, v := range cp.Violations() {
		t.Errorf("ocpp-go sent or answered outside the OCA schemas: %v", v)
	}

	// A request ocpp-go's validation refuses.
	_, err = cp.Call(ctx, "StatusNotification", map[string]any{"connectorId": "one", "status": "Available", "errorCode": "NoError"})
	wantCodeOf(t, "a StatusNotification with a string connector", err, "TypeConstraintViolation", "FormationViolation", "FormatViolation")
	if err := cp.Heartbeat(ctx); err != nil {
		t.Errorf("heartbeat after the CALLERROR: %v", err)
	}
}

// cs201 is an ocpp-go 2.0.1 CSMS handler that reports what it gets.
type cs201 struct {
	events chan *transactions.TransactionEventRequest
}

func (h *cs201) OnBootNotification(string, *provisioning.BootNotificationRequest) (*provisioning.BootNotificationResponse, error) {
	return provisioning.NewBootNotificationResponse(types2.NewDateTime(time.Now()), 300, provisioning.RegistrationStatusAccepted), nil
}
func (h *cs201) OnNotifyReport(string, *provisioning.NotifyReportRequest) (*provisioning.NotifyReportResponse, error) {
	return &provisioning.NotifyReportResponse{}, nil
}
func (h *cs201) OnAuthorize(string, *authorization.AuthorizeRequest) (*authorization.AuthorizeResponse, error) {
	return authorization.NewAuthorizationResponse(types2.IdTokenInfo{Status: types2.AuthorizationStatusAccepted}), nil
}
func (h *cs201) OnHeartbeat(string, *availability.HeartbeatRequest) (*availability.HeartbeatResponse, error) {
	return availability.NewHeartbeatResponse(*types2.NewDateTime(time.Now())), nil
}
func (h *cs201) OnStatusNotification(string, *availability.StatusNotificationRequest) (*availability.StatusNotificationResponse, error) {
	return availability.NewStatusNotificationResponse(), nil
}
func (h *cs201) OnMeterValues(string, *meter.MeterValuesRequest) (*meter.MeterValuesResponse, error) {
	return meter.NewMeterValuesResponse(), nil
}
func (h *cs201) OnTransactionEvent(_ string, r *transactions.TransactionEventRequest) (*transactions.TransactionEventResponse, error) {
	h.events <- r
	res := transactions.NewTransactionEventResponse()
	if r.IDToken != nil {
		res.IDTokenInfo = &types2.IdTokenInfo{Status: types2.AuthorizationStatusAccepted}
	}
	return res, nil
}

// The 2.0.1 charging station simulator against ocpp-go's CSMS (ocpp-go
// has no 2.1).
func TestOCPPGoCSMS201(t *testing.T) {
	port := freePort(t)
	h := &cs201{events: make(chan *transactions.TransactionEventRequest, 8)}
	cs := ocpp2.NewCSMS(nil, nil)
	cs.SetProvisioningHandler(h)
	cs.SetAuthorizationHandler(h)
	cs.SetAvailabilityHandler(h)
	cs.SetMeterHandler(h)
	cs.SetTransactionsHandler(h)
	go cs.Start(port, "/ocpp/{ws}")
	t.Cleanup(cs.Stop)
	waitListening(t, port)

	id := "SIMGO201"
	cp := sim.New(sim.Config{URL: fmt.Sprintf("ws://127.0.0.1:%d/ocpp/", port), ID: id, Version: ocpp.V201, Connectors: 1})
	connect(t, cp)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if st, err := cp.Boot(ctx); err != nil || st != "Accepted" {
		t.Fatalf("boot: %q %v", st, err)
	}
	if err := cp.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if st, err := cp.Authorize(ctx, "TAG-1"); err != nil || st != "Accepted" {
		t.Fatalf("authorize: %q %v", st, err)
	}
	tx, st, err := cp.Start(ctx, 1, "TAG-1", 1000)
	if err != nil || st != "Accepted" {
		t.Fatalf("start: %q %q %v", tx, st, err)
	}
	if r := recv(t, h.events); r.EventType != transactions.TransactionEventStarted || r.TransactionInfo.TransactionID != tx || r.IDToken == nil || r.IDToken.IdToken != "TAG-1" {
		t.Errorf("ocpp-go read the Started event %+v", r)
	}
	if err := cp.Meter(ctx, 1, 1500, 7400, [3]float64{230, 231, 229}); err != nil {
		t.Fatalf("meter values: %v", err)
	}
	if r := recv(t, h.events); r.EventType != transactions.TransactionEventUpdated || len(r.MeterValue) != 1 || len(r.MeterValue[0].SampledValue) != 5 ||
		r.MeterValue[0].SampledValue[0].Value != 1.5 {
		t.Errorf("ocpp-go read the Updated event %+v", r)
	}
	if err := cp.Stop(ctx, 1, 2000, "Local"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if r := recv(t, h.events); r.EventType != transactions.TransactionEventEnded || r.TransactionInfo.StoppedReason != transactions.ReasonLocal {
		t.Errorf("ocpp-go read the Ended event %+v", r)
	}

	errs := make(chan error, 4)
	one := 1
	rs := make(chan *remotecontrol.RequestStartTransactionResponse, 1)
	if err := cs.RequestStartTransaction(id, callback(rs, errs), 7, types2.IdToken{IdToken: "TAG-2", Type: types2.IdTokenTypeISO14443},
		func(r *remotecontrol.RequestStartTransactionRequest) { r.EvseID = &one }); err != nil {
		t.Fatal(err)
	}
	if r := result(t, "RequestStartTransaction", rs, errs); r.Status != remotecontrol.RequestStartStopStatusAccepted {
		t.Errorf("RequestStartTransaction: %+v", r)
	}
	started := recv(t, h.events)
	if started.TriggerReason != transactions.TriggerReasonRemoteStart || started.TransactionInfo.RemoteStartID == nil || *started.TransactionInfo.RemoteStartID != 7 {
		t.Errorf("remote start: %+v", started)
	}
	rstop := make(chan *remotecontrol.RequestStopTransactionResponse, 1)
	if err := cs.RequestStopTransaction(id, callback(rstop, errs), started.TransactionInfo.TransactionID); err != nil {
		t.Fatal(err)
	}
	if r := result(t, "RequestStopTransaction", rstop, errs); r.Status != remotecontrol.RequestStartStopStatusAccepted {
		t.Errorf("RequestStopTransaction: %+v", r)
	}
	if r := recv(t, h.events); r.EventType != transactions.TransactionEventEnded || r.TransactionInfo.StoppedReason != transactions.ReasonRemote {
		t.Errorf("remote stop: %+v", r)
	}
	reset := make(chan *provisioning.ResetResponse, 1)
	if err := cs.Reset(id, callback(reset, errs), provisioning.ResetTypeImmediate); err != nil {
		t.Fatal(err)
	}
	if r := result(t, "Reset", reset, errs); r.Status != provisioning.ResetStatusAccepted {
		t.Errorf("Reset: %+v", r)
	}
	comp, vr := types2.Component{Name: "OCPPCommCtrlr"}, types2.Variable{Name: "HeartbeatInterval"}
	sv := make(chan *provisioning.SetVariablesResponse, 1)
	if err := cs.SetVariables(id, callback(sv, errs), []provisioning.SetVariableData{{AttributeValue: "120", Component: comp, Variable: vr}}); err != nil {
		t.Fatal(err)
	}
	if r := result(t, "SetVariables", sv, errs); len(r.SetVariableResult) != 1 || r.SetVariableResult[0].AttributeStatus != provisioning.SetVariableStatusAccepted {
		t.Errorf("SetVariables: %+v", r)
	}
	gv := make(chan *provisioning.GetVariablesResponse, 1)
	if err := cs.GetVariables(id, callback(gv, errs), []provisioning.GetVariableData{{Component: comp, Variable: vr}}); err != nil {
		t.Fatal(err)
	}
	if r := result(t, "GetVariables", gv, errs); len(r.GetVariableResult) != 1 || r.GetVariableResult[0].AttributeStatus != provisioning.GetVariableStatusAccepted ||
		r.GetVariableResult[0].AttributeValue != "120" {
		t.Errorf("GetVariables: %+v", r)
	}
	for _, v := range cp.Violations() {
		t.Errorf("ocpp-go sent or answered outside the OCA schemas: %v", v)
	}

	_, err = cp.Call(ctx, "StatusNotification", map[string]any{"evseId": "one", "connectorId": 1, "connectorStatus": "Available",
		"timestamp": time.Now().UTC().Format(time.RFC3339)})
	wantCodeOf(t, "a StatusNotification with a string EVSE", err, "TypeConstraintViolation", "FormatViolation")
	if err := cp.Heartbeat(ctx); err != nil {
		t.Errorf("heartbeat after the CALLERROR: %v", err)
	}
}
