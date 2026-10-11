// Package sim is an in-process OCPP charge point simulator for tests
// and demos: an OCPP 1.6J, OCPP-S 1.6, 2.0.1 or 2.1 charger that boots,
// authorizes, runs charging sessions with meter values, answers the
// CSMS's commands (remote start and stop, reset, availability,
// configuration and variables, triggers, smart charging, firmware update,
// diagnostics and logs, reservations, local lists) and reconnects.
package sim

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cB-Sigma/ocpp"
	"github.com/cB-Sigma/ocpp/chargepoint"
)

// Config describes a simulated charger.
type Config struct {
	URL        string // CSMS endpoint without the identity, e.g. ws://127.0.0.1:8887/ocpp/
	ID         string // charge point identity
	Version    string // ocpp.V16, ocpp.V201 or ocpp.V21
	Password   string // basic auth (security profiles 1 and 2)
	TLS        *tls.Config
	Connectors int // default 2
	Timeout    time.Duration

	// SOAP makes the charger speak OCPP-S 1.6 instead of OCPP-J (Version
	// ocpp.V16): URL is then the Central System service URL, and the
	// charger serves the Charge Point service on SOAPListen (default
	// 127.0.0.1:0), giving SOAPFrom (default http://<listen address>/) as
	// its From address.
	SOAP       bool
	SOAPListen string
	SOAPFrom   string
	// SOAPLenient makes the OCPP-S charger take requests and answers that
	// do not follow the WSDLs (element order, required elements, simple
	// types). By default it is strict, as a charger with a validating
	// SOAP stack is: such a request gets a SOAP Fault, such an answer
	// fails the call, and both are listed in Violations.
	SOAPLenient bool
}

// Received is a CALL the CSMS sent.
type Received struct {
	Action  string
	Payload map[string]any
}

// ChargePoint is a simulated charger.
type ChargePoint struct {
	cfg Config

	mu       sync.Mutex
	conn     *ocpp.Conn
	cancel   context.CancelFunc
	received []Received
	answers  map[string]string // action → status to answer
	config   map[string]string
	profiles map[int]json.RawMessage // by connector/EVSE
	sessions map[int]string          // connector → transaction ID
	meter    map[int]int             // connector → Wh register
	seq      int
	listVer  int
	notify   chan Received

	bootInterval int
	firmware     string // firmware version the charger boots with (default 1.0.0)
	nextFirmware string // version it boots with after an update (SetFirmwareAfterUpdate)
	ext          extState
	violations   []error
}

// New creates a simulator; Connect opens its connection.
func New(cfg Config) *ChargePoint {
	if cfg.Connectors == 0 {
		cfg.Connectors = 2
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &ChargePoint{cfg: cfg, answers: map[string]string{}, config: map[string]string{"HeartbeatInterval": "300"},
		profiles: map[int]json.RawMessage{}, sessions: map[int]string{}, meter: map[int]int{}, notify: make(chan Received, 64)}
}

// Connect dials the CSMS and serves the connection in the background.
func (cp *ChargePoint) Connect(ctx context.Context) error {
	if cp.cfg.SOAP {
		return cp.connectSOAP()
	}
	cp.mu.Lock()
	pw := cp.cfg.Password
	cp.mu.Unlock()
	// The library's charging station role opens the connection: identity
	// in the path, the one version offered, basic authentication, TLS.
	d, err := chargepoint.Dial(ctx, chargepoint.Config{URL: cp.cfg.URL, ID: cp.cfg.ID, Versions: []string{cp.cfg.Version}, Password: pw, TLS: cp.cfg.TLS})
	if err != nil {
		return err
	}
	c := ocpp.NewConn(d.WS, cp.cfg.Version, cp.cfg.Timeout, cp.handle)
	if cp.cfg.Version == ocpp.V21 {
		c.CheckResult = func(action string, p json.RawMessage) error {
			if strings.HasSuffix(action, "-Signed") {
				return nil // a signed answer's payload is a JWS
			}
			return ocpp.ValidateMessage(ocpp.V21, action, true, p)
		}
	}
	sctx, cancel := context.WithCancel(context.Background())
	cp.mu.Lock()
	cp.conn, cp.cancel = c, cancel
	cp.mu.Unlock()
	go func() { _ = c.Serve(sctx, 0) }()
	return nil
}

// SetPassword changes the basic authentication password of later
// connections.
func (cp *ChargePoint) SetPassword(pw string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.cfg.Password = pw
}

// Disconnect drops the connection (the charger goes offline).
func (cp *ChargePoint) Disconnect() {
	cp.mu.Lock()
	c, cancel := cp.conn, cp.cancel
	cp.conn = nil
	cp.mu.Unlock()
	if c != nil {
		c.Close("going offline")
		cancel()
	}
}

// Done is closed when the current connection ends.
func (cp *ChargePoint) Done() <-chan struct{} {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.conn == nil {
		c := make(chan struct{})
		close(c)
		return c
	}
	return cp.conn.Done()
}

// Answer makes the charger answer action with status from now on.
func (cp *ChargePoint) Answer(action, status string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.answers[action] = status
}

// Received lists the CALLs the CSMS sent so far.
func (cp *ChargePoint) Received() []Received {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]Received(nil), cp.received...)
}

