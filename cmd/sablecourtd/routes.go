package main

import "net/http"

// recordingMux registers routes on a ServeMux and, when record is set, reports
// each pattern. Routing is the ServeMux's alone.
type recordingMux struct {
	mux    *http.ServeMux
	record func(pattern string)
}

func (m recordingMux) Handle(pattern string, handler http.Handler) {
	m.mux.Handle(pattern, handler)
	if m.record != nil {
		m.record(pattern)
	}
}

func (m recordingMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.mux.HandleFunc(pattern, handler)
	if m.record != nil {
		m.record(pattern)
	}
}
