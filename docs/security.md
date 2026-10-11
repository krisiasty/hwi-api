# Security

This document is for someone deciding whether `hwi-api` is safe to deploy: the threat model, the controls a request passes through in order, why the
`systemd` unit is built the way it is, and what is deliberately left undefended.

## Threat model

`hwi-api` is a **read-only information source** deployed on a managed network. It probes local hardware and OS state and serves it as JSON; nothing it
exposes lets a caller change host configuration, install software, or otherwise mutate the machine it runs on. The risk this agent introduces is therefore
**disclosure**, not modification: an unauthorized reader learning a host's hardware inventory, including DMI serial numbers and UUIDs, memory module and
disk serial numbers and WWNs (from `block` and the `udev-*` subsystems), PCI cards' part and serial numbers and
vendor-specific identifiers from their VPD, its OS and kernel details, its PCI and network topology, and (indirectly, via
`host_id`) a stable per-host identifier derived from its machine ID.

This document assumes the network the agent's API listener is reachable on is itself managed and access-controlled — an internal network reached by
trusted automation and operators, not the open internet. `hwi-api` adds authentication and an IP allow list on top of that assumption; it is not
designed, and should not be relied upon, to be safe to expose directly to an untrusted network on its own.

`api.tls.enabled` and `api.auth.enabled` both default to on, but each is independently optional — see
[Transport security and authentication are independently optional](#transport-security-and-authentication-are-independently-optional) below for what
each of the four combinations actually means for this threat model, and which of them are appropriate to run.

## Controls, in the order a request meets them

The API listener's middleware chain is fixed and applied in this order (`Recover`, `Observe`, `AccessControl`, `Authenticate`, `LimitConcurrency`, then the
handler); a request that fails an earlier stage never reaches a later one.

1. **Reachability.** `api.bind.addresses`/`api.bind.interfaces` control which local addresses the listener binds at all; `ops.bind` defaults to loopback
   only (`127.0.0.1`, `::1`). Never binding an external address at all is the first line of defense for the ops listener specifically.
2. **Transport security.** `api.tls.enabled` defaults to `true`; while it is, the agent refuses to start without `api.tls.cert_file` and
   `api.tls.key_file`, and the minimum negotiated version is `api.tls.min_version` (`"1.2"` or `"1.3"`, defaulting to `"1.3"`); when lowered to `"1.2"`,
   the cipher suite list is pinned to AEAD-only suites rather than accepting anything TLS 1.2 permits. TLS 1.3 has no cipher suite to pin — every suite it
   offers is already AEAD. Setting `api.tls.enabled: false` is supported — the realistic case is a trusted local proxy that already terminates TLS in
   front of this agent — but it means every byte between that proxy (or, absent one, every client) and the agent crosses the network unencrypted; the
   agent logs a startup warning naming exactly this every time it starts with TLS disabled.
3. **Access control (IP allow/deny lists).** `api.access`/`ops.access` are evaluated from the request's actual peer address (`RemoteAddr`), never from a
   client-controlled header such as `X-Forwarded-For`. **Deny is evaluated before allow, and wins.** An empty `allow` list denies every client — there is
   no built-in safe default, by explicit design, and the shipped example configuration ships that way deliberately. A request that fails this check never
   reaches authentication, so an unauthorized network can neither authenticate nor probe for what authentication mechanisms are enabled.
4. **Authentication.** `api.auth.enabled` defaults to `true` — including when the whole `api.auth` block is absent from the file, so a forgotten or
   mistyped section still enforces authentication rather than silently disabling it. While `true`, at least one of `api.auth.mtls.enabled` or
   `api.auth.token.enabled` must also be `true`, or the agent refuses to start: an *enabled* authentication layer with no mechanism to enforce it is a
   contradiction, not a choice. mTLS counts only while `api.tls.enabled` is `true`, since without TLS at this listener no request carries a client
   certificate; an mTLS-only configuration with TLS off is therefore rejected too. Either mTLS or a bearer token, whichever is enabled, independently
   admits a request; a server with both enabled accepts either. Setting `api.auth.enabled: false` disables authentication entirely and deliberately —
   the agent logs a startup warning every time, precisely because it means every request that passes access control reaches the handler with no
   credential check at all. No credentials at all is a `401`; credentials presented and refused is a `403` — deliberately indistinguishable from an
   access-control rejection, so a caller cannot use the response to enumerate which check it failed.
   - **mTLS** verifies the presented client certificate against `api.auth.mtls.client_ca_file`, and optionally narrows further with
     `api.auth.mtls.allowed_subjects`. **Read the pinning warning below before using this option** — it has a genuine foot-gun in how a pin is matched.
   - **Bearer tokens** are checked as SHA-256 digests, compared in constant time against every configured digest so that timing does not reveal how far
     down the list a match was found. Only digests are ever configured (`api.auth.token.hashes`/`hashes_file`) — the plaintext token is never stored by
     the agent, only by whoever issued it.
