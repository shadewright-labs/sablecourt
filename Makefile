BIN_DIR ?= $(HOME)/.local/bin

.PHONY: test vet build install demo

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

demo:
	go run ./cmd/sablecourtd --manifests=./examples/local-demo.yaml
