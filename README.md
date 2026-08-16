# logitechd

`logitechd` is a Linux daemon for already-paired Logitech Bolt and Unifying
receivers. It discovers the receiver HIDRAW node, keeps one shared receiver
session open, applies the configured MX Master features, and sends configured
button/gesture output through Linux uinput.

The current setup is covered by the example: a Bolt receiver with USB ID
`046d:c548`, an automatically discovered `/dev/hidraw*` path, and an MX Master
3S in wireless slot/index `2`. Set `receiver.type: unifying` for a Unifying
receiver. A fixed node can be selected with `receiver.path` or the
`-receiver-path` command-line override.

## Configuration and permissions

Configuration is strict YAML. Unknown fields and multiple YAML documents are
rejected. Start with [`example.yaml`](example.yaml):

```sh
./logitechd -config ./example.yaml -validate
```

Validation does not open HIDRAW or uinput. The daemon waits for a receiver or
paired child that is temporarily absent, and reconnects after receiver loss or
device sleep/wake. It does not log action values or other configuration
secrets.

The runtime user needs read/write access to the selected `/dev/hidraw*` node and
`/dev/uinput`. The supplied systemd unit runs as the dedicated `logitechd`
user/group with `input` as a supplementary group; it does not create that
account or grant device access by itself. Provision the account and verify the
host's udev ownership before enabling the unit:

```sh
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin logitechd
sudo usermod --append --groups input logitechd
```

## Build, test, run, and install

Go 1.23 or newer is required.

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...

go build -o logitechd ./cmd/logitechd
./logitechd -config ./example.yaml
```

An example installation using `/usr/local` and `/etc` is:

```sh
sudo install -d -o root -g logitechd -m 0750 /etc/logitechd
sudo install -Dm755 logitechd /usr/local/bin/logitechd
sudo install -o root -g logitechd -m 0640 example.yaml /etc/logitechd/config.yaml
sudo install -Dm644 logitechd.service /usr/lib/systemd/system/logitechd.service
systemctl daemon-reload
systemctl enable --now logitechd.service
```

The `logitechd.service` unit is independent: installing or starting it does
not stop, replace, or alter any other service unit.

## Status and limitations

This implementation is clean-room work. It uses the project’s own protocol,
configuration, lifecycle, feature, and uinput packages and does not copy source
code, identifiers, comments, or structure from another implementation.

Known limitations:

* There is no pairing API initially. Devices must already be paired to the
  receiver.
* The daemon is Linux-specific because HIDRAW, uinput, and the systemd unit are
  Linux facilities.
* Device selection uses the configured exact name and/or wireless index; a
  device that has been unpaired is not rediscovered through pairing.
* Feature settings are applied when the target becomes ready. Hardware and
  permission failures are reported clearly; ordinary receiver/device loss is
  retried.