// Wait returns the next CALL the CSMS sends with this action.
func (cp *ChargePoint) Wait(ctx context.Context, action string) (Received, error) {
	for {
		select {
		case r := <-cp.notify:
			if r.Action == action {
				return r, nil
			}
		case <-ctx.Done():
			return Received{}, fmt.Errorf("waiting for %s: %w", action, ctx.Err())
		}
	}
}

// Call sends any CALL and returns the raw answer.
func (cp *ChargePoint) Call(ctx context.Context, action string, payload any) (json.RawMessage, error) {
	cp.mu.Lock()
	c := cp.conn
	cp.mu.Unlock()
	if c == nil {
		return nil, errors.New("not connected")
	}
	// The simulator's own messages are checked against the OCA schema too
	// (1.6, 2.0.1 and 2.1), so a test notices a simulator that drifts.
	if !strings.HasSuffix(action, "-Signed") && ocpp.HasSchema(cp.cfg.Version, action) {
		if b, merr := json.Marshal(payload); merr == nil {
			if verr := ocpp.ValidateMessage(cp.cfg.Version, action, false, b); verr != nil {
				cp.violation(fmt.Errorf("simulator %s: %w", action, verr))
			}
		}
	}
	raw, err := c.Call(ctx, action, payload)
	if err == nil && ocpp.HasSchema(cp.cfg.Version, action) && !strings.HasSuffix(action, "-Signed") {
		// Every CSMS answer is checked against its OCA schema.
		if verr := ocpp.ValidateMessage(cp.cfg.Version, action, true, raw); verr != nil {
			cp.violation(verr)
		}
	}
	return raw, err
}

func (cp *ChargePoint) callInto(ctx context.Context, action string, payload any, out any) error {
	raw, err := cp.Call(ctx, action, payload)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func ts() string { return time.Now().UTC().Format(time.RFC3339) }

func (cp *ChargePoint) v16() bool { return cp.cfg.Version == ocpp.V16 }

// SetFirmwareAfterUpdate makes the charger boot with version v after an
// UpdateFirmware it installs, as a real charger restarts into the image.
func (cp *ChargePoint) SetFirmwareAfterUpdate(v string) {
	cp.mu.Lock()
	cp.nextFirmware = v
	cp.mu.Unlock()
}

// Firmware is the version the charger reports at boot.
func (cp *ChargePoint) Firmware() string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.firmware == "" {
		return "1.0.0"
	}
	return cp.firmware
}

