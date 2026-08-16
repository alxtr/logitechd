# logitechd

`logitechd` is a clean-room Go daemon for configuring Logitech mice on Linux.
It communicates with already-paired Logitech Bolt and Unifying receivers over
HID++, configures device features, and exposes remapped controls through Linux
`uinput`.

The current device implementation targets the **Logitech MX Master 3S**. The
receiver and HID++ transport layers are reusable for future Logitech devices.

## Supported hardware

| Component | Status |
| --- | --- |
| Logitech Bolt receiver | Supported |
| Logitech Unifying receiver | Supported |
| MX Master 3S | Supported |
| Other Logitech mice | Not yet guaranteed |

The receiver must already be paired with the mouse. Pairing and unpairing are
not currently implemented.

## Features

* Automatic Bolt/Unifying receiver discovery
* Receiver and wireless-device reconnect handling
* MX Master 3S SmartShift configuration
* Hi-res scrolling and thumb-wheel configuration
* DPI configuration
* Button remapping, key actions, scrolling, axes, and gestures
* Strict YAML configuration
* Optional systemd service

## Requirements

* Linux with HIDRAW and `uinput` support
* Go 1.23 or newer to build from source
* A paired Logitech Bolt or Unifying receiver
* Read/write access to the receiver's `/dev/hidraw*` node
* Read/write access to `/dev/uinput`

The daemon needs elevated device permissions, but it does not require a GUI or
D-Bus session.

## Quick start

Clone and build the daemon:

```sh
git clone https://github.com/atremb/logitechd.git
cd logitechd
go build -o logitechd ./cmd/logitechd
```

Copy and edit the example configuration:

```sh
cp example.yaml config.yaml
$EDITOR config.yaml
```

The example targets an MX Master 3S named `MX Master 3S` in receiver slot `2`.
For a Unifying receiver, change:

```yaml
receiver:
  type: unifying
```

Validate the configuration without opening hardware:

```sh
./logitechd -config ./config.yaml -validate
```

Run manually as root for an initial hardware test:

```sh
sudo ./logitechd -config ./config.yaml
```

## YAML configuration

Configuration is strict YAML. Unknown fields, invalid values, and multiple YAML
documents are rejected. A minimal configuration is:

```yaml
receiver:
  type: bolt

device:
  name: MX Master 3S
  index: 2

smart_shift:
  enabled: false
  threshold: 100
  torque: 100
```

The receiver path is discovered automatically. On systems with multiple
receivers, set it explicitly:

```yaml
receiver:
  type: bolt
  path: /dev/hidraw2
```

See [`example.yaml`](example.yaml) for button, wheel, DPI, and gesture examples.

## Device permissions

The supplied service runs as the `logitechd` user with the `input` group. Create
the service account before installing the unit:

```sh
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin logitechd
sudo usermod --append --groups input logitechd
```

Check the device permissions on your system:

```sh
ls -l /dev/hidraw* /dev/uinput
```

If your distribution does not already grant the `input` group access, create a
udev rule such as `/etc/udev/rules.d/70-logitechd.rules`:

```udev
KERNEL=="hidraw*", ATTRS{idVendor}=="046d", GROUP="input", MODE="0660"
KERNEL=="uinput", GROUP="input", MODE="0660"
```

Reload the rules and reconnect the receiver:

```sh
sudo udevadm control --reload-rules
sudo udevadm trigger
```

## systemd installation

Build and install the binary, configuration, and service:

```sh
sudo install -Dm755 logitechd /usr/local/bin/logitechd
sudo install -d -o root -g logitechd -m 0750 /etc/logitechd
sudo install -o root -g logitechd -m 0640 config.yaml /etc/logitechd/config.yaml
sudo install -Dm644 logitechd.service /usr/lib/systemd/system/logitechd.service
```

Enable and inspect the new service:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now logitechd.service
sudo systemctl status logitechd.service
sudo journalctl -u logitechd.service -f
```

## Troubleshooting

Confirm that the receiver is visible to USB:

```sh
lsusb | grep -i logitech
```

A Bolt receiver commonly appears as USB ID `046d:c548`. If the daemon reports
permission errors, inspect the ownership of `/dev/hidraw*` and `/dev/uinput`,
then reload the udev rules. If the device is not selected, verify its exact
HID++ name and wireless slot in the YAML configuration.

## Development

Run the complete hardware-free verification suite:

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

Linux HIDRAW, HID++, and `uinput` integration is isolated from the MX Master-specific feature
package so additional device implementations can be added independently.

## Limitations

* Pairing and unpairing are not implemented; devices must already be paired.
* The daemon is Linux-specific.
* The current feature and action implementation is MX Master-specific.
* Device selection uses an exact name and/or wireless index.
