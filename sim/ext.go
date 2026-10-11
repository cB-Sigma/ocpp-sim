package sim

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// DMVariable is one variable of the simulated 2.0.1 device model.
type DMVariable struct {
	Component, ComponentInstance string
	EVSE, Connector              int // 0: none
	Variable                     string
	Value, DataType, Unit        string
	Mutability                   string
}

// DeviceModel is the simulator's inventory, reported in two NotifyReport
// parts for GetBaseReport.
var DeviceModel = []DMVariable{
	{Component: "OCPPCommCtrlr", Variable: "HeartbeatInterval", Value: "300", DataType: "integer", Unit: "s", Mutability: "ReadWrite"},
	{Component: "SecurityCtrlr", Variable: "SecurityProfile", Value: "1", DataType: "integer", Mutability: "ReadOnly"},
	{Component: "ChargingStation", Variable: "Available", Value: "true", DataType: "boolean", Mutability: "ReadOnly"},
	{Component: "EVSE", EVSE: 1, Variable: "Power", Value: "22000", DataType: "decimal", Unit: "W", Mutability: "ReadOnly"},
	{Component: "Connector", EVSE: 1, Connector: 1, Variable: "ConnectorType", Value: "cType2", DataType: "string", Mutability: "ReadOnly"},
	{Component: "TemperatureSensor", ComponentInstance: "main", Variable: "Temperature", Value: "35.5", DataType: "decimal", Unit: "Celsius", Mutability: "ReadOnly"},
}

type installed struct {
	typ  string
	cert *x509.Certificate
}

type extState struct {
	monitors map[int]DMVariable
	monSeq   int
	certs    []installed
	signed   chan string
	signedMu sync.Mutex
}

func (cp *ChargePoint) signedChan() chan string {
	cp.ext.signedMu.Lock()
	defer cp.ext.signedMu.Unlock()
	if cp.ext.signed == nil {
		cp.ext.signed = make(chan string, 8)
	}
	return cp.ext.signed
}

// Signed returns the next certificate chain the CSMS sent with
// CertificateSigned (or in a DataTransfer CertificateSigned).
func (cp *ChargePoint) Signed(ctx context.Context) (string, error) {
	select {
	case c := <-cp.signedChan():
		return c, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// BootInterval is the interval of the last BootNotification answer.
func (cp *ChargePoint) BootInterval() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.bootInterval
}

// CSR makes a PKCS#10 request for cn (PEM).
func CSR(cn string) string {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn, Organization: []string{"SigmaSim"}}}, key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// SignCertificate sends SignCertificate with a CSR for cn (the charger's
// identity when empty) and returns the answer's status. certType is
// ChargingStationCertificate, V2GCertificate or "" (1.6: ignored).
func (cp *ChargePoint) SignCertificate(ctx context.Context, cn, certType string) (string, error) {
	if cn == "" {
		cn = cp.cfg.ID
	}
	p := map[string]any{"csr": CSR(cn)}
	if !cp.v16() && certType != "" {
		p["certificateType"] = certType
	}
	var r struct {
		Status string `json:"status"`
	}
	err := cp.callInto(ctx, "SignCertificate", p, &r)
	return r.Status, err
}

// ReservationStatus sends a 2.0.1 ReservationStatusUpdate.
func (cp *ChargePoint) ReservationStatus(ctx context.Context, id int, status string) error {
	return cp.callInto(ctx, "ReservationStatusUpdate", map[string]any{"reservationId": id, "reservationUpdateStatus": status}, nil)
}

// NotifyEvent sends a 2.0.1 NotifyEvent for a device model variable.
func (cp *ChargePoint) NotifyEvent(ctx context.Context, v DMVariable, value string, monitorID *int) error {
	e := map[string]any{"eventId": cp.nextSeq(), "timestamp": ts(), "trigger": "Alerting", "actualValue": value,
		"component": dmComponent(v), "variable": map[string]any{"name": v.Variable}, "eventNotificationType": "HardWiredNotification"}
	if monitorID != nil {
		e["variableMonitoringId"] = *monitorID
		e["eventNotificationType"] = "CustomMonitor"
	}
	return cp.callInto(ctx, "NotifyEvent", map[string]any{"generatedAt": ts(), "seqNo": 0, "eventData": []any{e}}, nil)
}

func dmComponent(v DMVariable) map[string]any {
	c := map[string]any{"name": v.Component}
	if v.ComponentInstance != "" {
		c["instance"] = v.ComponentInstance
	}
	if v.EVSE > 0 {
		e := map[string]any{"id": v.EVSE}
		if v.Connector > 0 {
			e["connectorId"] = v.Connector
		}
		c["evse"] = e
	}
	return c
}

