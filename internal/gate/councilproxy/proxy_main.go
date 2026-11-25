//go:build ignore

// Build this entry point explicitly for the local Docker image.
package main

import (
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
)

func main() {
	var cfg councilproxy.Config
	var listenName, probeSocket, probeToken string
	flag.StringVar(&cfg.Harness, "harness", "", "council harness")
	flag.StringVar(&cfg.DaemonListen, "daemon-listen", "", "daemon loopback address")
	flag.StringVar(&cfg.LeaseSHA256, "lease-sha256", "", "expected task lease SHA-256 digest")
	flag.StringVar(&cfg.GateSocket, "gate-socket", "", "container unix socket path for the council gate")
	flag.StringVar(&cfg.GateTLSPort, "gate-tls-port", "", "per-launch daemon TLS port for host-gateway mode")
	flag.StringVar(&cfg.GateSPKISHA256, "gate-spki-sha256", "", "expected SHA-256 hex digest of daemon gate TLS SPKI")
	flag.StringVar(&listenName, "listen-name", "", "proxy name on the private network")
	flag.StringVar(&probeSocket, "probe-gate-socket", "", "probe unix socket path and exit")
	flag.StringVar(&probeToken, "probe-token", "", "probe acknowledgement token")
	flag.Parse()

	if probeSocket != "" {
		err := councilproxy.ProbeGateSocketClient(probeSocket, probeToken)
		if errors.Is(err, councilproxy.ErrGateSocketUnsupported) {
			os.Exit(councilproxy.ProbeSocketUnsupportedExitCode)
		}
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	if listenName == "" {
		log.Fatal("council proxy requires its private network name")
	}
	handler, err := councilproxy.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(listenName, councilproxy.Port))
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Handler:           handler,
		ErrorLog:          log.New(io.Discard, "", 0),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	log.Fatal(server.Serve(councilproxy.LimitListener(ln, councilproxy.MaxGateConns)))
}
