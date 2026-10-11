// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package interop

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// image is the reference peer's image, built from ./peer.
const image = "sigma-ocpp-sim-interop:mobilityhouse-ocpp-2.1.0"

// envVar turns the Docker tests on.
const envVar = "SIGMA_OCPP_INTEROP"

// needDocker skips the test unless the environment variable is 1 and
// Docker answers, and builds the peer image.
func needDocker(t *testing.T) {
	t.Helper()
	if os.Getenv(envVar) != "1" {
		t.Skip("set " + envVar + "=1 to run the interop tests (needs Docker)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not running")
	}
	// Built every time (Docker reuses its cached layers), so edits to the
	// peer scripts are never tested against a stale image.
	out, err := exec.Command("docker", "build", "-q", "-t", image, "peer").CombinedOutput()
	if err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
}

// freePort returns a TCP port free on the loopback interface.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// peer is a reference program running in a container: its standard
// input takes commands, its "PEER " output lines are collected.
type peer struct {
	t     *testing.T
	name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string

	mu      sync.Mutex
	log     []string
	pending []string // collected lines no expect has taken yet
}

// startPeer runs `docker run --rm -i --name <unique> <args...>`.
func startPeer(t *testing.T, args ...string) *peer {
	t.Helper()
	p := &peer{t: t, name: fmt.Sprintf("sigma-ocpp-sim-%d", time.Now().UnixNano()), lines: make(chan string, 1024)}
	full := append([]string{"run", "--rm", "-i", "--name", p.name}, args...)
	p.cmd = exec.Command("docker", full...)
	var err error
	if p.stdin, err = p.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	r, w := io.Pipe()
	p.cmd.Stdout, p.cmd.Stderr = w, w
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			p.mu.Lock()
			p.log = append(p.log, line)
			p.mu.Unlock()
			if strings.HasPrefix(line, "PEER ") {
				p.lines <- strings.TrimPrefix(line, "PEER ")
			}
		}
	}()
	go func() { _ = p.cmd.Wait(); _ = w.Close() }()
	t.Cleanup(func() {
		_, _ = io.WriteString(p.stdin, "quit\n")
		_ = p.stdin.Close()
		_ = exec.Command("docker", "stop", "-t", "2", p.name).Run()
		if t.Failed() || os.Getenv(envVar+"_LOG") == "1" {
			t.Logf("peer output:\n%s", p.output())
		}
	})
	return p
}

func (p *peer) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.log, "\n")
}

// send writes one command line to the peer.
func (p *peer) send(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.stdin, line+"\n"); err != nil {
		p.t.Fatalf("peer stdin: %v", err)
	}
}

// take returns the first collected "PEER " line, in order, that match
// accepts, and removes it; lines that do not match stay for later calls.
func (p *peer) take(within time.Duration, match func(string) bool) (string, bool) {
	deadline := time.Now().Add(within)
	for {
		for {
			select {
			case l := <-p.lines:
				p.mu.Lock()
				p.pending = append(p.pending, l)
				p.mu.Unlock()
				continue
			default:
			}
			break
		}
		p.mu.Lock()
		for i, l := range p.pending {
			if match(l) {
				p.pending = append(p.pending[:i:i], p.pending[i+1:]...)
				p.mu.Unlock()
				return l, true
			}
		}
		p.mu.Unlock()
		if time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// expect waits for a "PEER " line containing all the parts and returns it.
func (p *peer) expect(within time.Duration, parts ...string) string {
	p.t.Helper()
	l, ok := p.take(within, func(l string) bool {
		for _, part := range parts {
			if !strings.Contains(l, part) {
				return false
			}
		}
		return true
	})
	if !ok {
		p.t.Fatalf("no peer line with %q within %s", parts, within)
	}
	return l
}

// event splits a peer line "<name> <json>" and decodes the JSON.
func event(t *testing.T, line string) map[string]any {
	t.Helper()
	_, js, _ := strings.Cut(line, " ")
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatalf("peer line %q: %v", line, err)
	}
	return m
}

// payloadOf is the "payload" object of a peer event.
func payloadOf(m map[string]any) map[string]any {
	p, _ := m["payload"].(map[string]any)
	return p
}

// call makes the peer send a CALL and returns its result event, or the
// error event when the charge point answered with a CALLERROR.
func (p *peer) call(verb, action string, payload any) (string, map[string]any) {
	p.t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		p.t.Fatal(err)
	}
	p.send(verb + " " + action + " " + string(b))
	l, ok := p.take(20*time.Second, func(l string) bool {
		kind, _, _ := strings.Cut(l, " ")
		return (kind == "result" || kind == "error") && strings.Contains(l, `"action": "`+action+`"`)
	})
	if !ok {
		p.t.Fatalf("no answer to %s within 20s", action)
	}
	kind, _, _ := strings.Cut(l, " ")
	return kind, event(p.t, l)
}