func reportData(v DMVariable) map[string]any {
	attr := map[string]any{"type": "Actual", "value": v.Value, "mutability": v.Mutability}
	ch := map[string]any{"dataType": v.DataType, "supportsMonitoring": v.DataType == "decimal" || v.DataType == "integer"}
	if v.Unit != "" {
		ch["unit"] = v.Unit
	}
	return map[string]any{"component": dmComponent(v), "variable": map[string]any{"name": v.Variable},
		"variableAttribute": []any{attr}, "variableCharacteristics": ch}
}

// report sends vars in NotifyReport parts of size per.
func (cp *ChargePoint) report(ctx context.Context, requestID any, vars []DMVariable, per int) {
	seq := 0
	for i := 0; i < len(vars); i += per {
		end := min(i+per, len(vars))
		var rd []any
		for _, v := range vars[i:end] {
			rd = append(rd, reportData(v))
		}
		p := map[string]any{"requestId": requestID, "generatedAt": ts(), "seqNo": seq, "reportData": rd}
		if end < len(vars) {
			p["tbc"] = true
		}
		_ = cp.callInto(ctx, "NotifyReport", p, nil)
		seq++
	}
}

func findVar(c, v map[string]any) (DMVariable, string) {
	status := "UnknownComponent"
	for _, x := range DeviceModel {
		if x.Component != fmt.Sprint(c["name"]) {
			continue
		}
		status = "UnknownVariable"
		if v == nil || x.Variable == fmt.Sprint(v["name"]) {
			return x, "Accepted"
		}
	}
	return DMVariable{}, status
}

// hashData is the certificate's OCPP hash data (SHA-256).
func hashData(c *x509.Certificate) map[string]any {
	nh := sha256.Sum256(c.RawIssuer)
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	_, _ = asn1.Unmarshal(c.RawSubjectPublicKeyInfo, &spki)
	kh := sha256.Sum256(spki.PublicKey.Bytes)
	return map[string]any{"hashAlgorithm": "SHA256", "issuerNameHash": hex.EncodeToString(nh[:]),
		"issuerKeyHash": hex.EncodeToString(kh[:]), "serialNumber": c.SerialNumber.Text(16)}
}

func parsePEM(s string) []*x509.Certificate {
	var out []*x509.Certificate
	rest := []byte(s)
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return out
		}
		if c, err := x509.ParseCertificate(b.Bytes); err == nil {
			out = append(out, c)
		}
	}
}

