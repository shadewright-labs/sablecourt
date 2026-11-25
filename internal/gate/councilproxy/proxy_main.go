//go:build ignore

// Build this entry point explicitly for the local Docker image.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
)

func main() {
	var cfg councilproxy.Config
	var listenName string
	flag.StringVar(&cfg.Harness, "harness", "", "council harness")
	flag.StringVar(&cfg.DaemonListen, "daemon-listen", "", "daemon loopback address")
	flag.StringVar(&cfg.GatePort, "gate-port", "", "optional host-gateway port for the council gate")
	flag.StringVar(&cfg.LeaseSHA256, "lease-sha256", "", "expected task lease SHA-256 digest")
	flag.StringVar(&listenName, "listen-name", "", "proxy name on the private network")
	flag.Parse()
	if listenName == "" {
		log.Fatal("council proxy requires its private network name")
	}
	handler, err := councilproxy.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Addr:              net.JoinHostPort(listenName, councilproxy.Port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	log.Fatal(server.ListenAndServe())
}
