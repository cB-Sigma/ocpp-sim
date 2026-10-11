// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

// Command ocppsim runs one simulated OCPP charge point (1.6J, 2.0.1 or
// 2.1) against a central system: it boots, reports its connectors and
// sends heartbeats until stopped, and with -session runs one charging
// session first. With -soap it is an OCPP-S 1.6 charger, strict about
// the WSDLs unless -soap-lenient is given.
//
//	ocppsim -url ws://127.0.0.1:8887/ocpp/ -id CP1 -version 2.1 -session
//	ocppsim -soap -url http://127.0.0.1:8887/ocpp16s/ -id CP1 -version 1.6
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/cB-Sigma/ocpp"
	"github.com/cB-Sigma/ocpp-sim/sim"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ocppsim:", err)
		os.Exit(1)
	}
}

func versionOf(v string) (string, error) {
	switch strings.TrimPrefix(strings.ToLower(v), "ocpp") {
	case "1.6", "1.6j":
		return ocpp.V16, nil
	case "2.0.1":
		return ocpp.V201, nil
	case "2.1":
		return ocpp.V21, nil
	}
	return "", fmt.Errorf("version %q is not 1.6, 2.0.1 or 2.1", v)
}

func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("ocppsim", flag.ContinueOnError)
	url := fs.String("url", "ws://127.0.0.1:8887/ocpp/", "central system endpoint without the identity")
	id := fs.String("id", "SIM1", "charge point identity")
	ver := fs.String("version", "1.6", "OCPP version: 1.6, 2.0.1 or 2.1")
	password := fs.String("password", "", "basic authentication password (security profiles 1 and 2)")
	insecure := fs.Bool("insecure", false, "do not verify the central system's TLS certificate (wss:// test setups)")
	connectors := fs.Int("connectors", 2, "number of connectors (EVSEs for 2.x)")
	session := fs.Bool("session", false, "run one charging session after booting")
	tag := fs.String("tag", "DEMO0001", "id tag of the session")
	heartbeat := fs.Duration("heartbeat", 0, "heartbeat interval (default: the interval the central system gives)")
	duration := fs.Duration("for", 0, "stop after this long (default: until interrupted)")
	soap := fs.Bool("soap", false, "speak OCPP-S 1.6 (SOAP): -url is the Central System service URL")
	soapListen := fs.String("soap-listen", "127.0.0.1:0", "OCPP-S: address the charger serves its Charge Point service on")
	soapFrom := fs.String("soap-from", "", "OCPP-S: the charger's address given in From (default: http://<soap-listen>/)")
	soapLenient := fs.Bool("soap-lenient", false, "OCPP-S: take messages that do not follow the WSDLs (strict by default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	v, err := versionOf(*ver)
	if err != nil {
		return err
	}
	cfg := sim.Config{URL: *url, ID: *id, Version: v, Password: *password, Connectors: *connectors, Timeout: 10 * time.Second,
		SOAP: *soap, SOAPListen: *soapListen, SOAPFrom: *soapFrom, SOAPLenient: *soapLenient}
	if *insecure {
		cfg.TLS = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- opt-in for test setups
	}
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	cp := sim.New(cfg)
	if err := cp.Connect(ctx); err != nil {
		return err
	}
	defer cp.Disconnect()
	status, err := cp.Boot(ctx)
	if err != nil {
		return fmt.Errorf("boot: %w", err)
	}
	fmt.Fprintf(out, "%s (%s) booted: %s\n", *id, v, status)
	if *session {
		if err := runSession(ctx, cp, v, *tag, out); err != nil {
			return err
		}
	}
	every := *heartbeat
	if every <= 0 {
		every = time.Duration(cp.BootInterval()) * time.Second
	}
	if every <= 0 {
		every = 300 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-cp.Done():
			return errors.New("the central system closed the connection")
		case <-t.C:
			if err := cp.Heartbeat(ctx); err != nil && ctx.Err() == nil {
				return fmt.Errorf("heartbeat: %w", err)
			}
		}
	}
}

func runSession(ctx context.Context, cp *sim.ChargePoint, version, tag string, out io.Writer) error {
	auth, err := cp.Authorize(ctx, tag)
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}
	fmt.Fprintf(out, "authorize %s: %s\n", tag, auth)
	if version == ocpp.V16 {
		if err := cp.Status(ctx, 1, "Preparing", ""); err != nil {
			return err
		}
	} else if err := cp.Status(ctx, 1, "Occupied", ""); err != nil {
		return err
	}
	tx, st, err := cp.Start(ctx, 1, tag, 1000)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	fmt.Fprintf(out, "transaction %s started: %s\n", tx, st)
	if err := cp.Meter(ctx, 1, 4000, 7400, [3]float64{230, 231, 229}); err != nil {
		return fmt.Errorf("meter values: %w", err)
	}
	if err := cp.Stop(ctx, 1, 8000, "Local"); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	fmt.Fprintf(out, "transaction %s stopped\n", tx)
	return cp.Status(ctx, 1, "Available", "")
}
