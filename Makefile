BIN_DIR ?= $(HOME)/.local/bin
PLIST_PATH ?= $(HOME)/Library/LaunchAgents/io.shadewright.sablecourtd.plist
LOG_PATH ?= $(HOME)/Library/Logs/sablecourtd.log
MANIFESTS ?= $(PWD)/examples/local-demo.yaml

.PHONY: test vet build install demo agent-image service-install service-uninstall docs docs-check

test:
	go test -race ./...

vet:
	go vet ./...

# Reference pages under docs/reference are generated from the code or checked
# against it by tests, which `go test ./...` (and so CI) also runs.
DOCS_TESTS = TestCLIReferenceIsCurrent|TestHTTPReferenceListsEveryRoute|TestManifestReferenceMatchesTypesAndLoader|TestLimitsReferenceMatchesConstants|TestMetricsReferenceListsEveryMetric
DOCS_PKGS = ./cmd/sablectl ./cmd/sablecourtd ./internal/config ./internal/refdocs ./internal/telemetry

# docs regenerates the generated blocks of docs/reference/cli.md.
docs:
	SABLECOURT_UPDATE_DOCS=1 go test -count=1 -run 'TestCLIReferenceIsCurrent' ./cmd/sablectl ./cmd/sablecourtd

# docs-check fails when any reference page is stale.
docs-check:
	go test -count=1 -run '$(DOCS_TESTS)' $(DOCS_PKGS)

build:
	mkdir -p bin
	go build -o bin/sablecourtd ./cmd/sablecourtd
	go build -o bin/sablectl ./cmd/sablectl

install: build
	mkdir -p $(BIN_DIR)
	cp bin/sablecourtd $(BIN_DIR)/sablecourtd
	cp bin/sablectl $(BIN_DIR)/sablectl

agent-image:
	docker build -t sablecourt-agent:0.1.0 -f cmd/sablecourt-agent/Dockerfile .

demo:
	go run ./cmd/sablecourtd --manifests=$(MANIFESTS)

service-install: install
	mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/Library/Logs
	@printf '%s\n' \
	'<?xml version="1.0" encoding="UTF-8"?>' \
	'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
	'<plist version="1.0"><dict>' \
	'  <key>Label</key><string>io.shadewright.sablecourtd</string>' \
	'  <key>ProgramArguments</key><array>' \
	'    <string>$(BIN_DIR)/sablecourtd</string>' \
	'    <string>--manifests=$(MANIFESTS)</string>' \
	'  </array>' \
	'  <key>RunAtLoad</key><true/>' \
	'  <key>KeepAlive</key><true/>' \
	'  <key>StandardOutPath</key><string>$(LOG_PATH)</string>' \
	'  <key>StandardErrorPath</key><string>$(LOG_PATH)</string>' \
	'</dict></plist>' > $(PLIST_PATH)
	launchctl unload $(PLIST_PATH) 2>/dev/null || true
	launchctl load $(PLIST_PATH)
	@echo "Installed launchd service: $(PLIST_PATH) (logs: $(LOG_PATH))"

service-uninstall:
	launchctl unload $(PLIST_PATH) 2>/dev/null || true
	rm -f $(PLIST_PATH)
	@echo "Removed launchd service: $(PLIST_PATH)"