// Boot sends BootNotification and returns the registration status.
func (cp *ChargePoint) Boot(ctx context.Context) (string, error) {
	var p any
	fw := cp.Firmware()
	if cp.v16() {
		p = map[string]any{"chargePointVendor": "SigmaSim", "chargePointModel": "AC22", "chargePointSerialNumber": cp.cfg.ID,
			"firmwareVersion": fw, "meterType": "sim"}
	} else {
		p = map[string]any{"reason": "PowerUp", "chargingStation": map[string]any{"vendorName": "SigmaSim", "model": "DC60",
			"serialNumber": cp.cfg.ID, "firmwareVersion": fw}}
	}
	var r struct {
		Status   string `json:"status"`
		Interval int    `json:"interval"`
	}
	err := cp.callInto(ctx, "BootNotification", p, &r)
	cp.mu.Lock()
	cp.bootInterval = r.Interval
	cp.mu.Unlock()
	// A 2.x station reports every connector's status once accepted
	// (connection and boot use case B01); this is how the CSMS learns
	// its EVSEs and connectors.
	if err == nil && !cp.v16() && r.Status == "Accepted" {
		for n := 1; n <= cp.cfg.Connectors; n++ {
			if err := cp.Status(ctx, n, "Available", ""); err != nil {
				return r.Status, err
			}
		}
	}
	return r.Status, err
}

// Heartbeat sends a Heartbeat.
func (cp *ChargePoint) Heartbeat(ctx context.Context) error {
	return cp.callInto(ctx, "Heartbeat", map[string]any{}, nil)
}

// Status sends a StatusNotification. For 2.0.1 status is a connector
// status (Available, Occupied, …) and the connector is EVSE n connector 1.
func (cp *ChargePoint) Status(ctx context.Context, connector int, status, errorCode string) error {
	if cp.v16() {
		if errorCode == "" {
			errorCode = "NoError"
		}
		return cp.callInto(ctx, "StatusNotification", map[string]any{"connectorId": connector, "status": status,
			"errorCode": errorCode, "timestamp": ts()}, nil)
	}
	return cp.callInto(ctx, "StatusNotification", map[string]any{"evseId": connector, "connectorId": 1,
		"connectorStatus": status, "timestamp": ts()}, nil)
}

// StatusAt sends a 2.x StatusNotification for connector of evse, for
// stations whose EVSEs have more than one connector (a dual-outlet EVSE
// with a Type 2 socket and a tethered cable, say).
func (cp *ChargePoint) StatusAt(ctx context.Context, evse, connector int, status string) error {
	return cp.callInto(ctx, "StatusNotification", map[string]any{"evseId": evse, "connectorId": connector,
		"connectorStatus": status, "timestamp": ts()}, nil)
}

// Authorize asks the CSMS about an id tag and returns its status.
func (cp *ChargePoint) Authorize(ctx context.Context, tag string) (string, error) {
	if cp.v16() {
		var r struct {
			IDTagInfo struct{ Status string } `json:"idTagInfo"`
		}
		err := cp.callInto(ctx, "Authorize", map[string]any{"idTag": tag}, &r)
		return r.IDTagInfo.Status, err
	}
	var r struct {
		IDTokenInfo struct{ Status string } `json:"idTokenInfo"`
	}
	err := cp.callInto(ctx, "Authorize", map[string]any{"idToken": map[string]any{"idToken": tag, "type": "ISO14443"}}, &r)
	return r.IDTokenInfo.Status, err
}

func (cp *ChargePoint) nextSeq() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.seq++
	return cp.seq
}

// Start starts a transaction on connector with tag (the meter starts at
// meterWh) and returns the transaction ID and the authorization status.
func (cp *ChargePoint) Start(ctx context.Context, connector int, tag string, meterWh int) (string, string, error) {
	return cp.start(ctx, connector, tag, meterWh, "Authorized", nil)
}

