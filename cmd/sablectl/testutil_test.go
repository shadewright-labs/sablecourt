package main

import (
	"io"
	"net/http"
	"sync"
	"time"
)

type handlerRoundTripper struct {
	handler http.Handler
}

func newTestClientFactory(handler http.Handler) func(time.Duration) (*http.Client, string) {
	rt := &handlerRoundTripper{handler: handler}
	return func(timeout time.Duration) (*http.Client, string) {
		return &http.Client{Timeout: timeout, Transport: rt}, "http://unix"
	}
}

func (rt *handlerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	rw := &pipeResponseWriter{
		header: make(http.Header),
		pw:     pw,
		ready:  make(chan struct{}),
	}
	r := req.Clone(req.Context())
	if r.Body == nil {
		r.Body = http.NoBody
	}
	go func() {
		defer rw.finish()
		rt.handler.ServeHTTP(rw, r)
	}()
	select {
	case <-rw.ready:
	case <-req.Context().Done():
		_ = pr.Close()
		return nil, req.Context().Err()
	}
	rw.mu.Lock()
	code := rw.code
	hdr := rw.clonedHeader
	rw.mu.Unlock()
	return &http.Response{
		StatusCode: code,
		Status:     http.StatusText(code),
		Header:     hdr,
		Body:       pr,
		Request:    req,
	}, nil
}

type pipeResponseWriter struct {
	mu           sync.Mutex
	header       http.Header
	clonedHeader http.Header
	code         int
	wrote        bool
	pw           *io.PipeWriter
	ready        chan struct{}
	once         sync.Once
}

func (w *pipeResponseWriter) Header() http.Header {
	return w.header
}

func (w *pipeResponseWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	if !w.wrote {
		w.code = statusCode
		w.clonedHeader = w.header.Clone()
		w.wrote = true
	}
	w.mu.Unlock()
	w.once.Do(func() { close(w.ready) })
}

func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(p)
}

func (w *pipeResponseWriter) Flush() {
	w.WriteHeader(http.StatusOK)
}

func (w *pipeResponseWriter) finish() {
	w.WriteHeader(http.StatusOK)
	_ = w.pw.Close()
}
