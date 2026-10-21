// lcl publishes temporary localhost HTTP services through the VPS's Traefik.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type request struct {
	Name    string        `json:"name,omitempty"`
	Timeout time.Duration `json:"timeout_ns,omitempty"`
	Port    int           `json:"port"`
	HTTPS   bool          `json:"https"`
	Tunnel  bool          `json:"tunnel,omitempty"`
	Attach  string        `json:"attach,omitempty"`
	dial    func(context.Context) (net.Conn, error)
}
type reply struct {
	URL     string `json:"url,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
	Dial    string `json:"dial,omitempty"`
}
type session struct {
	host      string
	uid       uint32
	https     bool
	named     bool
	deadline  time.Time
	proxy     *httputil.ReverseProxy
	transport *http.Transport
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	conns     map[*sessionConn]bool
}
type sessionConn struct {
	net.Conn
	s *session
}

func (c *sessionConn) Close() error {
	c.s.mu.Lock()
	delete(c.s.conns, c)
	c.s.mu.Unlock()
	return c.Conn.Close()
}
func (s *session) close() {
	s.cancel()
	s.transport.CloseIdleConnections()
	s.mu.Lock()
	cs := make([]*sessionConn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
}
func newSession(host string, uid uint32, r request) *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{host: host, uid: uid, https: r.HTTPS, named: r.Name != "", ctx: ctx, cancel: cancel, conns: map[*sessionConn]bool{}}
	if r.Timeout > 0 {
		s.deadline = time.Now().Add(r.Timeout)
	}
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", r.Port))
	s.transport = &http.Transport{MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 60 * time.Second}
	s.transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var c net.Conn
		var e error
		if r.dial != nil {
			c, e = r.dial(ctx)
		} else {
			c, e = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
		}
		if e != nil {
			return nil, e
		}
		a := &sessionConn{Conn: c, s: s}
		s.mu.Lock()
		if s.ctx.Err() != nil {
			s.mu.Unlock()
			c.Close()
			return nil, context.Canceled
		}
		s.conns[a] = true
		s.mu.Unlock()
		return a, nil
	}
	s.proxy = &httputil.ReverseProxy{Transport: s.transport, FlushInterval: -1, Rewrite: func(p *httputil.ProxyRequest) {
		p.SetURL(target)
		p.SetXForwarded()
		p.Out.Header.Set("X-Forwarded-Host", p.In.Host)
		scheme := "http"
		if s.https {
			scheme = "https"
		}
		p.Out.Header.Set("X-Forwarded-Proto", scheme)
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
		http.Error(w, "lcl: local application unavailable", http.StatusBadGateway)
	}}
	return s
}

type broker struct {
	mu         sync.RWMutex
	sessions   map[string]*session
	config     string
	backend    string
	domain     string
	resolver   string
	httpEntry  string
	httpsEntry string
	pending    map[string]*pendingConn
}

type pendingConn struct {
	uid  uint32
	conn chan net.Conn
}

func (b *broker) tunnelDial(ctx context.Context, uid uint32, send func(reply) error) (net.Conn, error) {
	var key [16]byte
	if _, e := rand.Read(key[:]); e != nil {
		return nil, e
	}
	token := hex.EncodeToString(key[:])
	p := &pendingConn{uid: uid, conn: make(chan net.Conn)}
	b.mu.Lock()
	if b.pending == nil {
		b.pending = map[string]*pendingConn{}
	}
	b.pending[token] = p
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.pending, token); b.mu.Unlock() }()
	if e := send(reply{Dial: token}); e != nil {
		return nil, e
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case c := <-p.conn:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("client relay timed out")
	}
}

func (b *broker) saveLocked() error {
	routers := map[string]any{}
	for host, s := range b.sessions {
		name := "lcl-" + strings.SplitN(host, ".", 2)[0]
		priority := 100
		if s.named {
			// Existing application routes take precedence over chosen names.
			priority = 1
		}
		router := map[string]any{"rule": "Host(`" + host + "`)", "entryPoints": []string{b.httpEntry}, "service": "lcl-backend", "priority": priority}
		if s.https {
			router["entryPoints"] = []string{b.httpsEntry}
			tls := map[string]any{}
			if b.resolver != "" {
				tls["certResolver"] = b.resolver
			}
			router["tls"] = tls
			routers[name+"-redirect"] = map[string]any{"rule": router["rule"], "entryPoints": []string{b.httpEntry}, "service": "lcl-backend", "priority": priority, "middlewares": []string{"lcl-https"}}
		}
		routers[name] = router
	}
	cfg := map[string]any{"http": map[string]any{"routers": routers, "services": map[string]any{"lcl-backend": map[string]any{"loadBalancer": map[string]any{"servers": []any{map[string]any{"url": b.backend}}}}}, "middlewares": map[string]any{"lcl-https": map[string]any{"redirectScheme": map[string]any{"scheme": "https", "permanent": false}}}}}
	// Traefik's configuration decoder rejects an explicitly empty router map.
	if len(routers) == 0 {
		delete(cfg["http"].(map[string]any), "routers")
	}
	data, e := json.MarshalIndent(cfg, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(b.config), ".lcl-*.tmp")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Chmod(0644); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), b.config)
}
func (b *broker) add(uid uint32, r request) (*session, error) {
	if r.Name != "" {
		name, e := normalizeName(r.Name)
		if e != nil {
			return nil, e
		}
		r.Name = name
	}
	if r.Timeout < 0 {
		return nil, errors.New("timeout must not be negative")
	}
	if r.Port < 1024 || r.Port > 65535 {
		return nil, errors.New("port must be between 1024 and 65535")
	}
	// Reject typos before publishing an inaccessible service.
	if !r.Tunnel {
		c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", r.Port), 2*time.Second)
		if e != nil {
			return nil, fmt.Errorf("no service reachable at 127.0.0.1:%d", r.Port)
		}
		c.Close()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for _, s := range b.sessions {
		if s.uid == uid {
			count++
		}
	}
	if count >= 8 || len(b.sessions) >= 256 {
		return nil, errors.New("session limit reached (8 per user)")
	}
	var host string
	if r.Name != "" {
		host = r.Name + "." + b.domain
		if b.sessions[host] != nil {
			return nil, fmt.Errorf("name %q is already in use", r.Name)
		}
	} else {
		for {
			var v [8]byte
			if _, e := rand.Read(v[:]); e != nil {
				return nil, e
			}
			host = hex.EncodeToString(v[:]) + "." + b.domain
			if b.sessions[host] == nil {
				break
			}
		}
	}
	if len(host) > 253 {
		return nil, errors.New("name and configured domain exceed the DNS hostname length limit")
	}
	s := newSession(host, uid, r)
	b.sessions[host] = s
	if e := b.saveLocked(); e != nil {
		delete(b.sessions, host)
		s.close()
		return nil, e
	}
	return s, nil
}
func (b *broker) remove(s *session) {
	b.mu.Lock()
	var e error
	if b.sessions[s.host] == s {
		delete(b.sessions, s.host)
		e = b.saveLocked()
	}
	b.mu.Unlock()
	s.close()
	if e != nil {
		log.Printf("removing route %s: %v", s.host, e)
	}
}
func (b *broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, e := net.SplitHostPort(host); e == nil {
		host = h
	}
	b.mu.RLock()
	s := b.sessions[host]
	b.mu.RUnlock()
	if s == nil || s.ctx.Err() != nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodHead && r.URL.Path == "/.well-known/lcl/ready" && r.Header.Get("X-Lcl-Probe") == host {
		w.Header().Set("X-Lcl-Ready", host)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}
func peerUID(c *net.UnixConn) (uint32, error) {
	raw, e := c.SyscallConn()
	if e != nil {
		return 0, e
	}
	var uid uint32
	var inner error
	e = raw.Control(func(fd uintptr) {
		var u *syscall.Ucred
		u, inner = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if inner == nil {
			uid = u.Uid
		}
	})
	if e != nil {
		return 0, e
	}
	return uid, inner
}
func (b *broker) control(c *net.UnixConn) {
	owned := true
	defer func() {
		if owned {
			c.Close()
		}
	}()
	enc := json.NewEncoder(c)
	var writeMu sync.Mutex
	send := func(r reply) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return enc.Encode(r)
	}
	uid, e := peerUID(c)
	if e != nil {
		return
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var r request
	if e = json.NewDecoder(io.LimitReader(c, 1024)).Decode(&r); e != nil {
		enc.Encode(reply{Error: "invalid request"})
		return
	}
	c.SetReadDeadline(time.Time{})
	if r.Attach != "" {
		b.mu.RLock()
		p := b.pending[r.Attach]
		b.mu.RUnlock()
		if p == nil || p.uid != uid {
			send(reply{Error: "invalid relay token"})
			return
		}
		if send(reply{Message: "attached"}) != nil {
			return
		}
		c.SetDeadline(time.Time{})
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case p.conn <- c:
			owned = false
		case <-timer.C:
		}
		return
	}
	if r.Tunnel {
		r.dial = func(ctx context.Context) (net.Conn, error) { return b.tunnelDial(ctx, uid, send) }
	}
	s, e := b.add(uid, r)
	if e != nil {
		enc.Encode(reply{Error: e.Error()})
		return
	}
	defer b.remove(s)
	scheme := "http"
	if r.HTTPS {
		scheme = "https"
	}
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if e = send(reply{URL: scheme + "://" + s.host}); e != nil {
		return
	}
	disconnected := make(chan struct{})
	go func() { var p [1]byte; c.Read(p[:]); close(disconnected) }()
	var expired <-chan time.Time
	if !s.deadline.IsZero() {
		timer := time.NewTimer(time.Until(s.deadline))
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-disconnected:
	case <-s.ctx.Done():
	case <-expired:
		send(reply{Message: "Session expired after the configured timeout."})
	}

}
func serve() error {
	socket := env("LCL_SOCKET", "/run/lcl/control.sock")
	addr := env("LCL_LISTEN", "127.0.0.1:19473")
	domain := strings.ToLower(strings.TrimSuffix(os.Getenv("LCL_DOMAIN"), "."))
	if !validDomain(domain) {
		return errors.New("LCL_DOMAIN must be a valid domain, such as preview.example.com")
	}
	b := &broker{sessions: map[string]*session{}, config: env("LCL_CONFIG", "/dynamic/lcl.yaml"), backend: env("LCL_BACKEND_URL", "http://"+addr), domain: domain, resolver: os.Getenv("LCL_CERT_RESOLVER"), httpEntry: env("LCL_HTTP_ENTRYPOINT", "http"), httpsEntry: env("LCL_HTTPS_ENTRYPOINT", "https")}
	var e error
	if e = b.saveLocked(); e != nil {
		return e
	}
	listener, e := net.Listen("tcp", addr)
	if e != nil {
		return e
	}
	defer listener.Close()
	_ = os.Remove(socket)
	ctl, e := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if e != nil {
		return e
	}
	defer ctl.Close()
	if e = os.Chmod(socket, 0666); e != nil {
		return e
	}
	// A second socket can be placed on a volume shared with development containers.
	if extra := os.Getenv("LCL_EXTRA_SOCKET"); extra != "" {
		_ = os.Remove(extra)
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: extra, Net: "unix"})
		if err != nil {
			return err
		}
		defer l.Close()
		if err = os.Chmod(extra, 0666); err != nil {
			return err
		}
		go func() {
			for {
				c, err := l.AcceptUnix()
				if err != nil {
					return
				}
				go b.control(c)
			}
		}()
	}
	server := &http.Server{Handler: b, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() {
		if e := server.Serve(listener); e != nil && e != http.ErrServerClosed {
			log.Fatal(e)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); ctl.Close() }()
	log.Printf("lcl ready on %s; sessions have no timeout by default", addr)
	for {
		c, e := ctl.AcceptUnix()
		if e != nil {
			if ctx.Err() != nil {
				break
			}
			return e
		}
		go b.control(c)
	}
	b.mu.Lock()
	ss := b.sessions
	b.sessions = map[string]*session{}
	e = b.saveLocked()
	b.mu.Unlock()
	for _, s := range ss {
		s.close()
	}
	server.Close()
	return e
}
func validDomain(domain string) bool {
	if len(domain) > 253 || !strings.Contains(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

const usage = "usage: lcl <port> [--https] [-t <minutes>] [--name <name>]"

func parseClientArgs(args []string) (request, string, error) {
	r := request{Tunnel: true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		key, value, hasValue := strings.Cut(arg, "=")
		switch key {
		case "-h", "--help":
			return r, "help", nil
		case "--version":
			return r, "version", nil
		case "--https":
			if hasValue {
				return r, "", errors.New(usage)
			}
			r.HTTPS = true
		case "--name":
			if !hasValue {
				i++
				if i >= len(args) {
					return r, "", errors.New("--name requires a name")
				}
				value = args[i]
			}
			name, e := normalizeName(value)
			if e != nil {
				return r, "", e
			}
			r.Name = name
		case "-t", "--timeout":
			if !hasValue {
				i++
				if i >= len(args) {
					return r, "", errors.New("-t/--timeout requires a duration in minutes")
				}
				value = args[i]
			}
			minutes, e := strconv.ParseInt(value, 10, 64)
			if e != nil || minutes <= 0 || minutes > (1<<63-1)/int64(time.Minute) {
				return r, "", errors.New("timeout must be a positive whole number of minutes within the supported range")
			}
			r.Timeout = time.Duration(minutes) * time.Minute
		default:
			n, e := strconv.Atoi(arg)
			if e != nil || r.Port != 0 {
				return r, "", errors.New(usage)
			}
			r.Port = n
		}
	}
	if r.Port < 1024 || r.Port > 65535 {
		return r, "", errors.New("specify a port between 1024 and 65535: lcl 3000 [-t <minutes>]")
	}
	return r, "", nil
}

func client(args []string) error {
	options, action, e := parseClientArgs(args)
	if e != nil {
		return e
	}
	switch action {
	case "help":
		fmt.Println("Usage: lcl <port> [--https] [-t <minutes>] [--name <name>]\n\nPublish a local HTTP application under a random or chosen subdomain.\nSessions stay open until Ctrl+C or client disconnection by default.\n-t, --timeout <minutes>: close after a fixed duration, regardless of traffic.\n--name <name>: choose a subdomain; spaces and Latin accents are normalized.\n--https: enable public HTTPS; the local application still uses HTTP.\nPorts: 1024-65535. Limit: 8 sessions per user.")
		return nil
	case "version":
		fmt.Println("lcl 1.2.0")
		return nil
	}
	port, https := options.Port, options.HTTPS
	probe, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if e != nil {
		return fmt.Errorf("no service reachable at 127.0.0.1:%d", port)
	}
	probe.Close()
	c, e := net.Dial("unix", env("LCL_SOCKET", "/run/lcl/control.sock"))
	if e != nil {
		return errors.New("lcl service unavailable; contact the server administrator")
	}
	defer c.Close()
	if e = json.NewEncoder(c).Encode(options); e != nil {
		return e
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	dec := json.NewDecoder(bufio.NewReader(c))
	var r reply
	if e = dec.Decode(&r); e != nil {
		return e
	}
	c.SetReadDeadline(time.Time{})
	if r.Error != "" {
		return errors.New(r.Error)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if !https {
		fmt.Fprintln(os.Stderr, "Preparing public route...")
		if e = waitForRoute(ctx, r.URL); e != nil {
			return e
		}
	}
	fmt.Printf("\n  %s → http://127.0.0.1:%d\n\n", r.URL, port)
	if options.Timeout > 0 {
		fmt.Printf("  Closes after %s, regardless of traffic. Press Ctrl+C to stop sooner.\n", options.Timeout)
	} else {
		fmt.Println("  Stays open until you stop it. Press Ctrl+C to stop.")
	}
	if https {
		fmt.Println("  The HTTPS certificate may take a moment to be issued.")
	}
	done := make(chan error, 1)
	go func() {
		for {
			var event reply
			if e := dec.Decode(&event); e != nil {
				done <- e
				return
			}
			if event.Dial != "" {
				go relay(ctx, port, event.Dial)
				continue
			}
			if event.Message != "" {
				fmt.Println("\n  " + event.Message)
			}
			done <- nil
			return
		}
	}()
	select {
	case <-ctx.Done():
		c.Close()
		fmt.Println("\n  Closing public access.")
		return nil
	case e := <-done:
		if e != nil && e != io.EOF {
			return e
		}
		if e == io.EOF {
			fmt.Println("\n  Session closed by the service.")
		}
		return nil
	}
}

func waitForRoute(ctx context.Context, rawURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	u, e := url.Parse(rawURL)
	if e != nil {
		return e
	}
	for {
		r, e := http.NewRequestWithContext(ctx, http.MethodHead, rawURL+"/.well-known/lcl/ready", nil)
		if e != nil {
			return e
		}
		r.Header.Set("X-Lcl-Probe", u.Host)
		response, e := client.Do(r)
		if e == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusNoContent && response.Header.Get("X-Lcl-Ready") == u.Host {
				return nil
			}
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("public route is not ready: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// relay connects to localhost in the client's network namespace, so a terminal
// inside a container can publish applications without exposing a new TCP port.
func relay(ctx context.Context, port int, token string) {
	local, e := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if e != nil {
		return
	}
	defer local.Close()
	tunnel, e := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", env("LCL_SOCKET", "/run/lcl/control.sock"))
	if e != nil {
		return
	}
	defer tunnel.Close()
	stop := context.AfterFunc(ctx, func() { local.Close(); tunnel.Close() })
	defer stop()
	tunnel.SetDeadline(time.Now().Add(5 * time.Second))
	if e = json.NewEncoder(tunnel).Encode(request{Attach: token}); e != nil {
		return
	}
	reader := bufio.NewReader(tunnel)
	var ack reply
	line, e := reader.ReadBytes('\n')
	if e != nil {
		return
	}
	if e = json.Unmarshal(line, &ack); e != nil || ack.Error != "" || ack.Message != "attached" {
		return
	}
	tunnel.SetDeadline(time.Time{})
	done := make(chan struct{}, 1)
	go func() { io.Copy(tunnel, local); done <- struct{}{} }()
	io.Copy(local, reader)
	tunnel.Close()
	local.Close()
	<-done
}
func main() {
	var e error
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		e = serve()
	} else {

		e = client(os.Args[1:])
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, "lcl:", e)
		os.Exit(1)
	}
}