func (cp *ChargePoint) start(ctx context.Context, connector int, tag string, meterWh int, trigger string, remoteStartID any) (string, string, error) {
	cp.mu.Lock()
	cp.meter[connector] = meterWh
	cp.mu.Unlock()
	if cp.v16() {
		var r struct {
			TransactionID int                     `json:"transactionId"`
			IDTagInfo     struct{ Status string } `json:"idTagInfo"`
		}
		if err := cp.callInto(ctx, "StartTransaction", map[string]any{"connectorId": connector, "idTag": tag,
			"meterStart": meterWh, "timestamp": ts()}, &r); err != nil {
			return "", "", err
		}
		id := strconv.Itoa(r.TransactionID)
		cp.mu.Lock()
		cp.sessions[connector] = id
		cp.mu.Unlock()
		return id, r.IDTagInfo.Status, nil
	}
	id := fmt.Sprintf("%s-%d-%d", cp.cfg.ID, connector, time.Now().UnixNano()%1e9)
	ti := map[string]any{"transactionId": id, "chargingState": "Charging"}
	if remoteStartID != nil {
		ti["remoteStartId"] = remoteStartID
	}
	var r struct {
		IDTokenInfo *struct{ Status string } `json:"idTokenInfo"`
	}
	// The station names a 2.x transaction itself, so it knows it before
	// the Started event goes out: a RequestStopTransaction the CSMS sends
	// as soon as it reads the event finds it.
	cp.mu.Lock()
	cp.sessions[connector] = id
	cp.mu.Unlock()
	if err := cp.callInto(ctx, "TransactionEvent", map[string]any{
		"eventType": "Started", "timestamp": ts(), "triggerReason": trigger, "seqNo": cp.nextSeq(),
		"transactionInfo": ti, "idToken": map[string]any{"idToken": tag, "type": "ISO14443"},
		"evse": map[string]any{"id": connector, "connectorId": 1},
		"meterValue": []any{map[string]any{"timestamp": ts(), "sampledValue": []any{map[string]any{
			"value": meterWh, "context": "Transaction.Begin", "measurand": "Energy.Active.Import.Register", "unitOfMeasure": map[string]any{"unit": "Wh"}}}}},
	}, &r); err != nil {
		cp.mu.Lock()
		if cp.sessions[connector] == id {
			delete(cp.sessions, connector) // as before: no transaction when the event failed
		}
		cp.mu.Unlock()
		return "", "", err
	}
	st := ""
	if r.IDTokenInfo != nil {
		st = r.IDTokenInfo.Status
	}
	return id, st, nil
}

// Meter sends meter values for connector: the energy register in Wh,
// active power in W and one voltage per phase.
func (cp *ChargePoint) Meter(ctx context.Context, connector int, energyWh int, powerW float64, volts [3]float64) error {
	cp.mu.Lock()
	cp.meter[connector] = energyWh
	tx := cp.sessions[connector]
	cp.mu.Unlock()
	if cp.v16() {
		sv := []any{
			map[string]any{"value": strconv.Itoa(energyWh), "measurand": "Energy.Active.Import.Register", "unit": "Wh", "context": "Sample.Periodic"},
			map[string]any{"value": strconv.FormatFloat(powerW, 'f', -1, 64), "measurand": "Power.Active.Import", "unit": "W"},
		}
		for i, v := range volts {
			sv = append(sv, map[string]any{"value": strconv.FormatFloat(v, 'f', -1, 64), "measurand": "Voltage", "unit": "V", "phase": fmt.Sprintf("L%d-N", i+1)})
		}
		p := map[string]any{"connectorId": connector, "meterValue": []any{map[string]any{"timestamp": ts(), "sampledValue": sv}}}
		if tx != "" {
			n, _ := strconv.Atoi(tx)
			p["transactionId"] = n
		}
		return cp.callInto(ctx, "MeterValues", p, nil)
	}
	sv := []any{
		map[string]any{"value": float64(energyWh) / 1000, "measurand": "Energy.Active.Import.Register", "unitOfMeasure": map[string]any{"unit": "kWh"}},
		map[string]any{"value": powerW / 1000, "measurand": "Power.Active.Import", "unitOfMeasure": map[string]any{"unit": "W", "multiplier": 3}},
	}
	for i, v := range volts {
		sv = append(sv, map[string]any{"value": v, "measurand": "Voltage", "phase": fmt.Sprintf("L%d-N", i+1), "unitOfMeasure": map[string]any{"unit": "V"}})
	}
	mv := []any{map[string]any{"timestamp": ts(), "sampledValue": sv}}
	if tx == "" {
		return cp.callInto(ctx, "MeterValues", map[string]any{"evseId": connector, "meterValue": mv}, nil)
	}
	return cp.callInto(ctx, "TransactionEvent", map[string]any{"eventType": "Updated", "timestamp": ts(), "triggerReason": "MeterValuePeriodic",
		"seqNo": cp.nextSeq(), "transactionInfo": map[string]any{"transactionId": tx, "chargingState": "Charging"},
		"evse": map[string]any{"id": connector, "connectorId": 1}, "meterValue": mv}, nil)
}

