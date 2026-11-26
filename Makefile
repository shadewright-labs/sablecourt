BIN_DIR ?= $(HOME)/.local/bin
PLIST_PATH ?= $(HOME)/Library/LaunchAgents/io.shadewright.sablecourtd.plist
LOG_PATH ?= $(HOME)/Library/Logs/sablecourtd.log
MANIFESTS ?= $(PWD)/examples/local-demo.yaml

.PHONY: test vet lint lint-config vulncheck build install demo agent-image service-install service-uninstall docs docs-requirements docs-check

# Pinned tool versions, run with `go run` (checked through the Go checksum database). CI calls the same targets.
GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION ?= v1.9.0
GOLANGCI_LINT = go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

test:
	go test -race ./...

vet:
	go vet ./...

# golangci-lint with .golangci.yml (what the lint job of CI runs).
lint:
	$(GOLANGCI_LINT) run ./...

lint-config:
	$(GOLANGCI_LINT) config verify

# Known vulnerabilities reached from the code, standard library of the current Go release included.
vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# Reference pages under docs/reference are generated from the code or checked
# against it by tests, which `go test ./...` (and so CI) also runs.
DOCS_TESTS = TestCLIReferenceIsCurrent|TestConfigurationStateDirectory|TestConfigurationEnvironmentVariables|TestHTTPReferenceListsEveryRoute|TestManifestReferenceMatchesTypesAndLoader|TestLimitsReferenceMatchesConstants|TestMetricsReferenceListsEveryMetric
DOCS_PKGS = ./cmd/sablectl ./cmd/sablecourtd ./internal/config ./internal/refdocs ./internal/telemetry
ZENSICAL ?= .venv/bin/zensical

# docs regenerates the generated blocks of docs/reference/cli.md. Each command
# package saves its rendered block to its own file in a scratch directory, and
# one writer puts them into the page.
docs:
	go run ./internal/clidoc/write docs/reference/cli.md ./cmd/sablectl ./cmd/sablecourtd

# docs-requirements regenerates the hash-locked requirements-docs.txt from requirements-docs.in.
docs-requirements:
	pipx run --spec 'pip-tools==7.6.2' pip-compile --generate-hashes --strip-extras --allow-unsafe --annotation-style=line \
		--output-file=requirements-docs.txt requirements-docs.in

# docs-check fails when any reference page is stale, when the Zensical build reports an issue,
# or when the built site has a broken internal link, broken anchor, missing image or third-party asset.
# The reference tests never write: the block directory variable is cleared so the render step cannot run.
docs-check:
	env -u SABLECOURT_CLI_BLOCK_DIR go test -count=1 -run '$(DOCS_TESTS)' $(DOCS_PKGS)
	$(ZENSICAL) build --clean --strict
	python3 -I hack/docs-site-check.py check site hack/docs-pages-pending.txt .

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
