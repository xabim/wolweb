.PHONY: build clean install uninstall

BINARY_NAME=wolweb
BINARY_PATH=/usr/local/bin/$(BINARY_NAME)
SERVICE_NAME=wolweb.service
SERVICE_PATH=/etc/systemd/system/$(SERVICE_NAME)

build: $(BINARY_NAME)
	@echo "✓ Binary built and installed to $(BINARY_PATH)"

$(BINARY_NAME): wolweb.go
	@echo "Building $(BINARY_NAME)..."
	go build -o $(BINARY_NAME) wolweb.go
	@echo "Installing to $(BINARY_PATH)..."
	sudo cp $(BINARY_NAME) $(BINARY_PATH)
	@echo "Setting executable permissions..."
	sudo chmod +x $(BINARY_PATH)

clean:
	@echo "Cleaning up..."
	rm -f $(BINARY_NAME)
	@echo "✓ Clean complete"

install:
	@echo "Installing systemd service..."
	@if [ -f "$(SERVICE_NAME)" ]; then \
		sudo cp $(SERVICE_NAME) $(SERVICE_PATH); \
		sudo systemctl daemon-reload; \
		echo "✓ Service installed at $(SERVICE_PATH)"; \
	else \
		echo "✗ $(SERVICE_NAME) not found"; \
		exit 1; \
	fi

uninstall:
	@echo "Removing systemd service..."
	@if systemctl is-enabled $(BINARY_NAME) >/dev/null 2>&1; then \
		sudo systemctl stop $(BINARY_NAME); \
		sudo systemctl disable $(BINARY_NAME); \
	fi
	sudo rm -f $(SERVICE_PATH)
	sudo systemctl daemon-reload
	@echo "✓ Service uninstalled"

help:
	@echo "Available targets:"
	@echo "  make build       - Compile and install binary to /usr/local/bin"
	@echo "  make clean       - Remove built binaries"
	@echo "  make install     - Install systemd service"
	@echo "  make uninstall   - Remove systemd service"
	@echo "  make help        - Show this help message"
