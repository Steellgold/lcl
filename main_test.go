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
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRelay(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "relayed "+r.URL.Path) }))
	t.Cleanup(origin.Close)
	preview := startRelayedPreview(t, origin)
	if preview.host != "etat-de-premiere-version.preview.example.test" {
		t.Fatal(preview.host)
	}
	response, err := preview.client.Do(preview.newRequest(t, http.MethodGet, "/through-client", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || err != nil || string(body) != "relayed /through-client" {
		t.Fatalf("%d %q %v", response.StatusCode, body, err)
	}
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
	req := httptest.NewRequest(http.MethodHead, "http://"+s.host+"/.well-known/lcl/ready", nil)
	req.Header.Set("X-Lcl-Probe", s.host)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("X-Lcl-Ready") != s.host {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
}

func testBroker(t *testing.T) *broker {
	t.Helper()
	return &broker{sessions: map[string]*session{}, config: filepath.Join(t.TempDir(), "lcl.yaml"), backend: "http://127.0.0.1:19473", domain: "preview.example.test", resolver: "letsencrypt", httpEntry: "http", httpsEntry: "https"}
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
func TestControlLifetimeAndDisconnect(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer origin.Close()
	cases := []struct {
		name                 string
		timeout              time.Duration
		activity, disconnect bool
	}{
		{name: "unlimited idle"},
		{name: "unlimited active", activity: true},
		{name: "timed idle", timeout: 200 * time.Millisecond},
		{name: "timed active", timeout: 200 * time.Millisecond, activity: true},
		{name: "disconnect", timeout: time.Minute, disconnect: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := testBroker(t)
			path := filepath.Join(t.TempDir(), "s.sock")
			l, e := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if e != nil {
				t.Fatal(e)
			}
			defer l.Close()
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				c, e := l.AcceptUnix()
				if e == nil {
					b.control(c)
				}
			}()
			c, e := net.Dial("unix", path)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			json.NewEncoder(c).Encode(request{Port: backendPort(t, origin), Timeout: tc.timeout})
			dec := json.NewDecoder(c)
			var initial reply
			if e = dec.Decode(&initial); e != nil || initial.URL == "" {
				t.Fatalf("%+v %v", initial, e)
			}
			host := strings.TrimPrefix(initial.URL, "http://")
			b.mu.RLock()
			s := b.sessions[host]
			b.mu.RUnlock()
			if s.deadline.IsZero() != (tc.timeout == 0) {
				t.Fatal("incorrect session deadline")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requests atomic.Int64
			if tc.activity {
				go func() {
					tick := time.NewTicker(10 * time.Millisecond)
					defer tick.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-tick.C:
							rec := httptest.NewRecorder()
							b.ServeHTTP(rec, httptest.NewRequest("GET", initial.URL, nil))
							if rec.Code == 200 {
								requests.Add(1)
							}
						}
					}
				}()
			}
			switch {
			case tc.disconnect:
				c.Close()
			case tc.timeout == 0:
				select {
				case <-finished:
					t.Fatal("unlimited session expired")
				case <-time.After(350 * time.Millisecond):
				}
				c.Close()
			default:
				var end reply
				if e = dec.Decode(&end); e != nil || !strings.Contains(end.Message, "timeout") {
					t.Fatalf("expiration: %+v %v", end, e)
				}
			}
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("control did not stop")
			}
			cancel()
			if tc.activity && requests.Load() == 0 {
				t.Fatal("test sent no activity")
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

func TestClientTimeoutArguments(t *testing.T) {
	cases := []struct {
		args    []string
		minutes int
		https   bool
	}{
		{args: []string{"3000"}},
		{args: []string{"3000", "-t", "10"}, minutes: 10},
		{args: []string{"3000", "--timeout", "5", "--https"}, minutes: 5, https: true},
		{args: []string{"-t", "2", "3000"}, minutes: 2},
		{args: []string{"3000", "--timeout=3"}, minutes: 3},
		{args: []string{"3000", "-t=4"}, minutes: 4},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			r, action, e := parseClientArgs(tc.args)
			if e != nil || action != "" || r.Port != 3000 || r.HTTPS != tc.https || r.Timeout != time.Duration(tc.minutes)*time.Minute {
				t.Fatalf("%+v %q %v", r, action, e)
			}
		})
	}
	for _, args := range [][]string{{"3000", "-t"}, {"3000", "--timeout"}, {"3000", "-t", "0"}, {"3000", "-t", "-1"}, {"3000", "-t", "1.5"}, {"3000", "-t", "hello"}, {"3000", "-t", "153722868"}, {"3000", "-t", "99999999999999999999"}, {"3000", "--https=yes"}} {
		t.Run("invalid "+strings.Join(args, " "), func(t *testing.T) {
			if _, _, e := parseClientArgs(args); e == nil {
				t.Fatal("invalid arguments accepted")
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
	c.Write([]byte("activity"))
	v := make([]byte, 8)
	if _, e = io.ReadFull(reader, v); e != nil || string(v) != "activity" {
		t.Fatalf("%q %v", v, e)
	}
	s.close()
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, e = reader.ReadByte(); e == nil {
		t.Fatal("websocket remained open")
	}
}
func TestLimitsAndValidation(t *testing.T) {
	if _, e := testBroker(t).add(1, request{Port: 3000, Timeout: -time.Second}); e == nil {
		t.Fatal("negative timeout accepted")
	}
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
