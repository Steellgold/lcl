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
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const accessLogBuffer = 128

type accessLog struct {
	Method   string        `json:"method"`
	Status   int           `json:"status"`
	Path     string        `json:"path"`
	Duration time.Duration `json:"duration_ns"`
	Dropped  uint64        `json:"dropped,omitempty"`
}

type accessLogStream struct {
	token    string
	attached bool // Protected by broker.mu; each token can be used once.
	records  chan accessLog
	dropped  atomic.Uint64
}

func newAccessLogStream() (*accessLogStream, error) {
	var key [16]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	return &accessLogStream{token: hex.EncodeToString(key[:]), records: make(chan accessLog, accessLogBuffer)}, nil
}

func (s *accessLogStream) record(entry accessLog) {
	select {
	case s.records <- entry:
	default:
		s.dropped.Add(1)
	}
}

// Record final response headers, so long-lived streams appear immediately.
// Preserve ResponseController operations without buffering any response data.
type accessLogWriter struct {
	http.ResponseWriter
	stream  *accessLogStream
	method  string
	path    string
	started time.Time
	once    sync.Once
}

func (w *accessLogWriter) record(status int) {
	if status < 200 && status != http.StatusSwitchingProtocols {
		return
	}
	w.once.Do(func() {
		w.stream.record(accessLog{Method: logField(w.method), Status: status, Path: logField(w.path), Duration: time.Since(w.started)})
	})
}

func (w *accessLogWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *accessLogWriter) WriteHeader(status int) {
	w.ResponseWriter.WriteHeader(status)
	w.record(status)
}

func (w *accessLogWriter) Write(data []byte) (int, error) {
	w.record(http.StatusOK)
	return w.ResponseWriter.Write(data)
}

func (w *accessLogWriter) FlushError() error {
	w.record(http.StatusOK)
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *accessLogWriter) Flush() { _ = w.FlushError() }

func (w *accessLogWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.record(http.StatusSwitchingProtocols)
	}
	return conn, rw, err
}

// Limit queue memory and prevent paths or methods from injecting terminal escapes.
func logField(value string) string {
	if len(value) > 2048 {
		value = value[:2048] + "..."
	}
	quoted := strconv.QuoteToASCII(value)
	return quoted[1 : len(quoted)-1]
}

// Logs use their own authenticated socket. A stalled log reader must never hold
// the control socket's write lock or delay the relay's connection requests.
func (b *broker) watchLogs(conn *net.UnixConn, uid uint32, token string) {
	var owner *session
	b.mu.Lock()
	for _, s := range b.sessions {
		if s.uid == uid && s.logs != nil && s.logs.token == token && !s.logs.attached {
			owner = s
			s.logs.attached = true
			break
		}
	}
	b.mu.Unlock()
	encoder := json.NewEncoder(conn)
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if owner == nil {
		encoder.Encode(reply{Error: "invalid log token"})
		return
	}
	if encoder.Encode(reply{Message: "watching"}) != nil {
		return
	}
	// A paused terminal may resume later. Session cancellation or disconnection
	// closes this socket; blocked log writes need no independent timeout.
	conn.SetWriteDeadline(time.Time{})
	stop := context.AfterFunc(owner.ctx, func() { conn.Close() })
	defer stop()
	disconnected := make(chan struct{})
	go func() { var data [1]byte; conn.Read(data[:]); close(disconnected) }()
	for {
		select {
		case <-owner.ctx.Done():
			return
		case <-disconnected:
			return
		case entry := <-owner.logs.records:
			entry.Dropped = owner.logs.dropped.Swap(0)
			if encoder.Encode(entry) != nil {
				return
			}
		}
	}
}

type accessLogConnection struct {
	net.Conn
	decoder *json.Decoder
}

func connectAccessLogs(ctx context.Context, token string) (*accessLogConnection, error) {
	// Older brokers do not advertise logs; the preview still works normally.
	if token == "" {
		return nil, nil
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", env("LCL_SOCKET", "/run/lcl/control.sock"))
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err = json.NewEncoder(conn).Encode(request{Watch: token}); err != nil {
		conn.Close()
		return nil, err
	}
	decoder := json.NewDecoder(conn)
	var ack reply
	if err = decoder.Decode(&ack); err != nil || ack.Message != "watching" {
		conn.Close()
		if err == nil {
			err = errors.New("log subscription refused")
		}
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return &accessLogConnection{Conn: conn, decoder: decoder}, nil
}

func printAccessLogs(ctx context.Context, conn *accessLogConnection, output io.Writer, color bool) {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer conn.Close()
	for {
		var entry accessLog
		if conn.decoder.Decode(&entry) != nil {
			return
		}
		if entry.Dropped > 0 {
			if _, err := fmt.Fprintf(output, "  Skipped %d request logs: output could not keep up.\n", entry.Dropped); err != nil {
				return
			}
		}
		if _, err := fmt.Fprintln(output, formatAccessLog(entry, color)); err != nil {
			return
		}
	}
}

func formatAccessLog(entry accessLog, color bool) string {
	status := strconv.Itoa(entry.Status)
	if color {
		shade := "36" // Informational responses.
		switch {
		case entry.Status >= 500:
			shade = "31"
		case entry.Status >= 400:
			shade = "33"
		case entry.Status >= 300:
			shade = "34"
		case entry.Status >= 200:
			shade = "32"
		}
		status = "\x1b[" + shade + "m" + status + "\x1b[0m"
	}
	duration := entry.Duration.Round(time.Millisecond)
	if duration < time.Millisecond {
		duration = entry.Duration.Round(time.Microsecond)
	}
	return fmt.Sprintf("  %-7s %s  %s  %s", entry.Method, status, entry.Path, duration)
}

func terminalColors(file *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	var terminal syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&terminal)))
	return err == 0
}
