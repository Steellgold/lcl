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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// relayedPreview exercises real frontend HTTP, the broker control protocol,
// the Unix relay, and a TCP application rather than bypassing the tunnel.
type relayedPreview struct {
	front  *httptest.Server
	host   string
	client *http.Client
}

func startRelayedPreview(t *testing.T, origin *httptest.Server) *relayedPreview {
	t.Helper()
	b := testBroker(t)
	path := filepath.Join(t.TempDir(), "control.sock")
	t.Setenv("LCL_SOCKET", path)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var handlers sync.WaitGroup
	acceptingDone := make(chan struct{})
	go func() {
		defer close(acceptingDone)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() { defer handlers.Done(); b.control(conn) }()
		}
	}()
	control, err := net.Dial("unix", path)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	control.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err = json.NewEncoder(control).Encode(request{Port: backendPort(t, origin), Tunnel: true, Name: "Etat de première version"}); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(control)
	var initial reply
	if err = decoder.Decode(&initial); err != nil || initial.URL == "" {
		t.Fatalf("starting relay: %+v, %v", initial, err)
	}
	control.SetReadDeadline(time.Time{})
	ctx, cancel := context.WithCancel(context.Background())
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for {
			var event reply
			if decoder.Decode(&event) != nil {
				return
			}
			if event.Dial != "" {
				go relay(ctx, backendPort(t, origin), event.Dial)
			}
		}
	}()
	front := httptest.NewServer(b)
	client := front.Client()
	client.Timeout = 2 * time.Second
	t.Cleanup(func() {
		cancel()
		control.Close()
		listener.Close()
		<-acceptingDone
		client.CloseIdleConnections()
		front.Close()
		<-eventsDone
		handlers.Wait()
	})
	return &relayedPreview{front: front, host: strings.TrimPrefix(initial.URL, "http://"), client: client}
}

func (p *relayedPreview) newRequest(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, p.front.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = p.host
	return req
}

func TestRelayForwardsIdleConnectionClosure(t *testing.T) {
	closed := make(chan struct{}, 8)
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "page "+r.URL.Path)
	}))
	origin.Config.IdleTimeout = 30 * time.Millisecond
	origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	origin.Start()
	t.Cleanup(origin.Close)
	preview := startRelayedPreview(t, origin)
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("/page-%d", i)
		response, err := preview.client.Do(preview.newRequest(t, http.MethodGet, path, nil))
		if err != nil {
			t.Fatalf("navigation %d after backend idle close: %v", i, err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || string(body) != "page "+path {
			t.Fatalf("navigation %d: %q, %v", i, body, err)
		}
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("application did not close its idle connection")
		}
	}
}

func TestRelayForwardsWebSocketClosure(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nbye")
		rw.Flush()
	}))
	t.Cleanup(origin.Close)
	preview := startRelayedPreview(t, origin)
	conn, err := net.Dial("tcp", preview.front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(conn, "GET /socket HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", preview.host)
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade failed: %v, %v", response, err)
	}
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != "bye" {
		t.Fatalf("application's WebSocket close was not forwarded: %q, %v", body, err)
	}
}

func TestRelayStreamsResponsesAndUploads(t *testing.T) {
	continueStream := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(continueStream) }) }
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-continueStream:
				fmt.Fprint(w, "data: second\n\n")
			case <-r.Context().Done():
			}
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(origin.Close)
	preview := startRelayedPreview(t, origin)
	defer release()
	response, err := preview.client.Do(preview.newRequest(t, http.MethodGet, "/events", nil))
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "data: first\n" {
		t.Fatalf("initial event was buffered: %q, %v", first, err)
	}
	release()
	rest, err := io.ReadAll(reader)
	response.Body.Close()
	if err != nil || string(rest) != "\ndata: second\n\n" {
		t.Fatalf("stream response: %q, %v", rest, err)
	}
	payload := strings.Repeat("upload payload\n", 8192)
	response, err = preview.client.Do(preview.newRequest(t, http.MethodPost, "/upload", strings.NewReader(payload)))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != payload {
		t.Fatalf("upload was truncated: %d bytes, %v", len(body), err)
	}
}

