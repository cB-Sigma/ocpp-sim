package sim

import (
	"context"
	"errors"
	"time"

	"github.com/cB-Sigma/ocpp"
)

// OCPP 2.x additions of the simulator: answers to the CSMS messages of
// OCPP 2.1 (and those 2.0.1 shares that the 2.0.1 simulator did not
// answer), SEND messages, and a record of every CSMS message that did not
// match its OCA schema.

func (cp *ChargePoint) violation(err error) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.violations = append(cp.violations, err)
}

// Violations are the CSMS requests and answers that did not match their
// OCA schema (2.0.1 and 2.1).
func (cp *ChargePoint) Violations() []error {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]error(nil), cp.violations...)
}

// Send sends a SEND message (OCPP 2.1), which the CSMS never answers.
func (cp *ChargePoint) Send(ctx context.Context, action string, payload any) error {
	cp.mu.Lock()
	c := cp.conn
	cp.mu.Unlock()
	if c == nil {
		return errors.New("not connected")
	}
	return c.Send(ctx, action, payload)
}

// Raw writes a raw frame (for RPC framework tests).
func (cp *ChargePoint) Raw(ctx context.Context, frame string) error {
	cp.mu.Lock()
	c := cp.conn
	cp.mu.Unlock()
	if c == nil {
		return errors.New("not connected")
	}
	return c.WriteRaw(ctx, []byte(frame))
}

// handle2x answers the CSMS messages the 2.0.1 simulator did not know.
// Answers follow the station behaviour of part 2; Answer(action, status)
// overrides the status.
func (cp *ChargePoint) handle2x(action string, p map[string]any) (any, bool) {
	status := func(def string) map[string]any { return map[string]any{"status": cp.answer(action, def)} }
	switch action {
	case "SetDefaultTariff", "ChangeTransactionTariff", "AFRRSignal", "AdjustPeriodicEventStream", "SetDERControl",
		"ClearDERControl", "NotifyAllowedEnergyTransfer", "UpdateDynamicSchedule", "UsePriorityCharging",
		"SetNetworkProfile", "SetDisplayMessage", "ClearDisplayMessage", "PublishFirmware":
		return status("Accepted"), true
	case "CostUpdated", "NotifyWebPaymentStarted":
		return map[string]any{}, true
	case "UnpublishFirmware":
		return status("Unpublished"), true
	case "GetTariffs":
		return map[string]any{"status": cp.answer(action, "NoTariff")}, true
	case "ClearTariffs":
		var res []any
		for _, id := range anyList(p["tariffIds"]) {
			res = append(res, map[string]any{"tariffId": id, "status": cp.answer(action, "Accepted")})
		}
		if len(res) == 0 {
			res = []any{map[string]any{"status": cp.answer(action, "NoTariff")}}
		}
		return map[string]any{"clearTariffsResult": res}, true
	case "GetPeriodicEventStream":
		return map[string]any{}, true
	case "GetDERControl":
		res := status("Accepted")
		if res["status"] == "Accepted" {
			rid := p["requestId"]
			cp.later(func(ctx context.Context) {
				_ = cp.callInto(ctx, "ReportDERControl", map[string]any{"requestId": rid}, nil)
			})
		}
		return res, true
	case "RequestBatterySwap":
		res := status("Accepted")
		if res["status"] == "Accepted" {
			rid, tok := p["requestId"], p["idToken"]
			cp.later(func(ctx context.Context) {
				for _, ev := range []string{"BatteryIn", "BatteryOut"} {
					_ = cp.callInto(ctx, "BatterySwap", map[string]any{"eventType": ev, "requestId": rid, "idToken": tok,
						"batteryData": []any{map[string]any{"evseId": 1, "serialNumber": "BAT-1", "soC": 20, "soH": 97}}}, nil)
				}
			})
		}
		return res, true
	case "GetTransactionStatus":
		res := map[string]any{"messagesInQueue": false}
		if _, ok := p["transactionId"]; ok {
			res["ongoingIndicator"] = cp.answer(action, "") == "ongoing"
		}
		return res, true
	case "CustomerInformation":
		res := status("Accepted")
		if res["status"] == "Accepted" && p["report"] == true {
			rid := p["requestId"]
			cp.later(func(ctx context.Context) {
				_ = cp.callInto(ctx, "NotifyCustomerInformation", map[string]any{"data": "no data held", "seqNo": 0,
					"generatedAt": ts(), "requestId": rid}, nil)
			})
		}
		return res, true
	case "GetDisplayMessages":
		res := status("Accepted")
		if res["status"] == "Accepted" {
			rid := p["requestId"]
			cp.later(func(ctx context.Context) {
				_ = cp.callInto(ctx, "NotifyDisplayMessages", map[string]any{"requestId": rid, "messageInfo": []any{
					map[string]any{"id": 1, "priority": "NormalCycle", "message": map[string]any{"format": "UTF8", "content": "Welcome"}}}}, nil)
			})
		}
		return res, true
	}
	return nil, false
}

func anyList(v any) []any { l, _ := v.([]any); return l }

// Periodic event streams (OCPP 2.1 N11 to N15).

// OpenStream opens a periodic event stream for a monitor.
func (cp *ChargePoint) OpenStream(ctx context.Context, id, monitorID, interval int) (string, error) {
	var r struct {
		Status string `json:"status"`
	}
	err := cp.callInto(ctx, "OpenPeriodicEventStream", map[string]any{"constantStreamData": map[string]any{
		"id": id, "variableMonitoringId": monitorID, "params": map[string]any{"interval": interval}}}, &r)
	return r.Status, err
}

// StreamValues sends values of a stream with a SEND (t: seconds after base).
func (cp *ChargePoint) StreamValues(ctx context.Context, id int, base time.Time, values map[float64]string) error {
	var data []any
	for t, v := range values {
		data = append(data, map[string]any{"t": t, "v": v})
	}
	return cp.Send(ctx, "NotifyPeriodicEventStream", map[string]any{"id": id, "pending": 0,
		"basetime": base.UTC().Format(time.RFC3339), "data": data})
}

var _ = ocpp.V21