5. **Concurrency limiting.** `api.max_concurrent_requests` (default `32`) bounds how many requests are served at once; beyond that, a request is rejected
   immediately with `503`/`too_many_requests` and `Retry-After: 1` rather than queued, so a burst cannot pile up unbounded work.
6. **The handler itself** reads from the agent's in-memory inventory cache (or, for `?refresh=true`, triggers a synchronous re-probe) and serves it. No
   handler on the API listener accepts a request body or performs a write.

The ops listener (`/healthz`, `/readyz`, `/metrics`, `/status`) skips steps 2 and 4 entirely: it is **plaintext HTTP with no authentication at all**,
protected only by its own access list (step 3) and defaulting to loopback. This is a deliberate scope decision, not an oversight: it exists for a local
Prometheus node agent, a health checker, or an operator already on the box, none of which need TLS or credentials to reach loopback. Its `/status`
response reports the agent's own build identity (name, version, commit) and never host identity, serials, or UUIDs, so an accidental broadening of its
access list is lower-consequence than the same mistake on the API listener — though still not something to do deliberately. Its timeouts and concurrency
cap are fixed in the binary specifically so they cannot be loosened through configuration. The cap is 16 concurrent requests, well above the few local
clients it expects, and applies to every ops endpoint except `/healthz`. Liveness must keep answering `200` while the process is serving: a limiter full
of `/metrics` requests answering it with `503` would let a supervisor restart a healthy agent under exactly the load the limiter exists to contain.
`/healthz` returns a small fixed response and still passes access control, logging and the server's own timeouts; it only skips the concurrency
limiter.

### Transport security and authentication are independently optional

Two keys each default to `true`/on and can each be turned off without touching the other, which gives four combinations:

