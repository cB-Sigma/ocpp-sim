// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cB-Sigma/ocpp"
)

// OCPP-S 1.6 charger: Central System service requests are POSTed to
// Config.URL, and the charger serves the Charge Point service itself.
// Unless Config.SOAPLenient is set, everything it receives is checked
// against the embedded OCA WSDLs (ocpp.CheckSOAPEnvelope), so a central
// system that writes elements out of the WSDL sequence fails against the
// simulator as it would against a strict charger.

func (cp *ChargePoint) connectSOAP() error {
	if cp.cfg.Version != ocpp.V16 {
		return fmt.Errorf("OCPP-S needs version %s, not %s", ocpp.V16, cp.cfg.Version)
	}
	addr := cp.cfg.SOAPListen
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	from := cp.cfg.SOAPFrom
	if from == "" {
		from = "http://" + ln.Addr().String() + "/"
	}
	srv := &http.Server{Handler: http.HandlerFunc(cp.serveSOAP), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	base := http.DefaultTransport.(*http.Transport).Clone()
	if cp.cfg.TLS != nil {
		base.TLSClientConfig = cp.cfg.TLS
	}
	client := &http.Client{Timeout: cp.cfg.Timeout, Transport: &soapTransport{cp: cp, base: base}}
	c := ocpp.NewSenderConn(ocpp.V16, cp.cfg.Timeout, func(ctx context.Context, action string, payload json.RawMessage) (json.RawMessage, error) {
		return ocpp.SOAPCallCentralSystem(ctx, client, cp.cfg.ID, from, cp.cfg.URL, action, payload)
	})
	cp.mu.Lock()
	cp.conn, cp.cancel = c, func() { _ = srv.Close() }
	cp.mu.Unlock()
	return nil
}

// soapTransport adds basic authentication and checks the Central
// System's answers.
type soapTransport struct {
	cp   *ChargePoint
	base http.RoundTripper
}

func (t *soapTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.cp.mu.Lock()
	pw := t.cp.cfg.Password
	t.cp.mu.Unlock()
	if pw != "" {
		r = r.Clone(r.Context())
		r.SetBasicAuth(t.cp.cfg.ID, pw)
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil || t.cp.cfg.SOAPLenient {
		return resp, err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if cerr := ocpp.CheckSOAPEnvelope(raw); cerr != nil {
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		action := strings.TrimPrefix(params["action"], "/")
		t.cp.violation(fmt.Errorf("Central System answer to %s: %w", action, cerr))
		// The charger's SOAP stack refuses the answer; the call fails
		// as a fault with the reason.
		rec := &faultWriter{header: http.Header{}}
		ocpp.WriteSOAPFault(rec, "", "Sender", "ProtocolError", "the answer does not follow the WSDL: "+cerr.Error())
		resp.StatusCode, resp.Status = rec.status, http.StatusText(rec.status)
		resp.Body = io.NopCloser(&rec.body)
		resp.ContentLength = int64(rec.body.Len())
	}
	return resp, nil
}

// faultWriter keeps a fault written by ocpp.WriteSOAPFault.
type faultWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (f *faultWriter) Header() http.Header         { return f.header }
func (f *faultWriter) WriteHeader(s int)           { f.status = s }
func (f *faultWriter) Write(b []byte) (int, error) { return f.body.Write(b) }

// serveSOAP answers the Central System's Charge Point service requests.
func (cp *ChargePoint) serveSOAP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "OCPP-S requests are POSTed", http.StatusMethodNotAllowed)
		return
	}
	q, f := ocpp.ReadSOAPRequest(r)
	if f != nil {
		f.Write(w)
		return
	}
	if q.Identity != cp.cfg.ID {
		ocpp.WriteSOAPFault(w, q.MessageID, "Sender", "IdentityMismatch", "this charge point is "+cp.cfg.ID)
		return
	}
	action, f := q.Action()
	if f != nil {
		f.Write(w)
		return
	}
	if !cp.cfg.SOAPLenient {
		if err := q.CheckWSDL(); err != nil {
			cp.violation(fmt.Errorf("Central System request %s: %w", action, err))
			ocpp.WriteSOAPFault(w, q.MessageID, "Sender", "ProtocolError", "the request does not follow the WSDL: "+err.Error())
			return
		}
	}
	pl, f := q.Payload(action)
	if f != nil {
		f.Write(w)
		return
	}
	res, err := cp.handle(r.Context(), action, pl)
	if err != nil {
		sub := "InternalError"
		var ce *ocpp.CallError
		if errors.As(err, &ce) && (ce.Code == "NotImplemented" || ce.Code == "NotSupported") {
			sub = "NotSupported"
		}
		ocpp.WriteSOAPFault(w, q.MessageID, "Receiver", sub, err.Error())
		return
	}
	ocpp.WriteChargePointSOAPResponse(w, q.MessageID, action, res)
}
