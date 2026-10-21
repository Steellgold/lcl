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
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClientRelay(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "relayed "+r.URL.Path) }))
	defer origin.Close()
	b := testBroker(t)
	path := filepath.Join(t.TempDir(), "relay.sock")
	t.Setenv("LCL_SOCKET", path)
	listener, e := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	go func() {
		for {
			c, e := listener.AcceptUnix()
			if e != nil {
				return
			}
			go b.control(c)
		}
	}()
	control, e := net.Dial("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	defer control.Close()
	json.NewEncoder(control).Encode(request{Port: backendPort(t, origin), Tunnel: true})
	dec := json.NewDecoder(control)
	var initial reply
	if e = dec.Decode(&initial); e != nil || initial.URL == "" {
		t.Fatalf("%+v %v", initial, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for {
			var r reply
			if dec.Decode(&r) != nil {
				return
			}
			if r.Dial != "" {
				go relay(ctx, backendPort(t, origin), r.Dial)
			}
		}
	}()
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, httptest.NewRequest("GET", initial.URL+"/through-client", nil))
	if rec.Code != 200 || rec.Body.String() != "relayed /through-client" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	control.Close()
	cancel()
	<-eventsDone
}

func TestDomainValidation(t *testing.T) {
	for _, d := range []string{"", "localhost", "a..com", "-a.example", "example.com/path", "a`b.example"} {
		if validDomain(d) {
			t.Errorf("accepted %q", d)
		}
	}
	if !validDomain("preview.example.com") {
		t.Fatal("rejected valid domain")
	}
}

func TestReadinessProbeDoesNotReachApplication(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("probe reached application") }))
	defer origin.Close()
	b := testBroker(t)
	s, e := b.add(1, request{Port: backendPort(t, origin)})
	if e != nil {
		t.Fatal(e)
	}
	defer b.remove(s)
	before := s.last.Load()
	req := httptest.NewRequest(http.MethodHead, "http://"+s.host+"/.well-known/lcl/ready", nil)
	req.Header.Set("X-Lcl-Probe", s.host)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("X-Lcl-Ready") != s.host {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if before != s.last.Load() {
		t.Fatal("readiness probe renewed session")
	}
}

func testBroker(t *testing.T) *broker {
	t.Helper()
	return &broker{sessions: map[string]*session{}, config: filepath.Join(t.TempDir(), "lcl.yaml"), backend: "http://127.0.0.1:19473", ttl: 750 * time.Millisecond, domain: "preview.example.test", resolver: "letsencrypt", httpEntry: "http", httpsEntry: "https"}
}
func backendPort(t *testing.T, s *httptest.Server) int {
	t.Helper()
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	v, _ := strconv.Atoi(p)
	return v
}
func TestProxyAndRemoval(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s|%s", r.URL.String(), r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Forwarded-Proto"))
	}))
	defer origin.Close()
	b := testBroker(t)
	s, e := b.add(12, request{Port: backendPort(t, origin), HTTPS: true})
	if e != nil {
		t.Fatal(e)
	}
	req := httptest.NewRequest("GET", "https://"+s.host+"/hello?q=world", nil)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Body.String() != "/hello?q=world|"+s.host+"|https" {
		t.Fatal(rec.Body.String())
	}
	data, _ := os.ReadFile(b.config)
	if !strings.Contains(string(data), "letsencrypt") || !strings.Contains(string(data), s.host) {
		t.Fatal(string(data))
	}
	b.remove(s)
	rec = httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	data, _ = os.ReadFile(b.config)
	if strings.Contains(string(data), s.host) {
		t.Fatal("route retained")
	}
}
func TestControlActivityExpirationAndDisconnect(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer origin.Close()
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprint(disconnect), func(t *testing.T) {
			b := testBroker(t)
			path := filepath.Join(t.TempDir(), "s.sock")
			l, e := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if e != nil {
				t.Fatal(e)
			}
			defer l.Close()
			finished := make(chan struct{})
			go func() {
				c, e := l.AcceptUnix()
				if e == nil {
					b.control(c)
				}
				close(finished)
			}()
			c, e := net.Dial("unix", path)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			json.NewEncoder(c).Encode(request{Port: backendPort(t, origin)})
			dec := json.NewDecoder(c)
			var r reply
			if e = dec.Decode(&r); e != nil || r.URL == "" {
				t.Fatalf("%+v %v", r, e)
			}
			host := strings.TrimPrefix(r.URL, "http://")
			if disconnect {
				c.Close()
			} else {
				for i := 0; i < 4; i++ {
					time.Sleep(300 * time.Millisecond)
					rec := httptest.NewRecorder()
					b.ServeHTTP(rec, httptest.NewRequest("GET", r.URL, nil))
					if rec.Code != 200 {
						t.Fatal("expired while active")
					}
				}
				b.mu.RLock()
				n := len(b.sessions)
				b.mu.RUnlock()
				if n != 1 {
					t.Fatal("session missing")
				}
				if e = dec.Decode(&r); e != nil || r.Message == "" {
					t.Fatalf("expiration: %+v %v", r, e)
				}
			}
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("control did not stop")
			}
			b.mu.RLock()
			n := len(b.sessions)
			b.mu.RUnlock()
			if n != 0 {
				t.Fatal("session not removed")
			}
			data, _ := os.ReadFile(b.config)
			if strings.Contains(string(data), host) {
				t.Fatal("route not removed")
			}
		})
	}
}
func TestWebSocketUpgradeAndTraffic(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, e := w.(http.Hijacker).Hijack()
		if e != nil {
			return
		}
		defer c.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		rw.Flush()
		io.Copy(c, c)
	}))
	defer origin.Close()
	b := testBroker(t)
	s, e := b.add(1, request{Port: backendPort(t, origin)})
	if e != nil {
		t.Fatal(e)
	}
	defer b.remove(s)
	front := httptest.NewServer(b)
	defer front.Close()
	c, e := net.Dial("tcp", front.Listener.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", s.host)
	reader := bufio.NewReader(c)
	resp, e := http.ReadResponse(reader, nil)
	if e != nil {
		t.Fatal(e)
	}
	if resp.StatusCode != 101 {
		t.Fatal(resp.StatusCode)
	}
	before := s.last.Load()
	time.Sleep(10 * time.Millisecond)
	c.Write([]byte("activity"))
	v := make([]byte, 8)
	if _, e = io.ReadFull(reader, v); e != nil || string(v) != "activity" {
		t.Fatalf("%q %v", v, e)
	}
	if s.last.Load() <= before {
		t.Fatal("websocket activity not recorded")
	}
	s.close()
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, e = reader.ReadByte(); e == nil {
		t.Fatal("websocket remained open")
	}
}
func TestLimitsAndValidation(t *testing.T) {
	b := testBroker(t)
	if _, e := b.add(1, request{Port: 80}); e == nil {
		t.Fatal("privileged port accepted")
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer origin.Close()
	for i := 0; i < 8; i++ {
		s, e := b.add(42, request{Port: backendPort(t, origin)})
		if e != nil {
			t.Fatal(e)
		}
		defer b.remove(s)
	}
	if _, e := b.add(42, request{Port: backendPort(t, origin)}); e == nil {
		t.Fatal("quota exceeded")
	}
	s, e := b.add(43, request{Port: backendPort(t, origin)})
	if e != nil {
		t.Fatal("another user rejected", e)
	}
	b.remove(s)
}
