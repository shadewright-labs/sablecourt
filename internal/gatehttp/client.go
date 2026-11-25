// Package gatehttp configures clients that send credentials to a single endpoint.
package gatehttp

import (
	"errors"
	"net/http"
	"time"
)

// NewClient refuses redirects so a response cannot select another credential destination.
// An injected client keeps its transport, cookie jar and timeout.
func NewClient(base *http.Client, timeout time.Duration) *http.Client {
	client := http.Client{Timeout: timeout}
	if base != nil {
		client.Transport = base.Transport
		client.Jar = base.Jar
		client.Timeout = base.Timeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("gate request redirected")
	}
	return &client
}
