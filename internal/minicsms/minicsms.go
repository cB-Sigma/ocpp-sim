// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

// Package minicsms is a minimal central system for the module's own tests
// and examples, built on the ocpp module's csms package: it accepts OCPP
// 1.6J, 2.0.1 and 2.1 connections, validates every request against the
// OCA schemas, answers the core messages, and records what it was sent.
// It is not a CSMS.
package minicsms

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/cB-Sigma/ocpp"
	"github.com/cB-Sigma/ocpp/csms"
)

// CSMS is an http.Handler serving <path><identity>.
type CSMS struct {
	// OnCall, when set, sees every CALL before it is answered.
	OnCall func(id, action string, payload json.RawMessage)

	srv    *csms.Server
	mu     sync.Mutex
	seen   []string // "<identity> <version> <action>"
	nextTx int
}

// New makes a CSMS.
func New() *CSMS {
	s := &CSMS{}
	s.srv = csms.New(csms.Config{Timeout: 5 * time.Second})
	for _, set := range []map[string]bool{actions(ocpp.V16In), actions(ocpp.V201In), ocpp.In21, ocpp.SendActions21} {
		for a := range set {
			s.srv.Handle(a, s.handle)
		}
	}
	return s
}

func actions(m map[string]ocpp.MsgDef) map[string]bool {
	out := make(map[string]bool, len(m))
	for a := range m {
		out[a] = true
	}
	return out
}

// Seen lists "<identity> <version> <action>" for every CALL received.
func (s *CSMS) Seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// Conn is the connection of a charger, or nil.
func (s *CSMS) Conn(id string) *ocpp.Conn {
	if st := s.srv.Station(id); st != nil {
		return st.Conn()
	}
	return nil
}

func (s *CSMS) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.srv.ServeHTTP(w, r) }

func (s *CSMS) handle(ctx context.Context, st *csms.Station, payload json.RawMessage) (any, error) {
	id, version, action := st.ID(), st.Version(), ocpp.ActionOf(ctx)
	s.mu.Lock()
	s.seen = append(s.seen, id+" "+version+" "+action)
	s.nextTx++
	tx := s.nextTx
	s.mu.Unlock()
	if s.OnCall != nil {
		s.OnCall(id, action, payload)
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
	case "DataTransfer":
		res = map[string]any{"status": "UnknownVendorId"}
	}
	return res, nil // csms checks it against the response schema
}
