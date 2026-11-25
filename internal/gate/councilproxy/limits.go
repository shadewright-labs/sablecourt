package councilproxy

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	MaxGateBodyBytes      int64 = 2 << 20
	MaxGateConns                = 32
	GateReadHeaderTimeout       = 5 * time.Second
	GateReadTimeout             = 30 * time.Second
	GateWriteTimeout            = 30 * time.Second
	GateIdleTimeout             = 30 * time.Second
	GateMaxHeaderBytes          = 32 << 10
)

type GateLimits struct {
	MaxConns          int
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
	MaxBodyBytes      int64
}

func DefaultGateLimits() GateLimits {
	return GateLimits{
		MaxConns:          MaxGateConns,
		ReadHeaderTimeout: GateReadHeaderTimeout,
		ReadTimeout:       GateReadTimeout,
		WriteTimeout:      GateWriteTimeout,
		IdleTimeout:       GateIdleTimeout,
		MaxHeaderBytes:    GateMaxHeaderBytes,
		MaxBodyBytes:      MaxGateBodyBytes,
	}
}

func (l GateLimits) normalized() GateLimits {
	def := DefaultGateLimits()
	if l.MaxConns <= 0 {
		l.MaxConns = def.MaxConns
	}
	if l.ReadHeaderTimeout <= 0 {
		l.ReadHeaderTimeout = def.ReadHeaderTimeout
	}
	if l.ReadTimeout <= 0 {
		l.ReadTimeout = def.ReadTimeout
	}
	if l.WriteTimeout <= 0 {
		l.WriteTimeout = def.WriteTimeout
	}
	if l.IdleTimeout <= 0 {
		l.IdleTimeout = def.IdleTimeout
	}
	if l.MaxHeaderBytes <= 0 {
		l.MaxHeaderBytes = def.MaxHeaderBytes
	}
	if l.MaxBodyBytes <= 0 {
		l.MaxBodyBytes = def.MaxBodyBytes
	}
	return l
}

type limitListener struct {
	net.Listener
	sem  chan struct{}
	done chan struct{}
	once sync.Once
}

type limitListenerConn struct {
	net.Conn
	release func()
}

func (c *limitListenerConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}

// LimitListener caps simultaneous accepted connections, including peers stalling before completing HTTP headers.
func LimitListener(ln net.Listener, maxConns int) net.Listener {
	if maxConns <= 0 {
		maxConns = MaxGateConns
	}
	return &limitListener{
		Listener: ln,
		sem:      make(chan struct{}, maxConns),
		done:     make(chan struct{}),
	}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitListenerConn{
		Conn:    c,
		release: sync.OnceFunc(func() { <-l.sem }),
	}, nil
}

func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// WrapGateHandler enforces concurrent request admission, per-request read/write deadlines, and bounded request bodies.
func WrapGateHandler(next http.Handler, limits GateLimits) http.Handler {
	limits = limits.normalized()
	sem := make(chan struct{}, limits.MaxConns)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			w.Header().Set("Connection", "close")
			http.Error(w, "council gate busy", http.StatusServiceUnavailable)
			return
		}
		rc := http.NewResponseController(w)
		now := time.Now()
		if err := rc.SetReadDeadline(now.Add(limits.ReadTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			w.Header().Set("Connection", "close")
			http.Error(w, "council gate read deadline failed", http.StatusInternalServerError)
			return
		}
		if err := rc.SetWriteDeadline(now.Add(limits.WriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			w.Header().Set("Connection", "close")
			http.Error(w, "council gate write deadline failed", http.StatusInternalServerError)
			return
		}
		if r.ContentLength > limits.MaxBodyBytes {
			w.Header().Set("Connection", "close")
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
			limited := http.MaxBytesReader(w, r.Body, limits.MaxBodyBytes)
			body, err := io.ReadAll(limited)
			_ = limited.Close()
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				w.Header().Set("Connection", "close")
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			if err != nil {
				w.Header().Set("Connection", "close")
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		next.ServeHTTP(w, r)
	})
}

func NewGateServer(handler http.Handler, limits GateLimits) *http.Server {
	limits = limits.normalized()
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: limits.ReadHeaderTimeout,
		ReadTimeout:       limits.ReadTimeout,
		WriteTimeout:      limits.WriteTimeout,
		IdleTimeout:       limits.IdleTimeout,
		MaxHeaderBytes:    limits.MaxHeaderBytes,
	}
}