// Stop ends connector's transaction with the meter at meterWh.
func (cp *ChargePoint) Stop(ctx context.Context, connector int, meterWh int, reason string) error {
	cp.mu.Lock()
	tx := cp.sessions[connector]
	delete(cp.sessions, connector)
	cp.mu.Unlock()
	if tx == "" {
		return errors.New("no transaction on this connector")
	}
	if cp.v16() {
		n, _ := strconv.Atoi(tx)
		p := map[string]any{"transactionId": n, "meterStop": meterWh, "timestamp": ts()}
		if reason != "" {
			p["reason"] = reason
		}
		return cp.callInto(ctx, "StopTransaction", p, nil)
	}
	ti := map[string]any{"transactionId": tx, "chargingState": "Idle"}
	if reason != "" {
		ti["stoppedReason"] = reason
	}
	return cp.callInto(ctx, "TransactionEvent", map[string]any{"eventType": "Ended", "timestamp": ts(), "triggerReason": "StopAuthorized",
		"seqNo": cp.nextSeq(), "transactionInfo": ti, "evse": map[string]any{"id": connector, "connectorId": 1},
		"meterValue": []any{map[string]any{"timestamp": ts(), "sampledValue": []any{map[string]any{
			"value": meterWh, "context": "Transaction.End", "unitOfMeasure": map[string]any{"unit": "Wh"}}}}}}, nil)
}

// Transaction returns connector's running transaction ID, or "".
func (cp *ChargePoint) Transaction(connector int) string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.sessions[connector]
}

// ---- CSMS-initiated calls -------------------------------------------------------

func (cp *ChargePoint) answer(action, def string) string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if s, ok := cp.answers[action]; ok {
		return s
	}
	return def
}

