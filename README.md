# hwi-api

`hwi-api` is a host inventory REST API: an unprivileged agent that collects a Linux host's hardware and OS inventory and serves it read-only over a REST
API. It reads the host's sysfs, procfs, udev database, netlink and firmware tables on a background schedule and keeps the last result of each subsystem in
memory, so a request is answered from that cache rather than triggering a fresh hardware probe. Anything that speaks HTTPS and JSON can consume it.

## What it serves

Each subsystem refreshes on its own schedule and has its own route, `/api/v1/inventory/{subsystem}`; `/api/v1/inventory` serves them all at once.

| Area | Subsystem names |
| --- | --- |
| Processors and memory | `cpu`, `memory` (including the installed modules), `topology` (NUMA nodes, caches and distances) |
| Storage | `block` (disks, partitions, device-mapper and MD devices, mount points) |
| Network | `network` (NICs, with link modes, FEC, offload features and permanent MAC addresses), `links` (every interface's state and addresses) |
| PCI | `pci` (with physical slots, link speed and width, SR-IOV, and VPD part and serial numbers), `slots` (expansion slots, empty ones included) |
| Firmware identity | `product`, `baseboard`, `chassis`, `bios` |
| udev | `udev-block`, `udev-net`, `udev-pci`, `udev-nvme`, `udev-scsi`, `udev-drm`, `udev-vfio` |
| Operating system | `os`, `virtualization` |

## How it runs

The agent listens on two ports. The **API listener** (default `8096`) serves the inventory over TLS, to clients that pass an IP allow list and
authenticate with a client certificate or a bearer token. The **ops listener** (default `9096`, loopback only by default) serves `/healthz`, `/readyz`,
Prometheus `/metrics` and `/status` over plain HTTP, without authentication.

The daemon runs as its own unprivileged user with no Linux capabilities, under a hardened `systemd` unit, and nothing either listener serves can change
the host. The few sources only root can read (DMI serial numbers and UUID, the SMBIOS table and PCI Vital Product Data) come from a socket-activated
privileged helper that answers a fixed set of requests from the daemon alone, in a sandbox that sees little more than `/sys`.

It ships as a single static binary with its `systemd` units, packaged as a `.deb` or `.rpm` for Linux on amd64 and, best effort, arm64. Install the
package, give `/etc/hwi-api/config.yaml` TLS material, an authentication mechanism and an allow list, then run `systemctl enable --now hwi-api`: the
package deliberately does not start the service itself. The [Quickstart](#quickstart) walks through each step.

- [`docs/consumers.md`](docs/consumers.md) — for anyone writing a client: the envelope, every payload, and the fields most likely to be misread.
- [`docs/security.md`](docs/security.md) — the threat model, the controls a request meets in order, the helper, and what is deliberately not defended.
- [`docs/api/openapi.yaml`](docs/api/openapi.yaml) — the published API contract: every path, status code, header and error code.

> [!WARNING]
> `api.access.allow` is empty by default, and an empty allow list denies **every** client — there is no address that is safe to allow automatically. The
> agent still starts and the listener still binds, but no request will ever be admitted until you populate this list. See
> [Configuration reference](#configuration-reference).
>
> `api.tls.enabled` and `api.auth.enabled` both default to `true`/on, and each can be turned off independently — the realistic case being a trusted local
> proxy that already terminates TLS and authenticates callers in front of this agent. `api.auth.enabled` defaults to `true` even when the whole `api.auth`
> block is absent, so a forgotten or mistyped section still enforces authentication rather than silently disabling it; set `api.auth.enabled: false`
> explicitly to opt out. Disabling either key logs a startup warning; disabling both at once serves hardware inventory, including DMI serial numbers and
> hardware UUIDs, in cleartext with no authentication to anything `api.access.allow` admits. Do this deliberately, and never on a routable interface. See
> [`docs/security.md`](docs/security.md) for the full threat model.

## Quickstart

These steps take a freshly installed package to a successful authenticated request. They use a self-signed certificate and a bearer token for brevity;
either mechanism may be replaced with mTLS against your own CA (see [Authentication examples](#authentication-examples)).

### 1. Install the package

```sh
# Debian/Ubuntu
sudo apt install ./hwi-api_<version>_linux_amd64.deb

# RHEL/Fedora
sudo dnf install ./hwi-api_<version>_linux_amd64.rpm
```

Both artifacts follow the same `hwi-api_<version>_linux_<arch>.<ext>` naming (goreleaser's default, not the traditional `name-version.arch.rpm` RPM
convention) — check the actual filename in your downloaded release rather than assuming the conventional form for either package format.

Installing does **not** start or enable the service: the shipped `/etc/hwi-api/config.yaml` has no TLS material and no authentication mechanism enabled,
so it cannot start yet. That is intentional — see [Security](docs/security.md) for why. `/etc/hwi-api` (including `config.yaml`) survives a later removal
of the package.

### 2. Provide TLS material

The unit loads the certificate and key systemd's credential mechanism from fixed host paths:

```sh
sudo mkdir -p /etc/hwi-api/tls
sudo openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout /etc/hwi-api/tls/key.pem -out /etc/hwi-api/tls/cert.pem \
  -subj "/CN=hwi-api.example.internal" \
  -addext "subjectAltName=DNS:hwi-api.example.internal,IP:10.0.0.5"
sudo chmod 0600 /etc/hwi-api/tls/key.pem /etc/hwi-api/tls/cert.pem
sudo chown root:root /etc/hwi-api/tls/key.pem /etc/hwi-api/tls/cert.pem
```

Use a certificate issued by your own CA in production; a self-signed one is shown here only to get a first request working. `config.yaml` already points
`api.tls.cert_file`/`api.tls.key_file` at `credential:tls_cert.pem`/`credential:tls_key.pem`. The unit loads every file in `/etc/hwi-api/tls`, which
the package creates empty, as a credential named `tls_<file name>`, so the two files above are exactly what those names resolve to. With
`api.tls.enabled: false` the directory can stay empty; with TLS enabled and a file missing, the agent exits at startup naming the setting, such as `api.tls.cert_file`.

#### Optional: encrypt the certificate and key with `systemd-creds`

By default the key sits in `/etc/hwi-api/tls` as plain PEM, protected only by its `root:root 0600` mode, so a disk image or backup of `/etc` carries it
too. `systemd-creds` can store it encrypted instead, bound to this host: with the host key in `/var/lib/systemd/credential.secret` and, when the machine
has one, its TPM2. systemd decrypts it only when it starts the service, and the agent reads it exactly as before, so `config.yaml` does not change. This
needs systemd 250 or later.

Encrypt both files, in place of the plain PEM files above. Every file in `/etc/hwi-api/tls` must be encrypted, because the unit loads the directory as
a whole. `--name` must be the credential name the unit derives from the file name, `tls_<file name>`: without it, `systemd-creds` embeds the output
file's name, `cert.pem`, and systemd refuses the credential when the service starts.

```sh
# cert.pem and key.pem are the plain PEM files, somewhere other than /etc/hwi-api/tls.
sudo systemd-creds encrypt --name=tls_cert.pem cert.pem /etc/hwi-api/tls/cert.pem
sudo systemd-creds encrypt --name=tls_key.pem key.pem /etc/hwi-api/tls/key.pem
sudo chmod 0600 /etc/hwi-api/tls/cert.pem /etc/hwi-api/tls/key.pem
sudo shred -u key.pem  # keep the original only where you keep keys, such as a vault
```

Then tell the unit to decrypt the directory, with a drop-in that survives package upgrades:

```sh
sudo systemctl edit hwi-api
```

```ini
[Service]
LoadCredential=
LoadCredentialEncrypted=tls:/etc/hwi-api/tls
```

The empty `LoadCredential=` clears the unit's own line; the next loads the same directory, decrypting each file. Restart with
`sudo systemctl restart hwi-api` and check `journalctl -u hwi-api`. A file that is not encrypted, or was encrypted with a different `--name`, stops the
service from starting, reported as `Failed to set up credentials`; some systemd versions give only "Protocol error" as the reason.

The encrypted files are useful only on this host. Re-imaging it, or clearing or replacing its TPM2, makes them undecryptable, so keep the original key
where you can encrypt it again; to renew the certificate, encrypt the new files the same way and restart the service. The default key source, `auto`,
uses both the host key and the TPM2 when the machine has one, and the host key alone otherwise; `systemd-creds has-tpm2` shows whether a TPM2 is
available, and `--with-key=host` or `--with-key=tpm2` picks one source explicitly.

### 3. Choose an authentication mechanism

For a bearer token:

```sh
TOKEN=$(openssl rand -hex 32)
DIGEST=$(printf '%s' "$TOKEN" | sha256sum | awk '{print $1}')
echo "Token (keep this, it is shown only once): $TOKEN"
```

Save `$TOKEN` somewhere the calling client can read it; only its digest goes into the agent's configuration. For mTLS instead, generate or obtain a client
CA certificate and point `api.auth.mtls.client_ca_file` at it (see [Configuration reference](#configuration-reference)).

### 4. Edit the configuration

Edit `/etc/hwi-api/config.yaml` (installed with mode `0640`, owned `root:hwi-api`, and marked as a config file that package upgrades never overwrite):

```yaml
api:
  tls:
    cert_file: credential:tls_cert.pem
    key_file: credential:tls_key.pem
  auth:
    token:
      enabled: true
      hashes: ["sha256:<DIGEST from step 3>"]
  access:
    allow: ["10.0.0.0/16"] # the networks that may reach the API listener; REQUIRED, see the warning above
```

Every key the decoder understands, active at its default, is already present in the shipped file with an explanatory comment; you are editing values in
place, not writing a file from scratch. The decoder is strict — an unrecognised or misspelled key is a startup error, not a silently ignored one, and
so is a second, non-empty YAML document after a `---` line. An empty trailing `---` or a comment-only document is accepted.

### 5. Enable and start the service

```sh
sudo systemctl enable --now hwi-api
sudo systemctl status hwi-api
```

### 6. Make the first request

```sh
curl --cacert /etc/hwi-api/tls/cert.pem \
  -H "Authorization: Bearer $TOKEN" \
  https://hwi-api.example.internal:8096/api/v1/inventory/cpu
```

A healthy agent also answers on its plaintext, unauthenticated ops listener, which defaults to loopback only:

```sh
curl http://127.0.0.1:9096/readyz
```

## Endpoints

### API listener (`api.bind`, default port `8096`, TLS, authenticated)

| Method    | Path                          | Description                                                                             |
| --------- | ----------------------------- | ---------------------------------------------------------------------------------------- |
| GET, HEAD | `/api/v1/inventory`           | A full snapshot: every enabled subsystem that has collected at least once.               |
| GET, HEAD | `/api/v1/inventory/{subsystem}` | One subsystem's current inventory. Any other method on either path returns `405`.      |

`{subsystem}` is one of: `cpu`, `memory`, `topology`, `block`, `network`, `links`, `pci`, `slots`, `chassis`, `bios`, `baseboard`, `product`,
`udev-block`, `udev-net`, `udev-pci`, `udev-nvme`, `udev-scsi`, `udev-drm`, `udev-vfio`, `os`, `virtualization`.
`all` is not a valid value here — the full snapshot is its own route above.

Every response is wrapped in an envelope carrying `schema_version`, `subsystem`, `host_id`, `collected_at`, `complete` and `data`, plus optional
`disabled` and `errors`. The full contract, every status code, and every error code are published in [`docs/api/openapi.yaml`](docs/api/openapi.yaml); do
not rely on a second, hand-copied description of it here or elsewhere, because it will drift. Consumers writing a client should also read
[`docs/consumers.md`](docs/consumers.md), which covers the fields most likely to be misread.

### Ops listener (`ops.bind`, default port `9096`, plain HTTP, unauthenticated, loopback-only by default)

| Method | Path        | Description                                                                     |
| ------ | ----------- | -------------------------------------------------------------------------------- |
| GET    | `/healthz`  | Liveness: `200` once the process is serving, regardless of readiness.            |
| GET    | `/readyz`   | Readiness: `200` once the first inventory collection has completed and the API listener is accepting connections; `503` before that or while draining for shutdown. |
| GET    | `/metrics`  | Prometheus text-format metrics.                                                  |
| GET    | `/status`   | Agent name, version, commit, build date, Go version, status and uptime — never host identity, serials or UUIDs. |

The ops listener opens before the first inventory collection starts, so `/healthz` answers even while a slow first collection is still running. The API
listener opens only once that collection has finished, and `/readyz` turns `200` only once the API listener is bound, so a ready agent is always reachable
on its API port.

This listener has no TLS and no authentication; it is protected only by `ops.access.allow`/`ops.access.deny`, and its timeouts and concurrency limit are
fixed in the binary rather than configurable. It serves at most 16 requests at once, not counting `/healthz`; beyond that, a request gets
`503`/`too_many_requests` with `Retry-After: 1`, as on the API listener. `/healthz` is exempt so that liveness keeps answering `200` while the process is
serving, even when the limit is full. See [`docs/security.md`](docs/security.md) for the reasoning.

## Authentication examples

Either mechanism, when enabled, independently admits a request; a server with both enabled accepts a request that satisfies just one of them.

### mTLS

```sh
curl --cacert /etc/hwi-api/tls/cert.pem \
  --cert /path/to/client.pem --key /path/to/client-key.pem \
  https://hwi-api.example.internal:8096/api/v1/inventory
```

If `api.auth.mtls.allowed_subjects` is set, read the warning in [`docs/security.md`](docs/security.md) about the two pin forms (`CN=...`/full DN vs. a
bare name) before relying on it — a pin containing `=` never matches a Subject Alternative Name, only the Subject DN or CN.

### Bearer token

```sh
curl --cacert /etc/hwi-api/tls/cert.pem \
  -H "Authorization: Bearer $TOKEN" \
  https://hwi-api.example.internal:8096/api/v1/inventory
```

### Conditional and forced-refresh requests

```sh
# Revalidate: 304 with an empty body if nothing changed since the last ETag.
curl --cacert cert.pem -H "Authorization: Bearer $TOKEN" \
  -H 'If-None-Match: W/"3fae1c9d1b3a7e9c4f7b2a6d8e5c0f1a2b3c4d5e6f708192a3b4c5d6e7f80912"' \
  https://hwi-api.example.internal:8096/api/v1/inventory/pci

# Force a synchronous re-collection instead of serving the cached result.
curl --cacert cert.pem -H "Authorization: Bearer $TOKEN" \
  "https://hwi-api.example.internal:8096/api/v1/inventory/pci?refresh=true"
```

`refresh=true` is recognised only as that exact literal; anything else, including `false` or an empty value, behaves as if it were absent. See
[`docs/consumers.md`](docs/consumers.md) for when forcing a refresh is warranted — it is materially more expensive than the default cached read.

## Configuration reference

The configuration file is a single YAML document, decoded strictly: an unrecognised key, or a second document with any settings in it, is a
startup error. Paths under `api.tls`, `api.auth.mtls.client_ca_file` and
`api.auth.token.hashes_file` accept a `credential:<name>` reference, resolved under `$CREDENTIALS_DIRECTORY` (populated by the unit's `LoadCredential=`
directives) instead of a literal path.

| Key                                | Default                        | Notes                                                                                          |
| ----------------------------------- | ------------------------------- | ------------------------------------------------------------------------------------------------ |
| `api.bind.addresses`                | `[]`                             | Literal addresses to bind. Empty means every address. May be combined with `interfaces`.        |
| `api.bind.interfaces`               | `[]`                             | Interface names, resolved to their current addresses at startup.                                |
| `api.bind.port`                     | `8096`                           |                                                                                                  |
| `api.tls.enabled`                   | `true`                           | Set `false` only when a trusted local proxy already terminates TLS in front of this agent; logs a startup warning. |
| `api.tls.cert_file`                 | *(none — required when enabled)* | Required, and validated, only while `api.tls.enabled` is `true`.                                 |
| `api.tls.key_file`                  | *(none — required when enabled)* | Required, and validated, only while `api.tls.enabled` is `true`.                                 |
| `api.tls.min_version`               | `"1.3"`                          | `"1.2"` or `"1.3"` only. `"1.2"` pins the cipher suite list to AEAD-only suites. Validated even when TLS is disabled. |
| `api.auth.enabled`                  | `true`                           | Defaults to `true` even when the whole `api.auth` block is absent. While `true`, at least one of `mtls.enabled`/`token.enabled` must also be `true` (`mtls.enabled` counts only with `api.tls.enabled: true`), or the agent refuses to start. Set `false` to disable authentication deliberately; logs a startup warning, and the `mtls`/`token` settings are then ignored. |
| `api.auth.mtls.enabled`             | `false`                          | One of the two mechanisms `api.auth.enabled: true` requires at least one of. Needs `api.tls.enabled: true`: without TLS at this listener it has no effect, and it cannot be the only mechanism. |
| `api.auth.mtls.client_ca_file`      | *(none)*                        | Required when `api.auth.enabled`, `mtls.enabled` and `api.tls.enabled` are all `true`; ignored when `api.auth.enabled` or `api.tls.enabled` is `false`. |
| `api.auth.mtls.allowed_subjects`    | `[]`                             | Empty means any certificate the CA vouched for is accepted. See the pinning warning above.       |
| `api.auth.token.enabled`            | `false`                          | One of the two mechanisms `api.auth.enabled: true` requires at least one of.                     |
| `api.auth.token.hashes`             | `[]`                             | SHA-256 digests only (`sha256:<64 hex chars>`), never plaintext tokens.                          |
| `api.auth.token.hashes_file`        | *(none)*                        | Unioned with `hashes` when both are set. At least one digest, from either, is required when both `api.auth.enabled` and `token.enabled` are `true`. |
| `api.access.allow`                  | `[]`                             | **Empty denies every client.** CIDR prefixes or bare addresses, either address family.           |
| `api.access.deny`                   | `[]`                             | Evaluated before `allow`, and wins.                                                               |
| `api.timeouts.read_header`          | `5s`                             | Must be a positive duration, like the other three timeouts: zero or a negative value is refused at startup rather than disabling the deadline. |
| `api.timeouts.read`                 | `10s`                            | Must be a positive duration.                                                                     |
| `api.timeouts.write`                | `30s`                            | Must be a positive duration.                                                                     |
| `api.timeouts.idle`                 | `60s`                            | Must be a positive duration.                                                                     |
| `api.max_concurrent_requests`       | `32`                             | Beyond this, a request gets `503`/`too_many_requests` with `Retry-After: 1` rather than queuing. |
| `ops.bind.addresses`                | `["127.0.0.1", "::1"]`           | Loopback only by default.                                                                        |
| `ops.bind.interfaces`               | `[]`                             |                                                                                                  |
| `ops.bind.port`                     | `9096`                           |                                                                                                  |
| `ops.access.allow`                  | `["127.0.0.1/32", "::1/128"]`    | Same empty-denies-everything rule as `api.access.allow`.                                         |
| `ops.access.deny`                   | `[]`                             |                                                                                                  |
| `inventory.refresh.interval`        | `1m`                             | The floor for every subsystem's own interval: none refreshes more often than this.               |
| `inventory.refresh.subsystems.<name>.enabled`  | *(unset = enabled)*   | Per-subsystem override, e.g. `subsystems: { udev-drm: { enabled: false } }`.                     |
| `inventory.refresh.subsystems.<name>.interval` | *(unset = the subsystem's default)* | Per-subsystem override, e.g. `subsystems: { pci: { interval: 30m } }`. Defaults are listed below; either way, an interval below `inventory.refresh.interval` is raised to it. |
| `log.level`                         | `info`                           | `debug`, `info`, `warn` or `error`.                                                              |

`ops` carries only `bind` and `access` — its timeouts and concurrency limit are fixed in the binary and are not configurable through this file.

Each subsystem refreshes at its own default interval unless overridden, chosen by how fast its data changes and what its probe costs:

| Subsystems                                                                  | Default interval |
| --------------------------------------------------------------------------- | ---------------- |
| `links`                                                                     | `1m`             |
| `cpu`, `memory`, `topology`, `block`, `network`, `pci`, `virtualization`    | `5m`             |
| `udev-block`, `udev-net`, `udev-pci`, `udev-nvme`, `udev-scsi`, `udev-vfio` | `5m`             |
| `os`                                                                        | `15m`            |
| `udev-drm`                                                                  | `30m`            |
| `chassis`, `bios`, `baseboard`, `product`, `slots`                          | `1h`             |

The effective interval is the override if one is set, else this default, raised to `inventory.refresh.interval` if lower. Validation warns when
`topology` ends up refreshing less often than `cpu` or `memory`, since it is composed from them: keep the three together when changing any of them.

A subsystem disabled here is reported in the snapshot envelope's `disabled` array and answers `404`/`subsystem_disabled` on its own route; it does not
affect `complete`. See [`docs/consumers.md`](docs/consumers.md) for what `complete` does and does not mean.

## Command-line flags

```sh
hwi-api --config /etc/hwi-api/config.yaml
hwi-api --version
```

`--config` (also accepted as `-config`) defaults to `/etc/hwi-api/config.yaml`, or to the path in the `HWI_API_CONFIG` environment variable when that
is set and `--config` is not given. `--version` prints the build name, version, commit, build date and Go version, and exits without starting the agent.

## See also

- [`docs/api/openapi.yaml`](docs/api/openapi.yaml) — the published API contract: every path, status code, header and error code, with examples.
- [`docs/consumers.md`](docs/consumers.md) — a guide for anyone writing a client against this API.
- [`docs/security.md`](docs/security.md) — the threat model, the controls a request meets in order, and what is deliberately not defended.
