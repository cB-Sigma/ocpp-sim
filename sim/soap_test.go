// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package sim_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cB-Sigma/ocpp"
	"github.com/cB-Sigma/ocpp-sim/sim"
)

// soapCS is a small OCPP-S Central System: it answers through the
// library (WSDL order), or, with sorted set, writes BootNotification's
// answer with its elements in name order, as SigmaM2M did before
// F-2888.
type soapCS struct {
	sorted bool
	from   string // the charger's From address
}

func (c *soapCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q, f := ocpp.ReadSOAPRequest(r)
	if f != nil {
		f.Write(w)
		return
	}
	c.from = q.From
	action, f := q.Action()
	if f != nil {
		f.Write(w)
		return
	}
	if _, f := q.Payload(action); f != nil {
		f.Write(w)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tag := map[string]any{"status": "Accepted", "expiryDate": now, "parentIdTag": "PARENT"}
	var res any = map[string]any{}
	switch action {
	case "BootNotification":
		if c.sorted {
			w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
			fmt.Fprintf(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://www.w3.org/2005/08/addressing"><s:Header><a:Action>/BootNotificationResponse</a:Action><a:RelatesTo>%s</a:RelatesTo></s:Header><s:Body><bootNotificationResponse xmlns="urn://Ocpp/Cs/2015/10/"><currentTime>%s</currentTime><interval>300</interval><status>Accepted</status></bootNotificationResponse></s:Body></s:Envelope>`, q.MessageID, now)
			return
		}
		res = map[string]any{"status": "Accepted", "currentTime": now, "interval": 300}
	case "Authorize", "StopTransaction":
		res = map[string]any{"idTagInfo": tag}
	case "StartTransaction":
		res = map[string]any{"idTagInfo": tag, "transactionId": 7}
	case "Heartbeat":
		res = map[string]any{"currentTime": now}
	}
	ocpp.WriteSOAPResponse(w, q.MessageID, action, res)
}

func soapCharger(t *testing.T, cs *soapCS, lenient bool) (*sim.ChargePoint, func()) {
	t.Helper()
	srv := httptest.NewServer(cs)
	cp := sim.New(sim.Config{URL: srv.URL + "/ocpp16s/", ID: "SOAPSIM", Version: ocpp.V16, SOAP: true, SOAPLenient: lenient})
	if err := cp.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return cp, func() { cp.Disconnect(); srv.Close() }
}

// Covers: SIG-IPE-6416
// The OCPP-S simulator is strict by default in both directions: a
// Central System answer or request whose elements are out of the WSDL
// sequence is refused (the call fails with a ProtocolError fault; the
// request gets a SOAP Fault) and listed in Violations; SOAPLenient takes
// both. Messages in WSDL order, with nested types, pass.
func TestSOAPSimulatorStrict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A Central System that follows the WSDLs: a whole session works.
	cs := &soapCS{}
	cp, done := soapCharger(t, cs, false)
	if st, err := cp.Boot(ctx); err != nil || st != "Accepted" {
		t.Fatalf("boot %q %v", st, err)
	}
	if st, err := cp.Authorize(ctx, "TAG1"); err != nil || st != "Accepted" {
		t.Fatalf("authorize %q %v", st, err)
	}
	if _, _, err := cp.Start(ctx, 1, "TAG1", 0); err != nil {
		t.Fatal(err)
	}
	if err := cp.Meter(ctx, 1, 1500, 7000, [3]float64{230, 230, 230}); err != nil {
		t.Fatal(err)
	}
	if err := cp.Stop(ctx, 1, 3000, "Local"); err != nil {
		t.Fatal(err)
	}
	// The Central System's commands in WSDL order, nested types included.
	profile := map[string]any{"chargingProfileId": 3, "transactionId": 7, "stackLevel": 1, "chargingProfilePurpose": "TxProfile",
		"chargingProfileKind": "Absolute", "validFrom": "2026-10-06T10:00:00Z", "chargingSchedule": map[string]any{"duration": 600,
			"startSchedule": "2026-10-06T10:00:00Z", "chargingRateUnit": "A", "minChargingRate": 6.0,
			"chargingSchedulePeriod": []any{map[string]any{"startPeriod": 0, "limit": 16.0, "numberPhases": 3}, map[string]any{"startPeriod": 300, "limit": 10.5}}}}
	for _, c := range []struct {
		action  string
		payload any
	}{
		{"SetChargingProfile", map[string]any{"connectorId": 1, "csChargingProfiles": profile}},
		{"RemoteStartTransaction", map[string]any{"connectorId": 1, "idTag": "TAG2", "chargingProfile": profile}},
		{"SendLocalList", map[string]any{"listVersion": 2, "updateType": "Full", "localAuthorizationList": []any{
			map[string]any{"idTag": "TAG1", "idTagInfo": map[string]any{"status": "Accepted", "expiryDate": "2026-12-31T00:00:00Z", "parentIdTag": "P"}}}}},
		{"GetConfiguration", map[string]any{"key": []any{"HeartbeatInterval", "Nope"}}},
		{"ChangeConfiguration", map[string]any{"key": "HeartbeatInterval", "value": "60"}},
		{"GetCompositeSchedule", map[string]any{"connectorId": 1, "duration": 600, "chargingRateUnit": "A"}},
	} {
		pl, _ := json.Marshal(c.payload)
		if _, err := ocpp.SOAPCall(ctx, http.DefaultClient, "SOAPSIM", "", cs.from, c.action, pl); err != nil {
			t.Errorf("%s: %v", c.action, err)
		}
	}
	for _, e := range cp.Violations() {
		t.Errorf("violation: %v", e)
	}
	// A request out of the WSDL sequence (ChargingProfile in name order).
	outOfOrder := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://www.w3.org/2005/08/addressing"><s:Header><chargeBoxIdentity xmlns="urn://Ocpp/Cp/2015/10/">SOAPSIM</chargeBoxIdentity><a:Action>/SetChargingProfile</a:Action><a:MessageID>urn:uuid:9</a:MessageID></s:Header><s:Body><setChargingProfileRequest xmlns="urn://Ocpp/Cp/2015/10/"><connectorId>1</connectorId><csChargingProfiles><chargingProfileId>1</chargingProfileId><chargingProfileKind>Absolute</chargingProfileKind><chargingProfilePurpose>TxDefaultProfile</chargingProfilePurpose><chargingSchedule><chargingRateUnit>A</chargingRateUnit><chargingSchedulePeriod><limit>16.0</limit><startPeriod>0</startPeriod></chargingSchedulePeriod></chargingSchedule><stackLevel>0</stackLevel></csChargingProfiles></setChargingProfileRequest></s:Body></s:Envelope>`
	post := func(to string) (int, string) {
		resp, err := http.Post(to, "application/soap+xml; charset=utf-8", strings.NewReader(outOfOrder))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, ans := post(cs.from); code != 500 || !strings.Contains(ans, ":ProtocolError<") || !strings.Contains(ans, "out of order") {
		t.Errorf("strict charger took an out-of-order request: %d %s", code, ans)
	}
	if v := cp.Violations(); len(v) != 1 || !strings.Contains(v[0].Error(), "SetChargingProfile") {
		t.Errorf("violations %v", v)
	}
	done()

	// A Central System that writes BootNotification's answer in name
	// order: the strict charger refuses it, the lenient one boots.
	cs = &soapCS{sorted: true}
	cp, done = soapCharger(t, cs, false)
	_, err := cp.Boot(ctx)
	var ce *ocpp.CallError
	if !errors.As(err, &ce) || ce.Code != "ProtocolError" || !strings.Contains(ce.Description, "out of order") {
		t.Errorf("strict charger booted on an out-of-order answer: %v", err)
	}
	if v := cp.Violations(); len(v) != 1 || !strings.Contains(v[0].Error(), "BootNotification") {
		t.Errorf("violations %v", v)
	}
	done()

	cp, done = soapCharger(t, cs, true)
	defer done()
	if st, err := cp.Boot(ctx); err != nil || st != "Accepted" {
		t.Errorf("lenient charger: boot %q %v", st, err)
	}
	if code, ans := post(cs.from); code != 200 || !strings.Contains(ans, "<setChargingProfileResponse") {
		t.Errorf("lenient charger: %d %s", code, ans)
	}
	if v := cp.Violations(); len(v) != 0 {
		t.Errorf("lenient charger lists %v", v)
	}
}