// later runs f after the CALLRESULT went out.
func (cp *ChargePoint) later(f func(ctx context.Context)) {
	go func() {
		time.Sleep(20 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		f(ctx)
	}()
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func (cp *ChargePoint) handle(_ context.Context, action string, raw json.RawMessage) (any, error) {
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	r := Received{Action: action, Payload: p}
	cp.mu.Lock()
	cp.received = append(cp.received, r)
	cp.mu.Unlock()
	select {
	case cp.notify <- r:
	default:
	}
	if base, signed := strings.CutSuffix(action, "-Signed"); signed {
		// A signed request (part 4 clause 7): its JWS payload is taken
		// as is (the simulator does not verify) and answered plainly.
		var jws struct {
			Payload string `json:"payload"`
		}
		_ = json.Unmarshal(raw, &jws)
		plain, err := base64.RawURLEncoding.DecodeString(jws.Payload)
		if err != nil {
			return nil, &ocpp.CallError{Code: "FormatViolation", Description: "not a JWS"}
		}
		return cp.handle(context.Background(), base, plain)
	}
	if ocpp.HasSchema(cp.cfg.Version, action) {
		// Every CSMS request is checked against its OCA schema.
		if verr := ocpp.ValidateMessage(cp.cfg.Version, action, false, raw); verr != nil {
			cp.violation(verr)
		}
	}
	if cp.cfg.Version != ocpp.V16 {
		if res, ok := cp.handle2x(action, p); ok {
			return res, nil
		}
	}
	if res, ok := cp.handleExt(action, p); ok {
		return res, nil
	}
	status := func(def string) map[string]any { return map[string]any{"status": cp.answer(action, def)} }
	switch action {
	case "RemoteStartTransaction", "RequestStartTransaction":
		res := status("Accepted")
		if res["status"] == "Accepted" {
			conn := 1
			tag := ""
			if cp.v16() {
				if c, ok := p["connectorId"]; ok {
					conn = num(c)
				}
				tag, _ = p["idTag"].(string)
			} else {
				if c, ok := p["evseId"]; ok {
					conn = num(c)
				}
				tok, _ := p["idToken"].(map[string]any)
				tag, _ = tok["idToken"].(string)
			}
			rsid := p["remoteStartId"]
			cp.later(func(ctx context.Context) { _, _, _ = cp.start(ctx, conn, tag, 1000, "RemoteStart", rsid) })
		}
		return res, nil
	case "RemoteStopTransaction", "RequestStopTransaction":
		res := status("Accepted")
		want := fmt.Sprint(p["transactionId"])
		if f, ok := p["transactionId"].(float64); ok {
			want = strconv.Itoa(int(f))
		}
		conn := -1
		cp.mu.Lock()
		for c, tx := range cp.sessions {
			if tx == want {
				conn = c
			}
		}
		m := cp.meter[conn]
		cp.mu.Unlock()
		if conn < 0 {
			res["status"] = "Rejected"
		} else if res["status"] == "Accepted" {
			cp.later(func(ctx context.Context) { _ = cp.Stop(ctx, conn, m+500, "Remote") })
		}
		return res, nil
	case "Reset", "ClearCache", "ReserveNow", "CancelReservation", "SendLocalList", "ClearChargingProfile":
		if action == "SendLocalList" {
			cp.mu.Lock()
			cp.listVer = num(p["listVersion"])
			cp.mu.Unlock()
		}
		return status("Accepted"), nil
	case "UnlockConnector":
		return status("Unlocked"), nil
	case "ChangeAvailability":
		res := status("Accepted")
		if res["status"] == "Accepted" {
			st := "Available"
			if p["type"] == "Inoperative" || p["operationalStatus"] == "Inoperative" {
				st = "Unavailable"
			}
			conn := 0
			if cp.v16() {
				conn = num(p["connectorId"])
			} else if e, ok := p["evse"].(map[string]any); ok {
				conn = num(e["id"])
			}
			cp.later(func(ctx context.Context) { _ = cp.Status(ctx, conn, st, "") })
		}
		return res, nil
	case "ChangeConfiguration":
		res := status("Accepted")
		if res["status"] == "Accepted" {
			cp.mu.Lock()
			cp.config[fmt.Sprint(p["key"])] = fmt.Sprint(p["value"])
			cp.mu.Unlock()
		}
		return res, nil
	case "GetConfiguration":
		// OCPP 1.6 clause 5.8: the asked keys only, when the request
		// names some, and the ones the charger does not know in
		// unknownKey; every key when it names none.
		cp.mu.Lock()
		defer cp.mu.Unlock()
		keys := []any{}
		var unknown []any
		asked, _ := p["key"].([]any)
		if len(asked) == 0 {
			names := make([]string, 0, len(cp.config))
			for k := range cp.config {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				keys = append(keys, map[string]any{"key": k, "readonly": false, "value": cp.config[k]})
			}
		}
		for _, a := range asked {
			k, _ := a.(string)
			if v, ok := cp.config[k]; ok {
				keys = append(keys, map[string]any{"key": k, "readonly": false, "value": v})
			} else {
				unknown = append(unknown, k)
			}
		}
		res := map[string]any{"configurationKey": keys}
		if len(unknown) > 0 {
			res["unknownKey"] = unknown
		}
		return res, nil
	case "SetVariables":
		var out []any
		data, _ := p["setVariableData"].([]any)
		for _, x := range data {
			m, _ := x.(map[string]any)
			c, _ := m["component"].(map[string]any)
			v, _ := m["variable"].(map[string]any)
			st := cp.answer(action, "Accepted")
			if st == "Accepted" {
				cp.mu.Lock()
				cp.config[fmt.Sprint(c["name"])+"/"+fmt.Sprint(v["name"])] = fmt.Sprint(m["attributeValue"])
				cp.mu.Unlock()
			}
			out = append(out, map[string]any{"attributeStatus": st, "component": c, "variable": v})
		}
		return map[string]any{"setVariableResult": out}, nil
	case "GetVariables":
		var out []any
		data, _ := p["getVariableData"].([]any)
		for _, x := range data {
			m, _ := x.(map[string]any)
			c, _ := m["component"].(map[string]any)
			v, _ := m["variable"].(map[string]any)
			cp.mu.Lock()
			val, ok := cp.config[fmt.Sprint(c["name"])+"/"+fmt.Sprint(v["name"])]
			cp.mu.Unlock()
			res := map[string]any{"attributeStatus": "UnknownVariable", "component": c, "variable": v}
			if ok {
				res["attributeStatus"], res["attributeValue"] = "Accepted", val
			}
			out = append(out, res)
		}
		return map[string]any{"getVariableResult": out}, nil
	case "TriggerMessage":
		res := status("Accepted")
		if res["status"] == "Accepted" {
			msg, _ := p["requestedMessage"].(string)
			cp.later(func(ctx context.Context) {
				switch msg {
				case "Heartbeat":
					_ = cp.Heartbeat(ctx)
				case "BootNotification":
					_, _ = cp.Boot(ctx)
				case "StatusNotification":
					_ = cp.Status(ctx, 1, "Available", "")
				case "MeterValues":
					_ = cp.Meter(ctx, 1, 0, 0, [3]float64{230, 230, 230})
				case "SignChargingStationCertificate":
					_, _ = cp.SignCertificate(ctx, "", "ChargingStationCertificate")
				case "SignV2GCertificate":
					_, _ = cp.SignCertificate(ctx, "", "V2GCertificate")
				}
			})
		}
		return res, nil
	case "SetChargingProfile":
		res := status("Accepted")
		if res["status"] == "Accepted" {
			key, prof := "csChargingProfiles", "connectorId"
			if !cp.v16() {
				key, prof = "chargingProfile", "evseId"
			}
			b, _ := json.Marshal(p[key])
			cp.mu.Lock()
			cp.profiles[num(p[prof])] = b
			cp.mu.Unlock()
		}
		return res, nil
	case "GetCompositeSchedule":
		cp.mu.Lock()
		prof, ok := cp.profiles[num(p["connectorId"])]
		cp.mu.Unlock()
		if !ok {
			return map[string]any{"status": "Rejected"}, nil
		}
		var cpf struct {
			ChargingSchedule json.RawMessage `json:"chargingSchedule"`
		}
		_ = json.Unmarshal(prof, &cpf)
		return map[string]any{"status": "Accepted", "connectorId": num(p["connectorId"]), "scheduleStart": ts(),
			"chargingSchedule": cpf.ChargingSchedule}, nil
	case "UpdateFirmware":
		steps := []string{"Downloading", "Downloaded", "Installing", "Installed"}
		if fail := cp.answer("firmware", ""); fail != "" {
			steps = []string{"Downloading", fail}
		}
		var rid any
		if !cp.v16() {
			rid = p["requestId"]
		}
		cp.later(func(ctx context.Context) {
			for _, s := range steps {
				m := map[string]any{"status": s}
				if rid != nil {
					m["requestId"] = rid
				}
				_ = cp.callInto(ctx, "FirmwareStatusNotification", m, nil)
			}
			cp.mu.Lock()
			next := cp.nextFirmware
			if steps[len(steps)-1] == "Installed" && next != "" {
				cp.firmware = next
			}
			cp.mu.Unlock()
			if steps[len(steps)-1] == "Installed" && next != "" {
				_, _ = cp.Boot(ctx) // restarts into the new image
			}
		})
		if cp.v16() {
			return map[string]any{}, nil
		}
		return status("Accepted"), nil
	case "GetDiagnostics":
		cp.later(func(ctx context.Context) {
			for _, s := range []string{"Uploading", "Uploaded"} {
				_ = cp.callInto(ctx, "DiagnosticsStatusNotification", map[string]any{"status": s}, nil)
			}
		})
		return map[string]any{"fileName": "diag-" + cp.cfg.ID + ".zip"}, nil
	case "GetLog":
		rid := p["requestId"]
		cp.later(func(ctx context.Context) {
			for _, s := range []string{"Uploading", "Uploaded"} {
				_ = cp.callInto(ctx, "LogStatusNotification", map[string]any{"status": s, "requestId": rid}, nil)
			}
		})
		res := status("Accepted")
		res["filename"] = "log-" + cp.cfg.ID + ".zip"
		return res, nil
	case "GetLocalListVersion":
		cp.mu.Lock()
		defer cp.mu.Unlock()
		return map[string]any{"listVersion": cp.listVer}, nil
	case "DataTransfer":
		return map[string]any{"status": "UnknownVendorId"}, nil
	}
	return nil, &ocpp.CallError{Code: "NotImplemented", Description: action}
}
