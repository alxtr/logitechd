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
* Automatic configuration recovery after host suspend/resume
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
D-Bus session. When available, it uses systemd-logind on the system D-Bus to
detect host resume and reapply volatile mouse configuration.

## Installation

The minimal systemd installation builds the daemon, creates its locked service
account, installs the configuration and service, and applies the included udev
rules:

```sh
git clone https://github.com/atremb/logitechd.git
cd logitechd
cp example.yaml config.yaml
$EDITOR config.yaml
sudo make install CONFIG=./config.yaml
sudo systemctl daemon-reload
sudo systemctl enable --now logitechd.service
```

The example targets an MX Master 3S named `MX Master 3S` in receiver slot `2`.
For a Unifying receiver, change:

```yaml
receiver:
  type: unifying
```

`make install` preserves an existing `/etc/logitechd/config.yaml`. After the
first installation, edit that file to change the service configuration.

Check the service with:

```sh
sudo systemctl status logitechd.service
sudo journalctl -u logitechd.service -f
```

## Build and run manually

Build and validate a configuration without opening hardware:

```sh
make build
make validate CONFIG=./config.yaml
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

scroll_mode: free_spin

smart_shift:
  threshold: 100
  torque: 100
```

`scroll_mode` controls the main wheel's mechanical mode: `smart_shift` switches
between ratcheted and free-spinning behavior based on wheel speed, `free_spin`
keeps it free-spinning, and `ratchet` keeps it ratcheted. Omit `scroll_mode` to
leave the current device mode unchanged. The optional `smart_shift.threshold`
and `smart_shift.torque` fields tune the switching threshold and ratchet torque.

The daemon reapplies a configured mode at startup and whenever it configures the
mouse again after reconnect or resume. It does not manage the physical wheel
mode button after that initial application, so pressing the button can change
the mode until the next startup, reconnect, or resume.

The receiver path is discovered automatically. On systems with multiple
receivers, set it explicitly:

```yaml
receiver:
  type: bolt
  path: /dev/hidraw2
```

See [`example.yaml`](example.yaml) for button, wheel, DPI, and gesture examples.

## Permission troubleshooting

The standard installation creates the service account and installs a udev rule
that grants the service access to Logitech HIDRAW devices and `uinput`. No
manual permission changes should normally be needed.

If the service reports `permission denied`, inspect the device permissions:

```sh
ls -l /dev/hidraw* /dev/uinput
```

Reinstall and reapply the supplied rule, then restart the service:

```sh
sudo make install-udev
sudo systemctl restart logitechd.service
```

The rules are limited to new Logitech HIDRAW devices and the `uinput` device.
They use normal group permissions without running external commands or adding
package-specific ACL entries, allowing other access rules to coexist:

```udev
ACTION=="add", SUBSYSTEM=="hidraw", ATTRS{idVendor}=="046d", GROUP="input", MODE="0660"
ACTION=="add", SUBSYSTEM=="misc", KERNEL=="uinput", GROUP="input", MODE="0660"
```

When running the daemon manually as a non-root user, add that user to the
`input` group, then log out and back in:

```sh
sudo usermod --append --groups input "$USER"
```

## Troubleshooting

Confirm that the receiver is visible to USB:

```sh
lsusb | grep -i logitech
```

A Bolt receiver commonly appears as USB ID `046d:c548`. If the device is not
selected, verify its exact HID++ name and wireless slot in the YAML
configuration.

## Development

Run the complete hardware-free verification suite:

```sh
make check
```

Linux HIDRAW, HID++, and `uinput` integration is isolated from the MX Master-specific feature
package so additional device implementations can be added independently.

## Limitations

* Pairing and unpairing are not implemented; devices must already be paired.
* The daemon is Linux-specific.
* The current feature and action implementation is MX Master-specific.
* Device selection uses an exact name and/or wireless index.