func (cp *ChargePoint) handleExt(action string, p map[string]any) (any, bool) {
	status := func(def string) map[string]any { return map[string]any{"status": cp.answer(action, def)} }
	switch action {
	case "GetBaseReport":
		res := status("Accepted")
		if res["status"] == "Accepted" {
			rid := p["requestId"]
			cp.later(func(ctx context.Context) { cp.report(ctx, rid, DeviceModel, 3) })
		}
		return res, true
	case "GetReport":
		var vars []DMVariable
		cvs, _ := p["componentVariable"].([]any)
		for _, x := range cvs {
			m, _ := x.(map[string]any)
			c, _ := m["component"].(map[string]any)
			v, _ := m["variable"].(map[string]any)
			for _, dv := range DeviceModel {
				if dv.Component == fmt.Sprint(c["name"]) && (v == nil || dv.Variable == fmt.Sprint(v["name"])) {
					vars = append(vars, dv)
				}
			}
		}
		if len(cvs) == 0 {
			vars = DeviceModel
		}
		if len(vars) == 0 {
			return map[string]any{"status": "EmptyResultSet"}, true
		}
		rid := p["requestId"]
		cp.later(func(ctx context.Context) { cp.report(ctx, rid, vars, 10) })
		return status("Accepted"), true
	case "SetVariableMonitoring":
		var out []any
		type fire struct {
			v  DMVariable
			id int
			at float64
		}
		var fires []fire
		data, _ := p["setMonitoringData"].([]any)
		for _, x := range data {
			m, _ := x.(map[string]any)
			c, _ := m["component"].(map[string]any)
			v, _ := m["variable"].(map[string]any)
			dv, st := findVar(c, v)
			r := map[string]any{"status": st, "type": m["type"], "severity": m["severity"], "component": c, "variable": v}
			if st == "Accepted" {
				cp.mu.Lock()
				if cp.ext.monitors == nil {
					cp.ext.monitors = map[int]DMVariable{}
				}
				cp.ext.monSeq++
				id := cp.ext.monSeq
				cp.ext.monitors[id] = dv
				cp.mu.Unlock()
				r["id"] = id
				f, _ := m["value"].(float64)
				fires = append(fires, fire{dv, id, f + 1})
			}
			out = append(out, r)
		}
		cp.later(func(ctx context.Context) {
			for _, f := range fires {
				id := f.id
				_ = cp.NotifyEvent(ctx, f.v, strconv.FormatFloat(f.at, 'f', -1, 64), &id)
			}
		})
		return map[string]any{"setMonitoringResult": out}, true
	case "GetMonitoringReport":
		cp.mu.Lock()
		var mons []any
		for id, v := range cp.ext.monitors {
			mons = append(mons, map[string]any{"component": dmComponent(v), "variable": map[string]any{"name": v.Variable},
				"variableMonitoring": []any{map[string]any{"id": id, "transaction": false, "value": 40, "type": "UpperThreshold", "severity": 4}}})
		}
		cp.mu.Unlock()
		if len(mons) == 0 {
			return map[string]any{"status": "EmptyResultSet"}, true
		}
		rid := p["requestId"]
		cp.later(func(ctx context.Context) {
			_ = cp.callInto(ctx, "NotifyMonitoringReport", map[string]any{"requestId": rid, "seqNo": 0, "generatedAt": ts(), "monitor": mons}, nil)
		})
		return status("Accepted"), true
	case "SetMonitoringBase", "SetMonitoringLevel":
		return status("Accepted"), true
	case "GetChargingProfiles":
		cp.mu.Lock()
		var profs []any
		evse := 0
		for k, v := range cp.profiles {
			var x any
			_ = json.Unmarshal(v, &x)
			profs = append(profs, x)
			evse = k
		}
		cp.mu.Unlock()
		if len(profs) == 0 {
			return map[string]any{"status": "NoProfiles"}, true
		}
		rid := p["requestId"]
		cp.later(func(ctx context.Context) {
			_ = cp.callInto(ctx, "ReportChargingProfiles", map[string]any{"requestId": rid, "chargingLimitSource": "CSO", "evseId": evse, "chargingProfile": profs}, nil)
		})
		return status("Accepted"), true
	case "ClearVariableMonitoring":
		var out []any
		ids, _ := p["id"].([]any)
		for _, x := range ids {
			id := num(x)
			cp.mu.Lock()
			_, ok := cp.ext.monitors[id]
			delete(cp.ext.monitors, id)
			cp.mu.Unlock()
			st := "NotFound"
			if ok {
				st = "Accepted"
			}
			out = append(out, map[string]any{"id": id, "status": st})
		}
		return map[string]any{"clearMonitoringResult": out}, true
	case "SendLocalList":
		res := status("Accepted")
		if res["status"] == "Accepted" {
			cp.mu.Lock()
			if cp.v16() {
				cp.listVer = num(p["listVersion"])
			} else {
				cp.listVer = num(p["versionNumber"])
			}
			cp.mu.Unlock()
		}
		return res, true
	case "GetLocalListVersion":
		cp.mu.Lock()
		defer cp.mu.Unlock()
		if cp.v16() {
			return map[string]any{"listVersion": cp.listVer}, true
		}
		return map[string]any{"versionNumber": cp.listVer}, true
	case "GetCompositeSchedule":
		if cp.v16() {
			return nil, false
		}
		cp.mu.Lock()
		prof, ok := cp.profiles[num(p["evseId"])]
		cp.mu.Unlock()
		if !ok {
			return map[string]any{"status": "Rejected"}, true
		}
		var cpf struct {
			ChargingSchedule []struct {
				ChargingRateUnit       string          `json:"chargingRateUnit"`
				ChargingSchedulePeriod json.RawMessage `json:"chargingSchedulePeriod"`
			} `json:"chargingSchedule"`
		}
		_ = json.Unmarshal(prof, &cpf)
		if len(cpf.ChargingSchedule) == 0 {
			return map[string]any{"status": "Rejected"}, true
		}
		cs := cpf.ChargingSchedule[0]
		return map[string]any{"status": "Accepted", "schedule": map[string]any{"evseId": num(p["evseId"]), "duration": num(p["duration"]),
			"scheduleStart": ts(), "chargingRateUnit": cs.ChargingRateUnit, "chargingSchedulePeriod": cs.ChargingSchedulePeriod}}, true
	case "ClearChargingProfile":
		n := -1
		if cp.v16() {
			if c, ok := p["connectorId"]; ok {
				n = num(c)
			}
		} else if crit, ok := p["chargingProfileCriteria"].(map[string]any); ok {
			if e, ok := crit["evseId"]; ok {
				n = num(e)
			}
		}
		cp.mu.Lock()
		defer cp.mu.Unlock()
		found := false
		for k := range cp.profiles {
			if n < 0 || k == n {
				delete(cp.profiles, k)
				found = true
			}
		}
		if !found {
			return map[string]any{"status": "Unknown"}, true
		}
		return map[string]any{"status": "Accepted"}, true
	case "InstallCertificate":
		res := status("Accepted")
		certs := parsePEM(fmt.Sprint(p["certificate"]))
		if len(certs) == 0 {
			return map[string]any{"status": "Rejected"}, true
		}
		if res["status"] == "Accepted" {
			cp.mu.Lock()
			cp.ext.certs = append(cp.ext.certs, installed{typ: fmt.Sprint(p["certificateType"]), cert: certs[0]})
			cp.mu.Unlock()
		}
		return res, true
	case "GetInstalledCertificateIds":
		want := map[string]bool{}
		switch t := p["certificateType"].(type) {
		case string:
			want[t] = true
		case []any:
			for _, x := range t {
				want[fmt.Sprint(x)] = true
			}
		}
		cp.mu.Lock()
		defer cp.mu.Unlock()
		var list []any
		for _, c := range cp.ext.certs {
			if len(want) > 0 && !want[c.typ] {
				continue
			}
			if cp.v16() {
				list = append(list, hashData(c.cert))
			} else {
				list = append(list, map[string]any{"certificateType": c.typ, "certificateHashData": hashData(c.cert)})
			}
		}
		if len(list) == 0 {
			return map[string]any{"status": "NotFound"}, true
		}
		key := "certificateHashDataChain"
		if cp.v16() {
			key = "certificateHashData"
		}
		return map[string]any{"status": "Accepted", key: list}, true
	case "DeleteCertificate":
		h, _ := p["certificateHashData"].(map[string]any)
		cp.mu.Lock()
		defer cp.mu.Unlock()
		for i, c := range cp.ext.certs {
			hd := hashData(c.cert)
			if hd["serialNumber"] == h["serialNumber"] && hd["issuerNameHash"] == h["issuerNameHash"] {
				cp.ext.certs = append(cp.ext.certs[:i], cp.ext.certs[i+1:]...)
				return map[string]any{"status": "Accepted"}, true
			}
		}
		return map[string]any{"status": "NotFound"}, true
	case "CertificateSigned":
		return cp.certificateSigned(fmt.Sprint(p["certificateChain"])), true
	case "DataTransfer":
		if p["vendorId"] == "org.openchargealliance.iso15118pnc" && p["messageId"] == "CertificateSigned" {
			var d struct {
				CertificateChain string `json:"certificateChain"`
			}
			s, _ := p["data"].(string)
			_ = json.Unmarshal([]byte(s), &d)
			r := cp.certificateSigned(d.CertificateChain)
			b, _ := json.Marshal(r)
			return map[string]any{"status": "Accepted", "data": string(b)}, true
		}
	case "ExtendedTriggerMessage":
		res := status("Accepted")
		if res["status"] == "Accepted" {
			msg, _ := p["requestedMessage"].(string)
			cp.later(func(ctx context.Context) {
				switch msg {
				case "SignChargePointCertificate":
					_, _ = cp.SignCertificate(ctx, "", "")
				case "Heartbeat":
					_ = cp.Heartbeat(ctx)
				case "BootNotification":
					_, _ = cp.Boot(ctx)
				}
			})
		}
		return res, true
	case "SignedUpdateFirmware":
		rid := p["requestId"]
		cp.later(func(ctx context.Context) {
			for _, s := range []string{"Downloading", "Downloaded", "SignatureVerified", "Installing", "Installed"} {
				_ = cp.callInto(ctx, "SignedFirmwareStatusNotification", map[string]any{"status": s, "requestId": rid}, nil)
			}
		})
		return status("Accepted"), true
	}
	return nil, false
}

func (cp *ChargePoint) certificateSigned(chain string) map[string]any {
	certs := parsePEM(chain)
	if len(certs) == 0 || time.Now().After(certs[0].NotAfter) {
		return map[string]any{"status": "Rejected"}
	}
	st := cp.answer("CertificateSigned", "Accepted")
	select {
	case cp.signedChan() <- chain:
	default:
	}
	return map[string]any{"status": st}
}
