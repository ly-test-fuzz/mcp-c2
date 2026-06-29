.PHONY: tidy vet test build smoke clean certs

SERVER_IP ?= 127.0.0.1
CERTS_DIR ?= certs

tidy:
	go mod tidy

vet:
	go vet ./...

test:
	go test ./...

# Generate certificates and sync to embedded data for client compilation.
certs:
	@echo "=== Generating certificates (SAN: $(SERVER_IP)) ==="
	@mkdir -p $(CERTS_DIR) internal/embedded/data
	CGO_ENABLED=0 go run ./server/cmd/mcpc2-hub -gen-certs -certs-dir $(CERTS_DIR) -server-ip $(SERVER_IP)
	cp $(CERTS_DIR)/ca.crt internal/embedded/data/
	cp $(CERTS_DIR)/client.crt internal/embedded/data/
	cp $(CERTS_DIR)/client.key internal/embedded/data/
	@echo "=== Certificates ready ==="

build: certs
	CGO_ENABLED=0 go build -o bin/mcpc2-hub ./server/cmd/mcpc2-hub
	CGO_ENABLED=0 go build -o bin/mcpc2-server ./server/cmd/mcpc2-server
	CGO_ENABLED=0 go build -o bin/mcpc2-client ./client/cmd/mcpc2-client

smoke:
	bash scripts/smoke-mtls.sh

clean:
	rm -rf bin $(CERTS_DIR)
