package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func nextAccessLog(t *testing.T, conn *accessLogConnection) accessLog {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var entry accessLog
	if err := conn.decoder.Decode(&entry); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Time{})
	return entry
}

func TestAccessLogsReportRequestsAndProxyErrors(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.Copy(w, r.Body)
	}))
	t.Cleanup(origin.Close)
	preview := startRelayedPreview(t, origin)
	response, err := preview.client.Do(preview.newRequest(t, http.MethodPost, "/api/items?token=secret", strings.NewReader("private body")))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	entry := nextAccessLog(t, preview.logs)
	if entry.Method != "POST" || entry.Status != http.StatusCreated || entry.Path != "/api/items" || entry.Duration <= 0 {
		t.Fatalf("unexpected access log: %+v", entry)
	}
	origin.Close()
	response, err = preview.client.Do(preview.newRequest(t, http.MethodGet, "/unavailable", nil))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	entry = nextAccessLog(t, preview.logs)
	if entry.Status != http.StatusBadGateway || entry.Path != "/unavailable" {
		t.Fatalf("proxy failure not logged: %+v", entry)
	}
}

func TestAccessLogsDoNotIncludeReadinessChecks(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(origin.Close)
	preview := startRelayedPreview(t, origin)
	probe := preview.newRequest(t, http.MethodHead, "/.well-known/lcl/ready", nil)
	probe.Header.Set("X-Lcl-Probe", preview.host)
	response, err := preview.client.Do(probe)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = preview.client.Do(preview.newRequest(t, http.MethodGet, "/missing", nil))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	entry := nextAccessLog(t, preview.logs)
	if entry.Path != "/missing" || entry.Status != http.StatusNotFound {
		t.Fatalf("readiness probe was logged: %+v", entry)
	}
}

func TestAccessLogsAppearBeforeStreamClosure(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ready\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(origin.Close)
	preview := startRelayedPreview(t, origin)
	response, err := preview.client.Do(preview.newRequest(t, http.MethodGet, "/events", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if line, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil || line != "data: ready\n" {
		t.Fatalf("stream did not flush: %q, %v", line, err)
	}
	entry := nextAccessLog(t, preview.logs)
	if entry.Status != http.StatusOK || entry.Path != "/events" {
		t.Fatalf("stream response not logged: %+v", entry)
	}
}

func TestSlowAccessLogReaderDoesNotBlockRelays(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Force another relay request after every response, including once the
		// unread log socket and bounded queue fill up.
		w.Header().Set("Connection", "close")
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(origin.Close)
	preview := startRelayedPreview(t, origin)
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Go(func() {
			for range 80 {
				request := preview.newRequest(t, http.MethodGet, "/"+strings.Repeat("x", 2048), nil)
				response, err := preview.client.Do(request)
				if err != nil {
					t.Errorf("unread logs blocked a new relay: %v", err)
					return
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != http.StatusOK || string(body) != "ok" {
					t.Errorf("response while logs stalled: %d, %q, %v", response.StatusCode, body, err)
					return
				}
			}
		})
	}
	workers.Wait()
	// Resuming output reports the dropped records instead of allocating an
	// unbounded backlog or silently stalling HTTP forwarding.
	for range 640 {
		if nextAccessLog(t, preview.logs).Dropped > 0 {
			return
		}
	}
	t.Fatal("log backlog did not report dropped records")
}

func TestAccessLogSubscriptionsRequireOwnerAndOneTimeToken(t *testing.T) {
	b := testBroker(t)
	stream, err := newAccessLogStream()
	if err != nil {
		t.Fatal(err)
	}
	s := newSession("owner.preview.example.test", 42, request{Port: 3000, logs: stream})
	b.sessions[s.host] = s
	path := filepath.Join(t.TempDir(), "logs.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var handlers sync.WaitGroup
	defer handlers.Wait()
	defer s.close()
	for _, tc := range []struct {
		uid   uint32
		token string
		valid bool
	}{
		{43, stream.token, false},
		{42, "invalid", false},
		{42, stream.token, true},
		{42, stream.token, false},
	} {
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		server, err := listener.AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		handlers.Go(func() { defer server.Close(); b.watchLogs(server, tc.uid, tc.token) })
		conn.SetReadDeadline(time.Now().Add(time.Second))
		var ack reply
		err = json.NewDecoder(conn).Decode(&ack)
		conn.Close()
		if err != nil || (ack.Message == "watching") != tc.valid || (!tc.valid && ack.Error == "") {
			t.Fatalf("subscription uid=%d valid=%v: %+v, %v", tc.uid, tc.valid, ack, err)
		}
	}
}

func TestAccessLogFormatting(t *testing.T) {
	for _, tc := range []struct {
		status int
		color  string
	}{{101, "36"}, {200, "32"}, {302, "34"}, {404, "33"}, {502, "31"}} {
		entry := accessLog{Method: "GET", Status: tc.status, Path: "/api/health", Duration: 12 * time.Millisecond}
		plain := formatAccessLog(entry, false)
		if strings.Contains(plain, "\x1b") || !strings.Contains(plain, fmt.Sprint(tc.status)) || !strings.HasSuffix(plain, "/api/health  12ms") {
			t.Fatalf("plain output: %q", plain)
		}
		colored := formatAccessLog(entry, true)
		if !strings.Contains(colored, "\x1b["+tc.color+"m") || !strings.Contains(colored, "\x1b[0m") {
			t.Fatalf("colored output: %q", colored)
		}
	}
	if field := logField("/bad\x1b[31m\r\n"); strings.ContainsAny(field, "\x1b\r\n") {
		t.Fatalf("unsafe terminal field: %q", field)
	}
	file, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if terminalColors(file) {
		t.Fatal("redirected output must not contain colors")
	}
	t.Setenv("NO_COLOR", "1")
	if terminalColors(os.Stdout) {
		t.Fatal("NO_COLOR was ignored")
	}
	if conn, err := connectAccessLogs(context.Background(), ""); conn != nil || err != nil {
		t.Fatalf("older broker compatibility: %v, %v", conn, err)
	}
}
