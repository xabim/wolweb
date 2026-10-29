.PHONY: build test install-bin install-config install uninstall clean help

BINARY_NAME=wolweb
BINARY_PATH=/usr/local/bin/$(BINARY_NAME)
SERVICE_NAME=wolweb.service
SERVICE_PATH=/etc/systemd/system/$(SERVICE_NAME)
CONFIG_DIR=/etc/wolweb
CONFIG_EXAMPLE=wolweb.toml.example

build: $(BINARY_NAME)

$(BINARY_NAME): wolweb.go template.html go.mod go.sum
	go build -trimpath -o $(BINARY_NAME) .

test:
	go test -race ./...

install-bin: build
	sudo install -m 0755 $(BINARY_NAME) $(BINARY_PATH)

install-config:
	sudo install -d -m 0755 $(CONFIG_DIR)
	sudo install -m 0644 $(CONFIG_EXAMPLE) $(CONFIG_DIR)/config.toml.example
	@test -f $(CONFIG_DIR)/config.toml || sudo install -m 0644 $(CONFIG_EXAMPLE) $(CONFIG_DIR)/config.toml
	@echo "Edit $(CONFIG_DIR)/config.toml (existing file is never overwritten)"

install: install-config
	@test -f "$(SERVICE_NAME)" || { echo "$(SERVICE_NAME) not found"; exit 1; }
	sudo install -m 0644 $(SERVICE_NAME) $(SERVICE_PATH)
	sudo systemctl daemon-reload
	@echo "Service installed at $(SERVICE_PATH)"

uninstall:
	-sudo systemctl disable --now $(BINARY_NAME)
	sudo rm -f $(SERVICE_PATH)
	sudo systemctl daemon-reload
	@echo "Service uninstalled"

clean:
	rm -f $(BINARY_NAME)

help:
	@echo "Available targets:"
	@echo "  make build        - Compile ./wolweb"
	@echo "  make test         - Run tests with the race detector"
	@echo "  make install-bin  - Build and install binary to $(BINARY_PATH)"
	@echo "  make install-config - Install example config to $(CONFIG_DIR) (keeps existing config.toml)"
	@echo "  make install      - Install config and systemd service file"
	@echo "  make uninstall    - Stop and remove the systemd service"
	@echo "  make clean        - Remove built binary"