- **`api.tls.enabled`** — `false` serves the API listener as plaintext HTTP. See control #2 above.
- **`api.auth.enabled`** — `false` skips authentication entirely; no credential is ever checked. This default holds even when the whole `api.auth`
  block is missing from the file, so omitting or mistyping the section does **not** produce this row — it produces a startup error instead (see control
  #4 above and [Why the package does not start](#why-the-package-does-not-start-or-enable-the-service-on-install)). Reaching this row requires writing
  `api.auth.enabled: false` explicitly. The `mtls` and `token` settings are then ignored: a mechanism left enabled needs no CA file or digest, and the agent logs
  that it has no effect.

Only one of the four combinations is never appropriate regardless of network placement:

| `api.tls.enabled` | `api.auth.enabled` | What it means                                                                                                                     | Appropriate when                                                                                     |
| --------- | --------- | ----------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| `true`    | `true`    | The shipped, secure-by-default posture: encrypted transport plus a credential check. This is what every example in this document assumes unless said otherwise. | Always — this is the default for a reason.                                                    |
| `true`    | `false`   | Transport is encrypted, but any client `api.access.allow` admits is trusted with no credential check at all.                          | A network where `api.access.allow` is drawn tightly enough that reachability itself is the control — e.g. a dedicated management VLAN with no other tenants. Still logged as a loud warning. |
| `false`   | `true`    | Requests are authenticated, but travel in cleartext between the client (or a proxy in front of the agent) and the API listener. Only token authentication can work here: client certificates need TLS terminated by this listener, so mTLS is inert, and an mTLS-only configuration is rejected at startup. | A trusted local proxy on the same host or a securely isolated network segment terminates TLS and forwards plaintext internally, with the agent's own `api.access.allow` restricted to that proxy. |
| `false`   | `false`   | Cleartext transport and no credential check. The API listener serves hardware inventory — including DMI serial numbers and hardware UUIDs — to anything `api.access.allow` admits, in the clear, with nothing checking who is asking. | Only behind a trusted local proxy that **both** terminates TLS and authenticates callers before ever reaching this agent, on a network segment nothing else can reach. **Never on a routable interface.** |

Disabling TLS, auth, or both is a deliberate operator choice the agent accepts and logs loudly at startup — it is not silently downgraded to, and it is
never the shipped default (see [`packaging/config.yaml`](../packaging/config.yaml), which ships with TLS on and token authentication on). Choosing one
of the three non-default rows means writing the corresponding `enabled: false` explicitly and accepting the tradeoff in its "what it means" column, not
discovering it later from a log line — and, for the `api.auth.enabled` column specifically, means it can never be reached by mere omission.

### The mTLS subject-pinning foot-gun

`api.auth.mtls.allowed_subjects` accepts two different pin shapes, matched differently, and mixing them up silently narrows or widens access in a way
that will not show up as an error:

- **A pin containing `=`** is DN-shaped and is matched against the certificate's Subject distinguished name as a whole, or — when it names a single CN
  and nothing else (`CN=<value>`) — against the Subject common name specifically. **A Subject Alternative Name never satisfies a DN-shaped pin, even for
  the same certificate.** Example: `allowed_subjects: ["CN=inventory-client.example.internal"]` matches only a certificate whose Subject CN is exactly
  that string; a certificate whose SAN lists that same DNS name but has a different CN is refused.
  - **A full-DN pin** — one with further attributes, such as `CN=inventory-client,O=Trusted` — must match the whole Subject DN as rendered in RFC 4514
    form, and never falls back to the common name. A certificate whose CN is literally `inventory-client,O=Trusted` is refused. To pin a CN that itself
    contains `,`, `+` or `;`, escape the character as RFC 4514 does (`CN=inventory-client\,O=Trusted` or `CN=inventory-client\2CO=Trusted`). A
    malformed escape — a backslash before anything other than an RFC 4514 special character or two hex digits — never falls back to the common name.
- **A bare pin with no `=`** matches the Subject common name (exact match) or any DNS Subject Alternative Name (case-insensitive). Example:
  `allowed_subjects: ["inventory-client.example.internal"]` matches a certificate whose CN *or* whose SAN list contains that name.

If your client certificates are issued with the identity in a SAN rather than the CN — increasingly the norm, since most TLS stacks now ignore CN for
hostname verification — a DN-shaped pin (anything with `=` in it) will silently refuse every one of them. Use the bare form when you want SAN matching;
reach for the DN/CN form only when you deliberately want to exclude SAN-only certificates.

## Secrets

TLS private keys and token digest files are referenced in configuration as `credential:<name>`, resolved at startup from `$CREDENTIALS_DIRECTORY`, which
the packaged `systemd` unit populates via `LoadCredential=`. This keeps secret material out of the configuration file itself and off disk in a form the
agent's own process account can read outside of that mechanism. **Distribution and rotation of the underlying secret material are deliberately out of
this agent's scope** — that is left to whatever provisioning system places files in `/etc/hwi-api/tls`, the directory the unit's `LoadCredential=`
loads (Ansible or similar). The agent only ever reads what it is handed.

Only SHA-256 digests of bearer tokens are ever configured; the agent has no code path that stores or logs a plaintext token. A malformed digest entry is
rejected without being echoed, specifically to guard against an operator pasting a live token where a digest belongs.

## Why the daemon holds no capabilities

The packaged unit runs the daemon as an unprivileged `User=hwi-api`/`Group=hwi-api` with **no Linux capabilities at all**: `AmbientCapabilities=`
and `CapabilityBoundingSet=` are both empty, plus `NoNewPrivileges=yes`.

Some of what the agent reports needs more than that. Four DMI identity fields are mode `0400`, root-only, at the kernel level:
`/sys/class/dmi/id/product_uuid`, `product_serial`, `board_serial` and `chassis_serial`. So is the raw SMBIOS table, `/sys/firmware/dmi/tables/DMI`,
which the agent decodes for the memory modules and expansion slots, and so is each PCI device's Vital Product Data, `vpd`, which holds a card's part
and serial numbers (a few drivers make it `0644`). Unreadable, the identity fields would come back as `"unknown"`, the sentinel the agent reports for a
failed read: a silent loss of exactly the fields most useful for hardware correlation.

Granting the daemon `CAP_DAC_READ_SEARCH` would not be scoped to those files: that capability bypasses read and directory-search permission checks on
**every** file the process can see, so a compromise of this network-facing daemon could read `/etc/shadow`, SSH host keys and other services' secrets.
Copying the root-only files as root before the daemon starts would not do either: it cannot serve data that is not one fixed file, such as a `vpd` per
device, and goes stale when a card is hot-swapped.

Instead, a separate **privileged helper** reads them on the daemon's behalf:

- `hwi-api-helper.socket` listens on `/run/hwi-api-helper.sock`, `root:hwi-api` mode `0660`, directly under root-owned `/run`: only root and
  the daemon's group can connect, and only root can replace the socket. The agent unit wants and orders after it, so starting the agent starts the
  socket. Removing the package stops it.
- For each connection, systemd starts one `hwi-api-helper@.service` instance (`Accept=yes`) running the same binary as `hwi-api helper`, so
  nothing privileged runs between collections. `RuntimeMaxSec=60` stops an instance still running a minute later, and `MaxConnections=4` bounds
  how many run at once.
- The helper answers fixed operations only: `dmi`, naming one of the five files above, and `vpd`, naming a PCI address, which must have the form
  `domain:bus:device.function`. It accepts no paths, commands or patterns, caps every answer (4 KiB for an identity field, 1 MiB for the SMBIOS
  table, 32 KiB for VPD, the format's own limit) and refuses anything else. It answers the `hwi-api` user only, checked with `SO_PEERCRED`, on
  top of the socket's mode.
- A read that hangs, as a VPD read on faulty hardware can, is answered as a timeout after 5 seconds, and the instance then exits: a stuck read costs
  one answer, not the helper.
- The instance runs as its own unprivileged user, `hwi-api-helper`, with `CAP_DAC_READ_SEARCH` as its only capability. Because that capability
  reads any file the process can see, the instance sees almost none: `TemporaryFileSystem=/:ro` gives it an empty, read-only root, into which only
  `/sys`, its own binary and `/etc/passwd` (to look up the `hwi-api` user) are bound, read-only. `/sys` is bound without its submounts, so debugfs,
  securityfs, efivarfs and the like stay out of reach; the one exception is the read-only cgroup tree at `/sys/fs/cgroup`, which systemd needs. systemd also gives it a `/proc` restricted to
  its own process (`ProtectProc=invisible`, `ProcSubset=pid`), a private minimal `/dev` (`PrivateDevices=yes`) and an empty `/run`. It has no
  network (`PrivateNetwork=yes`, `RestrictAddressFamilies=AF_UNIX`) and the daemon's write-blocking hardening.
- `Environment=HWI_API_HELPER_SOCKET=/run/hwi-api-helper.sock` tells the daemon where the helper is; without it, the daemon reads only what its
  own user may. A file the host lacks, such as `board_serial` on many VMs, leaves its field out of the payload. A helper that does not answer fails the
  product, baseboard and chassis probes, so the agent keeps serving their last good data with `errors.<subsystem>` rather than replacing a known
  serial number with `"unknown"`; the memory payload still carries its totals, with `modules_error` saying why the modules are
  missing, each PCI device's VPD is reported missing in its `vpd_error`, and the slots in `slots_error`.
  This applies until the helper's first answer for that file: after it, the DMI data is served from the daemon's memory, and only VPD still depends on
  the helper being reachable.

Every operation added later that needs a further capability, such as `CAP_NET_ADMIN` to read a transceiver's EEPROM, must be justified in this
section before it is added. Each connection starts and stops an instance, so the journal shows a `hwi-api-helper@` start and stop for each
collection that reads through it. The daemon asks for each DMI file once per start, since the data is fixed until reboot, and for VPD on each pci
refresh, so the journal shows helper starts mainly at daemon start and, on hosts with VPD-bearing cards, on each pci refresh.

`net.Interfaces` (used by the `links` subsystem and by interface-name binding in `api.bind.interfaces`/`ops.bind.interfaces`) requires an `AF_NETLINK`
socket. The unit's `RestrictAddressFamilies=` therefore includes `AF_NETLINK` alongside `AF_INET`, `AF_INET6` and `AF_UNIX` — omitting it would break
interface enumeration, reported as `errors.links` with no link data, and make interface-name binding fail at startup.

The `network` subsystem uses the same socket family to query the kernel's ethtool generic-netlink interface for link state, FEC and offload features.
Those GET requests need no capability, so the daemon still runs with none, and no `ethtool` binary is executed.

## Hardening kept vs. not set, and what each omitted directive would cost

The unit's guiding rule is: **block everything that costs no data; set nothing that would hide a read this agent needs.** Complete hardware visibility
is the product; a daemon that hardened itself into blindness would not be doing its job. A directive therefore earns its place by blocking a write, or
by hiding something no collector reads, and stays out when it would hide something one does.

Kept, none of which hides `/sys`, `/proc/cpuinfo`, `/proc/meminfo`, `/proc/stat` or udev's database from a read, and none of which blocks the netlink
socket the `links` and `network` subsystems need:

- Write-blocking: `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `ProtectKernelTunables=yes`, `ProtectKernelModules=yes`,
  `ProtectKernelLogs=yes`, `ProtectControlGroups=yes`, `ProtectClock=yes`, `ProtectHostname=yes`, `RestrictSUIDSGID=yes`, `RestrictRealtime=yes`,
  `RestrictNamespaces=yes`, `LockPersonality=yes`, `MemoryDenyWriteExecute=yes`, `SystemCallArchitectures=native`, `NoNewPrivileges=yes`.
- `PrivateDevices=yes`: a minimal private `/dev`. No collector opens anything under `/dev`; block reads `/sys/block`, udev's database and the mount
  table. It also closes the device cgroup and drops the `@raw-io` system calls.
- `PrivateIPC=yes` and `RemoveIPC=yes`: the daemon uses no System V IPC or POSIX message queues.
- `UMask=0077`: the daemon creates no files; any it ever does are private.
- `SystemCallFilter=@system-service`, minus `@privileged` and `@resources`, with `SystemCallErrorNumber=EPERM`. `EPERM` applies to every call the
  filter refuses, and is a deliberate choice over systemd's default of killing the process: a blocked call returns an error the code can handle,
  instead of killing the daemon in the middle of a refresh. The Go runtime raises `RLIMIT_NOFILE` at start through `prlimit64`, which systemd
  puts in `@default`, not `@resources` (`setrlimit` is in `@resources`), so the filter allows it; the smoke test logs both. Nothing in either set
  is called.
- `NoExecPaths=/` with `ExecPaths=/usr/bin/hwi-api`: the daemon executes no program but itself.

The unit needs systemd 248 or later, for `PrivateIPC=`, `NoExecPaths=` and `ExecPaths=`. An older systemd ignores each of them with an "Unknown key"
warning in the journal and starts the daemon without that protection.

The smoke test (`packaging/smoke_test.sh`) shows, in CI containers on Ubuntu 24.04 and Rocky 9, that the second group costs no data. It checks each of
them in effect on the running daemon, from outside the process, then restarts the daemon under a runtime drop-in that turns them all off, and again
under the shipped unit, and fails if any subsystem's payload differs between the two. A container is not a real host: comparing the inventory with and
without them on a real host is the operator's check. Exposure, from `systemd-analyze security`: 2.0 before, 1.5 after, on both systemd 255
(Ubuntu 24.04) and 252 (Rocky 9).

The smoke test also starts the shipped unit with user namespaces disabled (`user.max_user_namespaces=0`) and requires a complete inventory, because
hardened hosts disable them and no directive in the unit may need one.

Deliberately **not** set, each named in the unit's own comments, with what it would cost:

| Directive omitted | What it would cost |
| --- | --- |
| `ProcSubset=pid` | Restricts the visible `/proc` to per-process directories, hiding `/proc/cpuinfo`, `/proc/meminfo` and `/proc/stat`, which the cpu, memory, topology and os subsystems read. |
| `ProtectProc=invisible` | Hides PID 1's `/proc/1/mountinfo`, the host's mount table, from which block reads each device's mount points. The agent would fall back to its own table, which differs from the host's: `ProtectHome=` unmounts a separate `/home` in the agent's namespace, and `ExecPaths=` adds a self-bind of `/usr/bin/hwi-api`, which would show up as a mount point of the root device. |
| `PrivateUsers=yes` | Needs user namespaces. Where a host disables them (`user.max_user_namespaces=0`, as the RHEL 8 STIG and some CIS profiles set), systemd refuses to start the unit: CI showed "Failed to set up user namespacing: No space left on device", `status=217/USER`, on both systemd 252 and 255. The gain is small for a daemon with no capabilities whose inputs are world- or group-readable. |
| `RestrictFileSystems=` | An allow list would have to name every file system an operator may use for `/`, `/etc` and `/usr` (xfs, ext4, btrfs, zfs, an NFS root), and would break only on hosts with the BPF LSM enabled, which CI cannot reproduce. |
| `PrivateNetwork=yes`, `IPAddressDeny=` | The daemon serves its API on the network. |
| `DynamicUser=yes` | The package creates a fixed `hwi-api` user, which the helper socket's group and the configuration file's ownership rely on. |

Two things a stricter, read-blind unit would omit are present specifically to preserve hardware visibility: the privileged helper (the
mode-`0400` DMI fields above), and `AF_NETLINK` inside `RestrictAddressFamilies=` (interface discovery, as described above).

The `pci` subsystem names devices from the PCI ID database (`pci.ids`) bundled into the agent's binary: the agent reads no database file on
the host, fetches nothing over the network, and no environment variable affects it. The packages therefore depend on neither `pci.ids` (deb) nor
`hwdata` (rpm).

## Why the package does not start or enable the service on install

The shipped `/etc/hwi-api/config.yaml` leaves `api.tls.enabled` and `api.auth.enabled` at their secure defaults (on, the second of them also
enabling `api.auth.token.enabled`) but has no TLS certificate/key pair and no token digest configured, so `config.Load` still refuses to start the
agent until an operator supplies both — disabling TLS or authentication now only warns, but an *enabled*, *unconfigured* mechanism is still a hard
error, which is exactly the shipped state. Separately, `api.access.allow` defaults to empty, so even a fully configured agent admits no client until
that is populated too. If the package's install scripts started or enabled the unit automatically, the very first thing every fresh install would do
is either fail to start or start while refusing every request — presenting a deliberate, safe default as a broken package. Leaving the unit inactive
and disabled until an operator explicitly runs `systemctl enable --now hwi-api` (once configured, per the [Quickstart](../README.md#quickstart))
turns that into an explicit, visible step instead of a confusing failure. The helper's socket is not enabled either: the agent unit's `Wants=` starts
it with the agent. An upgrade, in contrast, preserves a running service in place — it is only a fresh install that starts inactive — and
`/etc/hwi-api` survives removal of the package entirely.

## What is deliberately not defended

Say this plainly rather than implying comprehensive protection: `hwi-api` defends against an unauthorized network peer reading hardware inventory. It
does **not** defend against:

- **A fully compromised host.** Anyone with root, or with `CAP_DAC_READ_SEARCH`-equivalent access, on the machine the agent runs on can read everything
  the agent can read directly from `/sys` and `/proc` — the agent adds no confidentiality on top of the OS's own file permissions for a local
  attacker; it only adds authentication and access control for a *remote* one.
- **Traffic analysis or endpoint compromise of a legitimate consumer.** A client holding a valid bearer token or client certificate is, by design, able
  to read everything the API exposes; protecting that credential once issued is the consumer's responsibility, not the agent's.
- **Denial of service beyond a single listener's own concurrency cap.** `api.max_concurrent_requests` bounds in-flight work on the API listener, and a
  fixed cap of 16 bounds the ops listener apart from `/healthz`; there is no broader rate limiting, and the ops listener's limits are fixed rather than tunable for a specific
  deployment's traffic pattern.
- **An untrusted network.** As stated in the threat model above, this agent assumes the network reaching its listeners is itself managed. It is not
  designed to be safe to expose directly to the general internet even with authentication enabled.
- **Confidentiality of data at rest.** The agent holds its inventory in memory only and writes nothing to disk itself, but it also applies no additional
  encryption beyond what the host's own filesystem and TLS in transit provide; disk encryption, if desired, is an operating-system concern.
