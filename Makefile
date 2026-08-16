SHELL := /bin/sh

GO ?= go
BINARY ?= logitechd
CONFIG ?= example.yaml
CMD ?= ./cmd/logitechd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf 'dev')
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || printf 'unknown')
LDFLAGS ?= -X main.version=$(VERSION) -X main.commit=$(COMMIT)

PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
SYSCONFDIR ?= /etc/logitechd
UDEVRULEDIR ?= /etc/udev/rules.d
UDEVRULE ?= 70-logitechd.rules
SYSTEMD_UNITDIR ?= /usr/lib/systemd/system
SERVICE_USER ?= logitechd
SERVICE_GROUP ?= logitechd

.PHONY: all build test vet fmt check validate install install-user install-udev clean

all: build

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

check: test vet build

validate: build
	./$(BINARY) -config $(CONFIG) -validate

# Install everything needed by the systemd service. Existing configuration is
# never overwritten, and the service is not enabled or started automatically.
install: build install-user
	@if [ "`id -u`" -ne 0 ]; then \
		echo "install must be run as root (try: sudo make install)" >&2; \
		exit 1; \
	fi
	install -Dm755 "$(BINARY)" "$(DESTDIR)$(BINDIR)/$(BINARY)"
	install -d -o root -g "$(SERVICE_GROUP)" -m 0750 "$(DESTDIR)$(SYSCONFDIR)"
	@if [ ! -e "$(DESTDIR)$(SYSCONFDIR)/config.yaml" ]; then \
		install -o root -g "$(SERVICE_GROUP)" -m 0640 "$(CONFIG)" "$(DESTDIR)$(SYSCONFDIR)/config.yaml"; \
	else \
		echo "preserving existing $(DESTDIR)$(SYSCONFDIR)/config.yaml"; \
	fi
	install -Dm644 logitechd.service "$(DESTDIR)$(SYSTEMD_UNITDIR)/logitechd.service"
	$(MAKE) --no-print-directory install-udev

# Create the locked service account when it does not already exist.
install-user:
	@if [ "`id -u`" -ne 0 ]; then \
		echo "install-user must be run as root (try: sudo make install-user)" >&2; \
		exit 1; \
	fi
	@if ! getent group "$(SERVICE_GROUP)" >/dev/null; then \
		groupadd --system "$(SERVICE_GROUP)"; \
	fi
	@if ! getent passwd "$(SERVICE_USER)" >/dev/null; then \
		useradd --system --gid "$(SERVICE_GROUP)" --no-create-home \
			--shell /usr/sbin/nologin "$(SERVICE_USER)"; \
	fi

# Install and apply the udev permissions needed by the service account. When
# DESTDIR is used for staging, do not reload or trigger the host's udev rules.
install-udev:
	@if [ "`id -u`" -ne 0 ]; then \
		echo "install-udev must be run as root (try: sudo make install-udev)" >&2; \
		exit 1; \
	fi
	install -Dm644 "$(UDEVRULE)" "$(DESTDIR)$(UDEVRULEDIR)/$(UDEVRULE)"
	@if [ -z "$(DESTDIR)" ]; then \
		if ! command -v udevadm >/dev/null 2>&1; then \
			echo "udevadm not found; reconnect the receiver after installing the udev rule" >&2; \
		else \
			udevadm control --reload-rules; \
			udevadm trigger --subsystem-match=hidraw --action=add; \
			udevadm trigger --subsystem-match=misc --sysname-match=uinput --action=add; \
			udevadm settle; \
		fi; \
	fi

clean:
	rm -f "$(BINARY)"
