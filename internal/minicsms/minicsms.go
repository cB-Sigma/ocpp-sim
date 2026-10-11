// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

// Package minicsms is a minimal central system for the module's own tests
// and examples: it accepts OCPP 1.6J, 2.0.1 and 2.1 connections, validates
// every request against the OCA schemas, answers the core messages, and
// records what it was sent. It is not a CSMS.
package minicsms

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/cB-Sigma/ocpp"
)

// CSMS is an http.Handler serving <path><identity>.
type CSMS struct {
	// OnCall, when set, sees every CALL before it is answered.
	OnCall func(id, action string, payload json.RawMessage)

	mu     sync.Mutex
	seen   []string // "<identity> <version> <action>"
	conns  map[string]*ocpp.Conn
	nextTx int
}

// New makes a CSMS.
func New() *CSMS { return &CSMS{conns: map[string]*ocpp.Conn{}} }

// Seen lists "<identity> <version> <action>" for every CALL received.
func (s *CSMS) Seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// Conn is the connection of a charger, or nil.
func (s *CSMS) Conn(id string) *ocpp.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[id]
}

func (s *CSMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: ocpp.Subprotocols([]string{ocpp.V21, ocpp.V201, ocpp.V16})})
	if err != nil {
		return
	}
	version := ocpp.VersionOf(ws.Subprotocol())
	if version == "" {
		_ = ws.Close(websocket.StatusPolicyViolation, "no OCPP subprotocol")
		return
	}
	c := ocpp.NewConn(ws, version, 5*time.Second, nil)
	c.SetHandler(func(ctx context.Context, action string, payload json.RawMessage) (any, error) {
		return s.handle(id, version, action, payload)
	})
	s.mu.Lock()
	s.conns[id] = c
	s.mu.Unlock()
	_ = c.Serve(r.Context(), 0)
}

func (s *CSMS) handle(id, version, action string, payload json.RawMessage) (any, error) {
	s.mu.Lock()
	s.seen = append(s.seen, id+" "+version+" "+action)
	s.nextTx++
	tx := s.nextTx
	s.mu.Unlock()
	if s.OnCall != nil {
		s.OnCall(id, action, payload)
	}
	if version == ocpp.V16 {
		md, ok := ocpp.V16In[action]
		if !ok {
			return nil, ocpp.RPCErrorf(ocpp.KindNotImplemented, "%s is not handled", action)
		}
		if _, verr := ocpp.DecodeIn(version, action, md, payload); verr != nil {
			return nil, verr
		}
	} else if _, verr := ocpp.ValidateJSON(version, action, false, payload); verr != nil {
		return nil, verr
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var res any = map[string]any{}
	switch action {
	case "BootNotification":
		res = map[string]any{"status": "Accepted", "currentTime": now, "interval": 300}
	case "Heartbeat":
		res = map[string]any{"currentTime": now}
	case "Authorize":
		if version == ocpp.V16 {
			res = map[string]any{"idTagInfo": map[string]any{"status": "Accepted"}}
		} else {
			res = map[string]any{"idTokenInfo": map[string]any{"status": "Accepted"}}
		}
	case "StartTransaction":
		res = map[string]any{"transactionId": tx, "idTagInfo": map[string]any{"status": "Accepted"}}
	case "StopTransaction":
		res = map[string]any{}
	case "DataTransfer":
		res = map[string]any{"status": "UnknownVendorId"}
	}
	if version == ocpp.V16 {
		return res, nil
	}
	return ocpp.CheckAnswer(version, action, res, payload)
}
