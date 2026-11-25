BIN_DIR ?= $(HOME)/.local/bin
PLIST_PATH ?= $(HOME)/Library/LaunchAgents/io.shadewright.sablecourtd.plist
LOG_PATH ?= $(HOME)/Library/Logs/sablecourtd.log
MANIFESTS ?= $(PWD)/examples/local-demo.yaml

.PHONY: test vet build install demo agent-image service-install service-uninstall docs-serve docs-build docs-check docs-requirements

test:
	go test -race ./...

vet:
	go vet ./...

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

# Docs site (Zensical, pinned and hash-locked in requirements-docs.txt). Local only: nothing here publishes it.
# One-time setup: python3 -m venv .venv && .venv/bin/pip install --require-hashes -r requirements-docs.txt
docs-serve: ## Preview the docs site on localhost:8000.
	.venv/bin/zensical serve

docs-build: ## Build the docs site into site/, failing on any warning (broken link or anchor) not in scripts/docs-known-warnings.txt.
	python3 -I scripts/docs-build.py .venv/bin/zensical

docs-check: ## Check nav against docs/ (scripts/docs-pending.txt lists pages not written yet), then build strictly.
	python3 -I scripts/docs-nav-check.py
	$(MAKE) docs-build

docs-requirements: ## Regenerate requirements-docs.txt (versions and sha256 hashes) from requirements-docs.in.
	pipx run --spec 'pip-tools==7.6.2' pip-compile --generate-hashes --strip-extras --allow-unsafe --annotation-style=line \
		--output-file=requirements-docs.txt requirements-docs.in
