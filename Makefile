.PHONY: build test install-bin install uninstall clean help

BINARY_NAME=wolweb
BINARY_PATH=/usr/local/bin/$(BINARY_NAME)
SERVICE_NAME=wolweb.service
SERVICE_PATH=/etc/systemd/system/$(SERVICE_NAME)

build: $(BINARY_NAME)

$(BINARY_NAME): wolweb.go template.html go.mod go.sum
	go build -trimpath -o $(BINARY_NAME) .

test:
	go test -race ./...

install-bin: build
	sudo install -m 0755 $(BINARY_NAME) $(BINARY_PATH)

install:
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
	@echo "  make install      - Install systemd service file"
	@echo "  make uninstall    - Stop and remove the systemd service"
	@echo "  make clean        - Remove built binary"