func TestRelayReportsUnavailableApplication(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(origin.Close)
	preview := startRelayedPreview(t, origin)
	origin.Close()
	response, err := preview.client.Do(preview.newRequest(t, http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("backend failure was not reported within the request deadline: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("unavailable application returned %d", response.StatusCode)
	}
}

func TestClosingSessionDoesNotReturnSuccessfulResponse(t *testing.T) {
	started := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(origin.Close)
	b := testBroker(t)
	s, err := b.add(1, request{Port: backendPort(t, origin)})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b)
	t.Cleanup(front.Close)
	t.Cleanup(func() { b.remove(s) })
	client := front.Client()
	client.Timeout = 2 * time.Second
	request, err := http.NewRequest(http.MethodGet, front.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = s.host
	done := make(chan int, 1)
	go func() {
		response, err := client.Do(request)
		if err != nil {
			done <- 0
			return
		}
		defer response.Body.Close()
		done <- response.StatusCode
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not reach application")
	}
	b.remove(s)
	select {
	case status := <-done:
		if status != 0 && status < 400 {
			t.Fatalf("session closure returned success: %d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("active request remained blocked after session closure")
	}
}

func TestRelayConcurrentNavigationAndCancellation(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(started)
			<-r.Context().Done()
			close(canceled)
			return
		}
		fmt.Fprint(w, "page "+r.URL.Path)
	}))
	t.Cleanup(origin.Close)
	preview := startRelayedPreview(t, origin)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := preview.newRequest(t, http.MethodGet, "/slow", nil).WithContext(ctx)
	requestDone := make(chan error, 1)
	go func() {
		response, err := preview.client.Do(slow)
		if response != nil {
			response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("application did not receive slow request")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("canceling navigation did not release the application connection")
	}
	if err := <-requestDone; err == nil {
		t.Fatal("canceled request unexpectedly completed")
	}
	var workers sync.WaitGroup
	failures := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for navigation := 0; navigation < 5; navigation++ {
				path := fmt.Sprintf("/page-%d-%d", worker, navigation)
				response, err := preview.client.Do(preview.newRequest(t, http.MethodGet, path, nil))
				if err != nil {
					failures <- err
					return
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != 200 || string(body) != "page "+path {
					failures <- fmt.Errorf("%s: status %d, body %q, error %v", path, response.StatusCode, body, err)
					return
				}
			}
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

func TestRelayPreservesResponseAfterRequestEOF(t *testing.T) {
	application, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { application.Close() })
	_, rawPort, _ := net.SplitHostPort(application.Addr().String())
	var port int
	fmt.Sscan(rawPort, &port)
	responseBody := strings.Repeat("response after request EOF\n", 4096)
	applicationDone := make(chan error, 1)
	go func() {
		conn, err := application.Accept()
		if err != nil {
			applicationDone <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		body, err := io.ReadAll(conn)
		if err == nil && string(body) != "complete request" {
			err = fmt.Errorf("unexpected request: %q", body)
		}
		if err == nil {
			_, err = io.WriteString(conn, responseBody)
		}
		applicationDone <- err
	}()
	path := filepath.Join(t.TempDir(), "half-close.sock")
	t.Setenv("LCL_SOCKET", path)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relayDone := make(chan struct{})
	go func() { defer close(relayDone); relay(ctx, port, "test-token") }()
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	var attachment request
	if err = json.NewDecoder(conn).Decode(&attachment); err != nil || attachment.Attach != "test-token" {
		t.Fatalf("relay handshake: %+v, %v", attachment, err)
	}
	// Coalesce the acknowledgement and first payload to check buffered bytes too.
	io.WriteString(conn, "{\"message\":\"attached\"}\ncomplete request")
	conn.CloseWrite()
	body, err := io.ReadAll(conn)
	if err != nil || string(body) != responseBody {
		t.Fatalf("response after half-close: %d bytes, %v", len(body), err)
	}
	if err = <-applicationDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-relayDone:
	case <-time.After(time.Second):
		t.Fatal("relay did not finish after both directions closed")
	}
}
