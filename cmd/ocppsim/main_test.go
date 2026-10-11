// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cB-Sigma/ocpp-sim/internal/minicsms"
)

func TestRunSession(t *testing.T) {
	csms := minicsms.New()
	srv := httptest.NewServer(csms)
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ocpp/"
	for _, v := range []string{"1.6", "2.0.1", "2.1"} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var out bytes.Buffer
		id := "CMD" + strings.ReplaceAll(v, ".", "")
		err := run(ctx, []string{"-url", url, "-id", id, "-version", v, "-session", "-for", "500ms", "-heartbeat", "100ms"}, &out)
		cancel()
		if err != nil {
			t.Fatalf("%s: %v\n%s", v, err, out.String())
		}
		for _, want := range []string{"booted: Accepted", "authorize DEMO0001: Accepted", "started", "stopped"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s: output lacks %q:\n%s", v, want, out.String())
			}
		}
	}
	seen := strings.Join(csms.Seen(), "\n")
	for _, want := range []string{"CMD16 ocpp1.6 StartTransaction", "CMD16 ocpp1.6 StopTransaction", "CMD201 ocpp2.0.1 TransactionEvent", "CMD21 ocpp2.1 Heartbeat"} {
		if !strings.Contains(seen, want) {
			t.Errorf("the central system did not see %q:\n%s", want, seen)
		}
	}
	if err := run(context.Background(), []string{"-version", "1.5"}, &bytes.Buffer{}); err == nil {
		t.Error("version 1.5 accepted")
	}
}
