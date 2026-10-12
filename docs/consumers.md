# Writing a client against hwi-api

This guide is for anyone writing a client against `hwi-api`'s API, in any language. The authoritative schema is
[`docs/api/openapi.yaml`](api/openapi.yaml); this document explains the parts of it that are easy to misread, not a second copy of it. When this
document and the OpenAPI file ever disagree, the OpenAPI file wins.

## The envelope

Every successful response — both `GET /api/v1/inventory` and `GET /api/v1/inventory/{subsystem}` — is wrapped in the same JSON envelope:

```jsonc
{
  "schema_version": "v1",
  "subsystem": "cpu",
  "host_id": "b41d2f0ce7a94a1cf3d5e0a19b7c2f41",
  "collected_at": "2026-09-21T10:00:00Z",
  "complete": true,
  "data": { "processors": [{ "id": 0, "vendor": "GenuineIntel" }], "total_cores": 8 }
}
```

- `schema_version` is the envelope format ("v1"), not the agent's release version, which the ops listener's `/status` reports.
- `subsystem` echoes the requested subsystem, or `"all"` for a full-snapshot response. `"all"` never appears as a value you can request.
- `host_id` identifies the machine; see [Correlating a response to a host](#correlating-a-response-to-a-host). It is always present, and `""` when the
  agent could not read a machine ID.
- `collected_at` is always UTC. For a single subsystem it is that subsystem's own collection time: the moment its most recent successful probe
  finished, which a failed refresh never moves. For a snapshot it is the **oldest** collection time among the subsystems present, so reading only this
  field on a snapshot tells you the worst-case staleness, not the newest.
- `data` on a snapshot response is an object keyed by subsystem name, each value a `{collected_at, data}` pair, because subsystems refresh on independent
  schedules. `data` on a single-subsystem response is that subsystem's own payload directly.
- The agent builds every payload itself, from sysfs, procfs, udev's database, netlink and the firmware's tables. Their shapes are described in this
  guide, including the order of each array, which can carry meaning (`topology.nodes[].distances[i]` is the distance to `topology.nodes[i]`).

### Values the agent could not read

The payloads mark a value the agent could not read with `"unknown"` for a string and the zero value otherwise; `os`, for example, does so for every
one of its fields:

- A string whose source could not be read is `"unknown"`.
- A string whose source was read but leaves it unset is `""`. For example, an `os-release` file with no `VARIANT` key gives `"variant": ""`, but a host
  with no readable `os-release` at all gives `"unknown"` in every `distribution` field. The kernel fields, `architecture` and `hostname` work the same
  way.
- `boot_time` is `0001-01-01T00:00:00Z`, the zero timestamp, when `/proc/stat` cannot be read.

`network` is the exception: it leaves a value the kernel does not know, such as a NIC's `duplex`, `port`, `speed_mbps` or `fec.active`, out of the
payload rather than reporting it as `"unknown"`. Only an active FEC mode the agent cannot name is `"unknown"`. The DMI subsystems likewise leave out a
field the host has no file for, or whose file is empty, keeping `"unknown"` for one they could not read, `block` does the same for a field whose
source the device does not have, and `cpu` for a field `/proc/cpuinfo` does not report.

Every `os` key is always present. That is not true of every payload: some fields are optional and omitted when empty, such as a `links` entry's
`mac_address` and `addresses`, and an empty list may be `null`, such as `virtualization.evidence` when there is no evidence, so treat any field not
described as required here as possibly absent or `null`. The fields most likely to be unreadable, or unreliable when read, are the DMI identity
fields; their `identifiers` supplement, described below, assesses them for you.

### `network` and `links`

Two subsystems describe the host's networking, and a consumer joins them on the interface `name` or `index`:

- **`network`** is the network hardware: each physical or virtual NIC (never `lo`), with its name, kernel interface index, MAC address, PCI address, link modes, FEC setting and
  offload features. The agent reads it from sysfs and from the kernel's ethtool netlink interface, so the host needs no `ethtool` binary. Hardware
  changes rarely, so it refreshes every 5 minutes.
- **`links`** is every interface the kernel reports, `lo` and virtual interfaces included, with its index, MTU, administrative and operational
  state, MAC address (`mac_address`) and assigned addresses. Addresses and link states change often, so it refreshes every minute.

Each link carries two states, because an interface can be enabled and still not pass traffic:

- **`admin_state`** is `"up"` when the interface is administratively enabled (`ip link set … up`), else `"down"`.
- **`oper_state`** is the kernel's operational state (RFC 2863), exactly as `/sys/class/net/<name>/operstate` reports it: `"up"`, `"down"`,
  `"lowerlayerdown"`, `"dormant"`, `"testing"`, `"notpresent"` or `"unknown"`. An interface that has lost its carrier, such as a pulled cable or a
  switch port gone down, reads `admin_state: "up"` with `oper_state: "down"`. `"unknown"` is the kernel's own value, typical of `lo` and of virtual or
  tunnel interfaces with no carrier to sense; it never means the agent could not read the state.

The `links` list is complete whenever `errors.links` is absent. If listing the interfaces, or reading any one interface's addresses or operational
state, fails, the agent
keeps serving the last list it collected successfully, with `errors.links` naming the failure and `collected_at` showing how old the list is; if it
has never succeeded, `links` is not served at all. It never reports an empty list in place of a failed enumeration, so an empty or shortened list
really does mean interfaces are gone. A `links` failure does not affect `network`, and the reverse.

The `network` payload is `{"nics": [...], "ethtool_error": "...", "permanent_mac_error": "..."}`, with `nics` sorted by name. Each NIC has:

- **`name`** and **`mac_address`**: the interface's name and its current address, as `/sys/class/net/<name>` reports them.
- **`permanent_mac_address`**: the interface's permanent (burned-in) address, formatted like `mac_address`, such as `"3c:fd:fe:a1:b2:c3"`.
  The agent reads it from the kernel's `IFLA_PERM_ADDRESS` in one rtnetlink link dump per refresh (Linux 5.6 or later). For an interface the dump
  has none for, or when the dump fails, it falls back to `/sys/class/net/<name>/bonding_slave/perm_hwaddr`, which the bonding driver keeps for
  each bond member: the member's address when it was enslaved. That is the burned-in address unless the address was changed before enslaving,
  as by a cloned MAC or a systemd `.link` file's `MACAddress=`. It is absent when neither source has one, or only zeros, as for a bridge, bond,
  VLAN or veth. A few virtual drivers, such as ipvlan and macsec, report their parent's address as their own permanent one.
- **`virtual`**: `true` when the interface's sysfs directory is under `/devices/virtual/`: a bridge, bond, VLAN, tunnel and the like.
- **`pci_address`**: the PCI function the interface belongs to, such as `0000:3b:00.0`, or, for a virtio-net interface, the PCI device its virtio
  device sits on. It is absent for a virtual interface and for any other parent, such as a USB NIC.
- **`link`**: what `ethtool <if>` prints. `port` is the connector, such as `"TP"`, `"FIBRE"` or `"DA"`; `autonegotiation`, `speed_mbps`, `duplex`
  (`"full"` or `"half"`) and `lanes` are the link's settings. Each is absent when the kernel does not know it, such as the speed of a link that is
  down. `supported_modes`, `advertised_modes` and `partner_modes` list link-mode names, such as `25000baseCR/Full`, in the kernel's order: the modes
  the NIC supports, the ones it advertises, and the ones the link partner advertises. They hold only link modes, never the `TP`, `Autoneg`, `Pause`
  or FEC bits `ethtool` prints beside them, and are `[]`, never `null`, when `link` is present.
- **`fec`**: what `ethtool --show-fec <if>` prints. `configured` lists the configured FEC modes, such as `["RS", "BASER"]`, and is `[]`, never `null`;
  `active` is the mode in use, `"unknown"` for a mode the agent cannot name, and absent when the kernel does not report one; `auto` is whether the
  FEC mode is chosen automatically, absent when the kernel does not say.
- **`features`**: what `ethtool -k <if>` prints, keyed by the kernel's feature names, which are the names `ethtool -K <if> <name> on|off` takes, such
  as `rx-checksum` or `rx-gro-hw`. Each has `active` (on now), `requested` (wanted on) and `fixed` (cannot be changed: `ethtool -k`'s `[fixed]`), so
  a feature that can be changed has `fixed: false`. A feature missing from the map is off and cannot be changed on that device: the kernel lists
  only the features that are on, wanted on or changeable, plus the few it never lets any device change, such as `vlan-challenged`.
- **`ethtool_error`**: the first of the NIC's ethtool queries to fail, naming the query, such as `"features: input/output error"`; absent when none
  failed.

`mac_address` is the address the interface uses now, and that is not always its own: a bond member usually carries its bond's address, so all
the members of a bond, and the bond itself, can report the same `mac_address`. A consumer that records a physical NIC's (`virtual: false`)
hardware identity should take the address it records for the interface from `permanent_mac_address` when present, and from `mac_address` otherwise. For a
virtual interface it should use `mac_address`: a `permanent_mac_address` there can be its parent's.

The top-level `permanent_mac_error` says why the link dump failed, starting with `permanent addresses:`; absent when it succeeded. A dump the
kernel flags as interrupted, because interfaces were added or removed while it ran, may have left some out: it is asked for again, up to three
times, and reported as `permanent addresses: the dump was interrupted by link changes` if every attempt was. The NICs then
still carry the `bonding_slave/perm_hwaddr` fallback, so bond members keep their own addresses, and every other NIC has no
`permanent_mac_address`. The dump gets 5 seconds. One that does not return is reported as `permanent addresses: no reply: i/o timeout`, and,
as with ethtool, nothing can cancel it: until it returns, each refresh reports
`permanent addresses: a previous request is still blocked in the kernel` rather than starting another.

`ethtool -k` also prints legacy group names, which are not kernel feature names and so are never keys of `features`. Each stands for the kernel
features its pattern matches. When it matches more than one, `ethtool -k` lists them indented below it; when it matches one, such as
`generic-receive-offload`, that feature is printed only under the group name, so `rx-gro` appears in `features` but not in `ethtool -k`:

| `ethtool -k` group name        | Kernel features in `features`                                                                                                            |
| ------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `rx-checksumming`              | `rx-checksum`                                                                                                                            |
| `tx-checksumming`              | `tx-checksum-*`, such as `tx-checksum-ipv4`, `tx-checksum-ip-generic`, `tx-checksum-ipv6`, `tx-checksum-sctp`                            |
| `scatter-gather`               | `tx-scatter-gather*`: `tx-scatter-gather`, `tx-scatter-gather-fraglist`                                                                  |
| `tcp-segmentation-offload`     | `tx-tcp*-segmentation`, such as `tx-tcp-segmentation`, `tx-tcp-ecn-segmentation`, `tx-tcp-mangleid-segmentation`, `tx-tcp6-segmentation` |
| `udp-fragmentation-offload`    | `tx-udp-fragmentation`                                                                                                                   |
| `generic-segmentation-offload` | `tx-generic-segmentation`                                                                                                                |
| `generic-receive-offload`      | `rx-gro`                                                                                                                                 |
| `large-receive-offload`        | `rx-lro`                                                                                                                                 |
| `rx-vlan-offload`              | `rx-vlan-hw-parse`                                                                                                                       |
| `tx-vlan-offload`              | `tx-vlan-hw-insert`                                                                                                                      |
| `ntuple-filters`               | `rx-ntuple-filter`                                                                                                                       |
| `receive-hashing`              | `rx-hashing`                                                                                                                             |

A block whose query the driver does not support is absent, with no error. Any other failed query leaves its block absent and sets that NIC's
`ethtool_error`. When the kernel has no ethtool netlink family (it needs Linux 5.6 or later, built with `CONFIG_ETHTOOL_NETLINK`) or the socket
cannot be opened, the top-level `ethtool_error` says why, and no NIC has `link`, `fec` or `features`. Each query, its request to the kernel
included, gets 5 seconds (at most 5.1). If one times out, because the kernel or the driver stops answering, the agent asks nothing more: the
top-level `ethtool_error` starts with `ethtool queries stopped:`, and the NICs not yet asked about have only their sysfs fields. A request stuck
in the kernel cannot be cancelled, so until it returns the agent does not ask again: each refresh reports the top-level `ethtool_error`
`another ethtool session is open, or a previous request is still blocked in the kernel`, with no NIC's ethtool blocks. An interface removed
while the agent reads it is left out. An interface whose device is detached, such as during a driver reset, after a fatal firmware error or
while suspended, keeps its entry, with `ethtool_error` such as
`"link modes: no such device"` and whichever blocks the kernel still answered. The probe fails, and the agent keeps serving the last payload it
collected, only when `/sys/class/net` cannot be listed.

### The DMI subsystems: `product`, `baseboard`, `chassis` and `bios`

The agent builds these four payloads itself, from the strings the firmware publishes in `/sys/class/dmi/id`. Each field is the trimmed content of one file:

| Subsystem | Fields (file) |
| --- | --- |
| `product` | `family` (`product_family`), `name` (`product_name`), `vendor` (`sys_vendor`), `serial_number` (`product_serial`), `uuid` (`product_uuid`), `sku` (`product_sku`), `version` (`product_version`), `identifiers` |
| `baseboard` | `asset_tag` (`board_asset_tag`), `serial_number` (`board_serial`), `vendor` (`board_vendor`), `version` (`board_version`), `product` (`board_name`), `identifiers` |
| `chassis` | `asset_tag` (`chassis_asset_tag`), `serial_number` (`chassis_serial`), `type` (`chassis_type`), `type_description`, `vendor` (`chassis_vendor`), `version` (`chassis_version`), `identifiers` |
| `bios` | `vendor` (`bios_vendor`), `version` (`bios_version`), `date` (`bios_date`) |

- A field whose file the host does not have, as is common on VMs, or whose file is empty, is omitted.
- A field whose file exists but cannot be read is `"unknown"`.
- `chassis.type` is the SMBIOS chassis type code in decimal, such as `"23"`, and `type_description` its name, such as `"Rack mount chassis"`;
  `type_description` is `"unknown"` for a code outside the SMBIOS table's 1 to 36, or when `type` itself is `"unknown"`.

Four of these files are root-only: `product_serial`, `product_uuid`, `board_serial` and `chassis_serial`. The agent runs with no capabilities, so its
privileged helper reads them for it (see `docs/security.md`), once per agent start, like the SMBIOS table. A file the helper reports the host does not
have is omitted. Without the helper configured, the agent reads these files itself, as it does the others: an agent running without the capability to
read them reports them as `"unknown"`. When the helper does not answer, or cannot read the file, the `product`, `baseboard` or `chassis` probe fails
rather than report `"unknown"` in place of a serial number it knew: the agent keeps serving the last payload it collected, with `errors.<subsystem>`
saying why. `bios` has no root-only field, so the helper never affects it.

`product`, `baseboard` and `chassis` carry an `identifiers` object, last, assessing `serial_number` and, on `product`, `uuid`, each when that field is
in the payload; it is omitted when there is nothing to assess. See [Reading the `identifiers` supplement](#reading-the-identifiers-supplement-not-the-raw-field).

### `cpu`

The agent builds the `cpu` payload itself, from the logical CPUs in `/sys/devices/system/cpu` and their descriptions in `/proc/cpuinfo`. It lists the
host's processor packages (sockets), each package's cores and each core's logical CPUs (hardware threads), in this order:

```jsonc
{
  "total_cores": 64,
  "total_hardware_threads": 128,
  "processors": [
    {
      "id": 0, "vendor": "GenuineIntel", "model": "Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz", "microcode": "0xd0003e7",
      "capabilities": ["fpu", "vme", "…"], "total_cores": 32, "total_hardware_threads": 64,
      "cores": [{ "id": 0, "total_hardware_threads": 2, "logical_processors": [0, 64] }]
    }
    // the second processor, id 1, is omitted here; the totals above count both
  ],
  "offline_logical_processors": [5]
}
```

- **`total_cores`** and **`total_hardware_threads`**, always present, are the sums over `processors`.
- **`processors`**, always present and sorted by `id`, has one entry per package. Its **`id`** is the kernel's `physical_package_id`.
- **`vendor`**, **`model`**, **`microcode`** and **`capabilities`** are `/proc/cpuinfo`'s `vendor_id`, `model name`, `microcode` (the loaded microcode
  revision, which vulnerability tracking needs) and `flags`, the last split into a list in the kernel's order. They come from the package's own
  lowest-numbered online CPU that has a block in cpuinfo, never from another package's CPU. Each is absent when cpuinfo does not report it, as arm64 does not report `microcode`.
- **`total_cores`** and **`total_hardware_threads`**, always present, count the package's cores and its online logical CPUs.
- **`cores`**, always present, sorted by `id` and then by the lowest logical CPU. A core's **`id`** is the kernel's `core_id`;
  **`logical_processors`**, sorted, are its online CPUs, and **`total_hardware_threads`** is their count.
- **`offline_logical_processors`**, sorted, lists the CPUs that are present but not online, which every count above leaves out. It is absent when
  there are none, or when `/sys/devices/system/cpu/present` or `online` is missing or cannot be read: it is a hint, not a count. A VM that declares
  room for vCPUs it may hot-add does not list them here, because they are absent, not offline. SMT disabled by the kernel (`nosmt`, or
  `/sys/devices/system/cpu/smt/control` set to off, a common L1TF/MDS mitigation) lists the sibling threads here, as lscpu's "Off-line CPU(s) list"
  does; SMT disabled in firmware does not, because those threads are never present.

A core is the set of logical CPUs the kernel names as siblings (`topology/thread_siblings_list`, or `core_cpus_list` on a kernel without it), not
every CPU with the same `core_id`: `core_id` is unique only within a die, so on a multi-die package, such as Cascade Lake-AP and some EPYC
configurations, two cores can share an `id`. Read them as two cores, not one. A CPU with neither list, or whose list cannot be read or is malformed,
is a core of its own. A CPU whose `physical_package_id` or `core_id` cannot be read gets `-1` for it, so it is never merged into a real package or core.

A CPU whose `online` file reads `0` is offline and left out; a CPU without an `online` file, as `cpu0` is on most x86 hosts, or whose `online` file
cannot be read, counts as online. A CPU whose `online` file reads `1` but whose `topology/physical_package_id` is gone by the time it is read, as when
it goes offline in the middle of a probe, is treated as offline: it is left out of the counts and, when the kernel's lists agree, shows in
`offline_logical_processors` on the next refresh. `total_hardware_threads` is therefore the number of online CPUs, what `getconf _NPROCESSORS_ONLN` reports.

When `/proc/cpuinfo` is missing, no package has `vendor`, `model`, `microcode` or `capabilities`; when it exists but cannot be read, `vendor` and
`model` are `"unknown"` and the other two absent. A package takes the four from the lowest of its online CPUs that has a block in cpuinfo, and has none of them when none of its CPUs has one.

On arm64, supported best effort, cpuinfo names neither vendor nor model. `vendor` is then named from `CPU implementer`, such as `"ARM"` for `0x41`
or `"Ampere"` for `0xc0`, or is the raw code for one the agent does not know. `model` is named from `CPU implementer` and `CPU part` together, such as
`"Neoverse-V2"`, or is `"<implementer>:<part>"`, such as `"0x41:0xd0b"`, for a pair it does not know; it is absent when either is missing.
`capabilities` are cpuinfo's `Features`, and there is no `microcode`.

The refresh fails, and the agent keeps serving the last payload it collected with `errors.cpu` naming the failure, only when
`/sys/devices/system/cpu` is missing or cannot be listed, or lists no online CPU: an empty `cpu` would read as a host without processors.

### `memory`

The agent builds the `memory` payload itself. The fields it shares with each `topology.nodes[].memory` have the same names and meanings there, where
they are counted for one node:

- **`total_usable_bytes`** is `MemTotal` from `/proc/meminfo`: what the kernel can allocate.
- **`total_physical_bytes`** is the memory the kernel has online, counted from its memory blocks, or `total_usable_bytes` where the kernel does not
  expose them. It leaves out ranges the firmware reserves, so on real hardware it is usually less than the installed capacity.
- **`installed_bytes`** is the sum of the installed modules' sizes: the capacity actually fitted. It is `0` when the modules could not be read.
- **`supported_page_sizes`**, **`default_huge_page_size`**, **`total_huge_page_bytes`** and **`huge_page_amounts_by_size`** describe huge pages.
- **`modules`** lists every memory slot the firmware describes, in the firmware's order, with `installed` telling a fitted module from an empty slot.
  An empty slot carries only what describes the slot: its `label` (the bank, such as `"P0 CHANNEL A"`), `location` (the slot, such as `"DIMM_A1"`)
  and `form_factor`. An installed module adds
  `size_bytes`, `type` (such as `"DDR5"`), `speed_mts` and `configured_speed_mts` (in MT/s), `rank`, `vendor`, `part_number`,
  `serial_number`, `total_width_bits` and `data_width_bits`. A module with ECC has a total width greater than its data width, such as 72 against 64.
  An installed module whose size the firmware does not know has `size_bytes: 0`.

The modules come from the host's SMBIOS table, which only root can read. The agent reads it through the privileged helper, as it does the DMI identity
fields. Both are read once per agent start: the kernel copies them at boot, so they cannot change before a reboot. When the table is missing (not
readable through the helper, or absent, as on some VMs) or cannot be decoded, the payload still carries every other field, with `modules: null`,
`installed_bytes: 0`, and `modules_error` saying why. `modules_error` is omitted when the modules were read. A missing table is not a probe failure,
so it never sets `errors.memory`.

### `topology`

The agent builds the `topology` payload itself, from the NUMA nodes in `/sys/devices/system/node`, the logical CPUs and their caches in
`/sys/devices/system/cpu`, and each node's memory. It lists the host's nodes, each with its cores, caches, distances and memory, in this order:

```jsonc
{
  "architecture": "numa",
  "nodes": [
    {
      "id": 0,
      "cores": [{ "id": 0, "total_hardware_threads": 2, "logical_processors": [0, 64] }],
      "caches": [{ "level": 1, "type": "data", "size_bytes": 49152, "logical_processors": [0, 64] }],
      "distances": [10, 21],
      "memory": {
        "total_physical_bytes": 274877906944,
        "total_usable_bytes": 270582939648,
        "supported_page_sizes": [2097152, 1073741824],
        "total_huge_page_bytes": 0,
        "huge_page_amounts_by_size": { "2097152": { "total": 0, "free": 0, "surplus": 0 } }
      }
    }
    // the second node, id 1, is omitted here
  ]
}
```

- **`architecture`**, always present, is `"smp"` for a host of one node and `"numa"` for more.
- **`nodes`**, always present and never empty, has one entry per node directory, `node<N>`, sorted by its **`id`**, `N`, numerically: `node10`
  follows `node9`.
- **`cores`**, always present, are the `cpu` payload's cores restricted to the node's online CPUs: the same grouping by sibling set, the same `id`s
  and the same order: by package, then by `id` and then by the lowest logical CPU within it. A node that holds more than one package, as a
  multi-socket VM with one NUMA node does, lists one package's cores and then the next's. The node's CPUs are its `cpulist`, or its
  `cpu<N>` entries when the list is missing or cannot be read or parsed. Only the CPUs `cpu` counts are used, so a node's cores never hold an offline CPU.
- **`caches`**, always present, lists each cache the node's online CPUs report in `cache/index<M>` once, however many of them share it, with
  **`level`**, **`type`** (`"data"`, `"instruction"` or `"unified"`), **`size_bytes`** (the index's own `size`) and **`logical_processors`** (the
  node's online CPUs that share it, sorted). A cache is told from another by its level, type and the kernel's `id`, or by its `shared_cpu_list` where
  the kernel gives no `id`. Caches are sorted by `level`, then `type` in the order above, then the lowest logical CPU. A cache shared by two nodes, as
  an L3 is under sub-NUMA clustering, appears in each with that node's CPUs. A CPU without a `cache` directory, as in many VMs, adds none. A `level`
  that cannot be read is `0`, a `type` `"unknown"` and a `size_bytes` `0`.
- **`distances`** are the kernel's distances from this node to each node, `distances[i]` being the distance to `nodes[i]`, not to the node whose `id`
  is `i`: the two differ where node ids have gaps. The local distance is `10`. `distances` is absent when the node's `distance` file is missing or
  cannot be read or parsed, or does not have exactly one entry per node, as when a node comes online or goes away while the agent reads.
- **`memory`**, always present, is the node's memory with the `memory` payload's field names, counted for the node alone:
  **`total_physical_bytes`** (its online memory blocks, or `total_usable_bytes` where the kernel does not expose them), **`total_usable_bytes`** (its
  `MemTotal`, `0` when its `meminfo` cannot be read), **`supported_page_sizes`** (its huge page sizes, ascending), **`total_huge_page_bytes`** and
  **`huge_page_amounts_by_size`**, each size's **`total`**, **`free`** and **`surplus`**. The huge page fields are the node's own.
  `total_huge_page_bytes` is always present, `0` on a kernel without huge pages; `supported_page_sizes` and `huge_page_amounts_by_size` are absent there.

A node without CPUs, such as one of CXL or HBM memory only, is listed with `"cores": []`, `"caches": []` and its memory.

The nodes' `total_usable_bytes` add up to `memory.total_usable_bytes`, and their huge pages to the system's in `memory.huge_page_amounts_by_size`.
Two of `memory`'s huge page fields have no per-node counterpart: `default_huge_page_size`, a system-wide setting, and each size's `reserved`, which
the kernel counts system-wide only.

A memory block that spans two nodes is linked under both, so the nodes' `total_physical_bytes` can add up to more than
`memory.total_physical_bytes`, by a block or so. Only `total_usable_bytes` adds up to `memory`'s.

A host whose kernel has no `/sys/devices/system/node`, as one built without NUMA support, or whose node directory lists no node, is one node: `id`
`0`, every online CPU, `distances` `[10]`, as `numactl -H` shows for a single node, and the system's memory, the same values as `memory`'s.

The refresh fails, and the agent keeps serving the last payload it collected with `errors.topology` naming the failure, only when
`/sys/devices/system/cpu` is missing or cannot be listed, or lists no online CPU, when `cpu`'s refresh fails too, or when
`/sys/devices/system/node` exists but cannot be listed: a payload with one node in place of several would misdescribe the host.

### `pci`

The agent builds the `pci` payload itself, from sysfs, naming each device from the PCI ID database (`pci.ids`) bundled into the agent, never the host's
copy, so names do not depend on how old a host's copy is. The bundled database is refreshed from the [PCI ID Project](https://pci-ids.ucw.cz/) weekly,
and a newer agent release carries a newer one. `devices` lists every PCI function in address order. Each one has:

- **`address`**, `domain:bus:device.function`, and **`parent_address`**, the bridge or port above it, absent for a device directly under a host bridge.
- **`vendor`**, **`product`**, **`class`**, **`subclass`** and **`programming_interface`**, each with the `id` the device reports, in lowercase hex, and
  its `name` from the database. **`subsystem`** identifies the board built around the chip, with its own `vendor_id`, since a card's vendor is often not
  its chip's. A name the database does not list is `"unknown"`.
- **`revision`**; **`driver`**, absent when no driver is bound; **`driver_module`**, the kernel module providing the driver, absent when the kernel links
  the driver to no module, as it does for some drivers built into the kernel; **`iommu_group`**, absent without an IOMMU; and **`numa_node`**, absent when
  the kernel gives the device no NUMA affinity or its value cannot be read, since `0` is a real node.
- **`power_state`**, `"D0"` to `"D3cold"`, on kernels that report it.
- **`slot`**, the physical slot: **`name`**, the kernel's name for it, as `lspci` shows it in "Physical Slot", and **`designation`**, the
  firmware's label, such as `"CPU1 SLOT3 PCI-E 4.0 X16"`, the name to give a technician or an inventory record. Either can be absent; `slot` is
  absent when both are. Every function of a card carries its slot, and so does every VF of one, even at another device number. The kernel's name
  comes from its ACPI slot or PCIe hotplug drivers, so it is absent on most VMs and on hosts where neither describes the slot; a slot it ties to a
  whole bus covers every device on that bus without a slot of its own. The designation comes from the SMBIOS table, matched by the address the
  firmware records for the slot, which is often the port above the card, so the port carries it too. The kernel also knows empty slots, such as
  drive bays; they are on no device, and `slots` lists every slot the firmware describes, empty or not. A missing `designation` can mean the SMBIOS
  table was unavailable, so check `slots.slots_error` before treating it as a card leaving its slot.
- **`label`**, the firmware's name for an onboard device, such as `"Embedded NIC 1"`.
- **`kernel_names`**, what the kernel built on the device, by kind: `net` interfaces, `nvme` controllers, `drm` cards and `infiniband` devices, such as
  `{"net": ["enp59s0f0np0"], "infiniband": ["mlx5_0"]}`, including those of a virtio device in a VM. The `net` names join a device to `links`,
  `network` and `udev-net`. `nvme` lists the controller, such as `nvme0`, not its namespaces, such as `nvme0n1`, and a SATA or SAS controller lists no
  disks, so `kernel_names` does not join a device to `block`: a `block` device's `pci_address` does.
- **`link`**, on a PCI Express device: `current_speed` and `current_width`, what the link trained at, and `max_speed` and `max_width`, the most the device
  supports. Speeds are the kernel's strings, such as `"16.0 GT/s PCIe"`; widths are lane counts. The agent gives no verdict on them. A current width below
  the maximum, such as a x16 card running at x8, reliably means a problem with the card, slot or riser. A current speed below the maximum does not: GPUs
  and some NICs lower their link speed when idle, so compare speeds only on a device under load. A speed of `"Unknown"`, capitalised, is the kernel's
  own value for a speed it cannot tell, unlike `"unknown"`, which means the agent could not read the file. The values mean something only while
  `power_state` is `"D0"` or `"D3hot"`: a device in `"D3cold"` is powered off, and its link can read as garbage, such as a width of 63. A virtual function
  has no `link`: its link is its physical function's.
- **`sriov`**, on a physical function that supports SR-IOV: `total_vfs`, how many virtual functions it supports, `num_vfs`, how many are configured, and
  `vfs`, their addresses by VF index. A virtual function has **`vf_of`**, its physical function's address, instead.
- **`vpd`**, a card's Vital Product Data, from its own EEPROM: what the PCI IDs cannot say, the orderable card and the physical one. `product_name`,
  `part_number`, `serial_number`, `engineering_change` and `manufacturer_id` are the standard keywords, each absent when the card does not set it;
  `vendor_specific` holds every other read-only keyword by name, such as `V0`, often a firmware version or a port's MAC address, and `read_write`
  the writable ones. Server NICs nearly always have VPD, RAID and SAS controllers and NVMe drives sometimes. Every function of a multi-function card
  usually reports the same VPD, so a dual-port NIC shows its part and serial number twice; deduplicate by `serial_number`. The VPD is read on every
  `pci` refresh, so a hot-swapped card is reported correctly from the next one. The file is root-only, so the agent reads it through its privileged
  helper (see `docs/security.md`). Reading VPD wakes a powered-down device and the port above it, so the agent does not read it from a device that
  is runtime-suspended or in `"D3cold"`, such as an idle GPU: it reports the VPD it last read from that card while the card was awake, if it has
  read it since it started and every `pci` refresh since has found a device at that address with the same vendor, device and subsystem IDs. A
  refresh that finds the address empty forgets that VPD, so a replacement of the same model is reported unread until it wakes. PCI IDs name a
  model, not a card, so a card swapped for one of the same model between two refreshes, and powered down before the second, cannot be told apart
  without waking it: it reports the removed card's VPD until it next wakes.
- **`vpd_error`**, instead of `vpd`, on a device whose VPD exists but could not be read or decoded, saying why: the helper is not configured or
  did not answer, the read failed or timed out (some controllers never answer one), or the data is malformed or fails its checksum. A VPD is never
  reported in part. A powered-down device whose VPD the agent has not read while it was awake says
  `not read: the device is powered down, and reading its VPD would wake it`. Once a read times out or the helper does not answer, the agent stops
  asking for the rest of that refresh, so one faulty card does not hold up every subsystem: the devices after it say
  `not read: an earlier VPD read in this refresh failed (...)`, with that first error, and the next refresh tries again.

A field whose source exists but cannot be read is `"unknown"` or `0`, as in every payload, except `numa_node`, which is left out. If the device list
itself cannot be read, the refresh fails, and the agent keeps serving the last list it collected, with `errors.pci` naming the failure. A device removed
while it is being read is left out of that refresh.

### GPUs

GPUs and other display controllers are the `pci` devices whose `class.id` is `"03"`, whatever their driver: that includes a GPU bound to `vfio-pci` to
be passed through to a virtual machine, and a display adapter with no driver bound at all. A device with a DRM driver has its DRM devices in
`kernel_names.drm`, such as `["card0", "renderD128"]`, which joins it to `udev-drm`.

### `slots`

The agent builds the `slots` payload itself, from the SMBIOS table's System Slots (Type 9) structures, which it reads through its privileged helper.
`slots` lists every expansion slot the firmware describes, empty ones included, in the firmware's order. Each one has:

- **`designation`**, the slot's name as the chassis or board labels it, such as `"PCIe Slot 3"` or `"CPU1 SLOT6 PCI-E 5.0 X16"`. A card in the slot
  carries the same name in `pci` as `slot.designation`.
- **`type`**, such as `"PCI Express 5 x16"`, and **`width`**, the data bus width, such as `"x16"`. A slot's width can be narrower than its type's,
  as for a x16 connector wired for x8.
- **`in_use`**, `true` for a slot holding a card and `false` for an available one; absent when the firmware does not say.
- **`address`**, the PCI address the firmware gives the slot, often the port the slot hangs off rather than a card in it; absent when it gives
  none, as firmware usually does for an empty slot.

Most VMs describe no slots, so `slots` is an empty list there. Without the SMBIOS table, `slots` is `null` and **`slots_error`** says why, as
`modules_error` does in `memory`. The firmware writes the table at boot, so the list does not follow hot-plug: `pci` does.

### `block`

The agent builds the `block` payload itself, from `/sys/block`, udev's database in `/run/udev/data` and the host's mount table. It is `{"devices": [...],
"mounts_error": "..."}`, with `devices` in `/sys/block`'s order, which is by name:

```jsonc
{
  "devices": [
    {
      "name": "sda", "type": "disk", "size_bytes": 480103981056, "logical_block_size_bytes": 512, "physical_block_size_bytes": 4096,
      "drive_type": "ssd", "removable": false, "read_only": false, "transport": "sata", "pci_address": "0000:00:17.0",
      "bus_path": "pci-0000:00:17.0-ata-1.0", "vendor": "ATA", "model": "SAMSUNG MZ7LH480", "serial_number": "EXSSD000000001",
      "wwn": "0x5002538e00000000", "partition_table_type": "gpt",
      "partitions": [
        {
          "name": "sda3", "number": 3, "start_bytes": 2149580800, "size_bytes": 477953261568, "read_only": false,
          "partition_type": "e6d6d379-f507-44c2-a23c-238f2a3df928", "partition_uuid": "…", "filesystem_type": "LVM2_member",
          "filesystem_uuid": "…", "holders": ["dm-0", "dm-1"]
        }
      ]
    },
    {
      "name": "dm-0", "type": "dm", "size_bytes": 107374182400, "logical_block_size_bytes": 512, "physical_block_size_bytes": 4096,
      "removable": false, "read_only": false, "dm_name": "vg0-root", "dm_uuid": "LVM-…", "filesystem_type": "xfs", "filesystem_uuid": "…",
      "mount_points": ["/"], "slaves": ["sda3"], "partitions": []
    }
  ]
}
```

`devices` lists every entry of `/sys/block` except an unused loop, nbd, ram or zram device (size 0: no file, no server or not configured; an optical
drive of size 0, one without a disc, is listed) and a device the kernel marks hidden, a path of an NVMe multipath namespace whose head is listed
instead. Device-mapper devices (LVM volumes, dm-crypt, multipath) and MD arrays are listed beside the disks they are built
on, which is why the list is `devices`, not `disks`. Each device has, in this order:

- **`name`**, the kernel's name: `/dev/<name>` is its node and `udev-block`'s `devname`.
- **`type`**: `"dm"` for a device-mapper device, `"md"` for an MD device, `"loop"`, `"rom"` for an optical drive (`sr*`, or a SCSI device of type 5)
  and `"disk"` for anything else.
- **`size_bytes`**, **`logical_block_size_bytes`** and **`physical_block_size_bytes`**, always present, `0` when unreadable. The size is the kernel's
  count of 512-byte sectors times 512, whatever the block size.
- **`drive_type`**, on a disk or an optical drive only: `"optical"` for an optical drive; `"ssd"` for an NVMe namespace, multipath heads and NVMe over
  fabrics included; `"virtual"` for a virtio or Xen disk, or one the kernel builds itself under `/devices/virtual/`, such as zram, rbd or nbd, whose
  rotational flag says nothing about the storage behind it; otherwise `"hdd"` or `"ssd"` from the rotational flag, `"unknown"` when it cannot be read.
- **`removable`** and **`read_only`**, always present, `false` when unreadable.
- **`transport`**, how the device is attached, from its sysfs path as `lsblk`'s `TRAN` reads it: `"nvme"` (or, for NVMe over fabrics, the controller's
  transport, such as `"tcp"`, `"rdma"` or `"fc"`), `"usb"`, `"sata"` (any ATA port, parallel ATA included), `"sas"`, `"iscsi"`, `"fc"`, `"virtio"`
  (a virtio-blk disk and also a virtio-scsi one, for which `lsblk` reports none) or `"mmc"`. It is absent when the path names none, as for a RAID
  controller's logical drive and for anything built in software. A native NVMe multipath head sits under its subsystem, not a controller, so its
  transport is read from the controllers the subsystem links (`nvme-subsysN/nvmeN`): the first, by name, whose transport is not `pcie`, else `"nvme"`.
- **`pci_address`**, the controller the device hangs off, joining `pci`: the NVMe, SATA, SAS or RAID controller, the USB host controller, or the PCI
  device a virtio disk sits on, never a bridge above it. A native NVMe multipath head takes its subsystem's controllers' address when every controller
  with one has the same; a dual-port drive whose controllers are two PCI functions has none. It is absent for a device with no PCI device under it: dm,
  md, loop, NVMe over fabrics, a Xen disk.
- **`bus_path`**, udev's `ID_PATH`, such as `pci-0000:00:17.0-ata-1.0`.
- **`vendor`**, sysfs's `device/vendor`, absent where there is none, as for NVMe. A virtio disk's is the virtio vendor ID, `0x1af4`, as `lsblk` shows
  it.
- **`model`**, sysfs's `device/model`, as the drive reports it; udev's `ID_MODEL`, whose spaces udev replaces with `_`, only when sysfs has no model
  file. An NVMe namespace's `device` is its controller, which has the model.
- **`serial_number`**, udev's `SCSI_IDENT_SERIAL`, `ID_SCSI_SERIAL` or `ID_SERIAL_SHORT`, else sysfs's `device/serial`, the first that is set.
- **`wwn`**, udev's `ID_WWN_WITH_EXTENSION`, `ID_WWN` or `DM_WWN`, else sysfs's `wwid` or `device/wwid`, the first that is set.
- **`dm_name`** and **`dm_uuid`**, on a device-mapper device: its name, as under `/dev/mapper/`, and its UUID, whose prefix, such as `LVM-`, `CRYPT-`
  or `mpath-`, says what made it. **`md_level`**, on an MD device: its RAID level, such as `"raid1"`, or `"container"` for an IMSM or DDF container.
- **`partition_table_type`**, udev's `ID_PART_TABLE_TYPE`, `"gpt"` or `"dos"`.
- **`filesystem_type`**, **`filesystem_label`** and **`filesystem_uuid`**, udev's `ID_FS_TYPE`, `ID_FS_LABEL` and `ID_FS_UUID`: the signature on the
  whole device, a filesystem, an LVM physical volume (`"LVM2_member"`), a RAID member or swap.
- **`mount_points`**, where the device is mounted, in the mount table's order, without duplicates. An active swap device has none: `lsblk`'s `[SWAP]`
  comes from `/proc/swaps`, not the mount table, and `filesystem_type: "swap"` says it is swap.
- **`slaves`** and **`holders`**, by name, sorted: the devices this one is built on, and the ones built on it. A consumer walks from a mounted LVM volume
  to its disks through `slaves`, and from a disk or partition to what uses it through `holders`.
- **`partitions`**, always present, `[]` when none, by partition number.

Each partition has `name`, `number`, `start_bytes`, `size_bytes` and `read_only` (always present, `0` or `false` when unreadable), then
**`partition_type`** (udev's `ID_PART_ENTRY_TYPE`, a GPT type GUID or an MBR type such as `"0x83"`), **`partition_label`** (`ID_PART_ENTRY_NAME`, the
GPT partition name, which is not a filesystem label) and **`partition_uuid`** (`ID_PART_ENTRY_UUID`), followed by `filesystem_type`,
`filesystem_label`, `filesystem_uuid`, `mount_points` and `holders` as on a device.

The value rules are the common ones, with udev's fields read as udev stored them:

- A field whose source the device does not have is absent: `dm_name` on a disk, `vendor` on NVMe, `slaves` on a device built on nothing.
- A sysfs string that exists but cannot be read is `"unknown"`; a sysfs `device/model` that cannot be read stays `"unknown"`, without udev's `ID_MODEL`
  in its place.
- A udev value that is empty counts as missing. A device udev has no entry for, or a host without udev's database, such as a container without udevd,
  has none of the udev fields, never empty strings: the device is still listed with its sysfs fields.
- A device or partition whose files cannot be read is still listed, with what could be read.

The mount points come from the host's mount table, `/proc/1/mountinfo`, not the agent's own: systemd gives the agent's mount namespace extra mounts,
such as `PrivateTmp=`'s binds of the root filesystem over `/tmp` and `/var/tmp`, which the host does not have. Where `/proc` is mounted with `hidepid`
and the agent cannot read PID 1's table, it reads its own, `/proc/self/mountinfo`, leaving out the mounts systemd makes for the agent's unit:
`PrivateTmp=`'s binds, a mount whose root, the directory it binds, has a path component starting with `systemd-private-`, and `ExecPaths=`'s bind of
the agent's binary onto itself, recognised by its mount point, the binary's path. Every other mount in it, a bind of a file included, is the
host's and is kept. A mount is matched to a device by its device number, or, for a btrfs subvolume, whose device number is anonymous, by its
source: `/dev/<name>` or `/dev/mapper/<dm_name>`. A mount with major 0 whose source names neither, such as `/dev/root` or a `/dev/disk/by-*` path,
gives no mount point. A bind mount lists its mount point on the device it comes from. When neither table can be
read, no device has `mount_points`, and **`mounts_error`** says why; it is absent otherwise.

A host or container without `/sys/block` reports `{"devices": []}`. Only a `/sys/block` that exists and cannot be listed fails the refresh, and the
agent then keeps serving the last list it collected, with `errors.block` naming the failure.

### The `udev-*` subsystems

Seven subsystems report what udev recorded about each device of one kind, read from udev's database in `/run/udev/data`:

| Subsystem    | udev subsystems                                  | Typical use                                                            |
| ------------ | ------------------------------------------------ | ---------------------------------------------------------------------- |
| `udev-block` | `block`                                          | disk identity: `ID_WWN`, `ID_SERIAL`, `disk/by-id` and `by-path` links |
| `udev-net`   | `net`                                            | interface naming: `ID_NET_NAME_PATH`, `_SLOT`, `_ONBOARD`, driver      |
| `udev-pci`   | `pci`                                            | vendor and model names from udev's hardware database, `ID_PATH`        |
| `udev-nvme`  | `nvme`, `nvme-subsystem`                         | NVMe controllers and subsystems, as opposed to their namespaces        |
| `udev-scsi`  | `scsi`, `scsi_host`, `scsi_disk`, `scsi_generic` | the HBAs, targets and `sg` nodes behind the disks                      |
| `udev-drm`   | `drm`                                            | GPU device nodes and their PCI paths                                   |
| `udev-vfio`  | `vfio`, `vfio-dev`                               | devices bound for passthrough                                          |

Each payload is `{"devices": [...]}`, one entry per device, in the order of udev's database file names. An entry carries:

- `id`: udev's name for the device: `b<major>:<minor>` for a block device, `c<major>:<minor>` for another device node, `n<ifindex>` for a network
  interface, `+<subsystem>:<sysname>` for a device with neither.
- `subsystem`, `devpath` (under `/sys`) and `devname` (such as `/dev/sda`), read from sysfs, which udev's database does not record. Any sysfs does not
  resolve is `"unknown"`; `devname` is `""` for a device without a node, such as a network interface.
- `properties`: every property udev recorded, exactly as it recorded it; `links`: the device's symlinks under `/dev/`, in udev's order; `tags` and
  `current_tags`. Each is always present, empty when udev recorded nothing.

The entries change only when a device appears, disappears or is changed, such as a disk reformatted or an interface created, so an unchanged host serves
unchanged payloads. On a hypervisor, virtual interfaces created and removed with each VM make `udev-net` the one that changes most. A host without udev's
database, such as a container without udevd, reports no devices; a database that cannot be read fails the refresh, and the last list collected is kept.

## `complete`, `disabled` and `errors`: a three-way distinction

These three fields answer three different questions, and conflating any two of them will misinform a consumer:

| Field        | Question it answers                                                        | Absence from `data` means...                     |
| ------------ | ---------------------------------------------------------------------------- | --------------------------------------------------- |
| `disabled`   | Which subsystems has the *operator* switched off in this host's configuration? | The operator chose this; it is not evidence of anything about the hardware. |
| `errors`     | Which subsystems' *most recent* collection attempt failed?                    | Not applicable — a subsystem in `errors` may still have data (see below).   |
| `complete`   | Is every subsystem this envelope is responsible for actually present in `data`? | The subsystem was never collected, or is failing with no prior good data. |

A subsystem named in `errors` can still be present in `data`: that means the agent is serving last-good, previously collected data while the current
refresh attempt fails, and `collected_at` on that entry shows how old the data actually is. Only a subsystem missing from `data` altogether drags
`complete` to `false`.

### The rule that matters most: never reconcile destructively on `complete: false`

`complete: false` means one or more *enabled* subsystems are missing from this response — either they have not collected yet, or they are failing with no
prior good data. **It does not mean the hardware is gone.** A subsystem missing from a partial response is silence, not evidence of removal.

Concretely, for a consumer that keeps an inventory database in step with its hosts: if you read `complete: false` and a subsystem such as `pci` or `block` is absent, **do not**
delete or deprovision the PCI devices or block devices you previously recorded for that host. Treat the response as "I don't currently know," not as "this
no longer exists." A sync loop that deletes on absence will, the first time a probe glitches or the agent restarts mid-collection, silently erase real
inventory it will not get back except by re-discovery. Prefer an upsert-only strategy driven by `complete: true` responses, or a separate reconciliation
pass that requires several consecutive complete, matching responses before it treats an absence as authoritative.

A subsystem is different from a *disabled* subsystem: `disabled` is a deliberate operator choice, reported alongside `complete`, and does not affect
`complete` at all. Only an enabled subsystem missing from `data` counts against completeness.

## Conditional requests

Every 200/304 response carries a weak `ETag` (`W/` followed by a double-quoted, lowercase-hex SHA-256 digest) and, when the collection time is known, a
`Last-Modified` header. Send that ETag back as `If-None-Match` to revalidate cheaply:

```http
If-None-Match: W/"3fae1c9d1b3a7e9c4f7b2a6d8e5c0f1a2b3c4d5e6f708192a3b4c5d6e7f80912"
```

The ETag covers the inventory payload, the `errors` it is served with, and the envelope's `schema_version` and `host_id`. It deliberately does not
cover `collected_at`: a refresh that re-collects identical hardware keeps the same ETag, so routine polling of many hosts returns `304` for every host whose
hardware and error state are unchanged, even though the agent refreshes every minute by default. A refresh that starts failing, or recovers, changes the ETag,
so you never miss a new or cleared error. If you need the latest `collected_at` itself, read `Last-Modified` from the `304`, or make an unconditional request.
Two `pci` values are live state rather than hardware: a device's `link.current_speed`, which GPUs and some NICs lower when idle, and its `power_state`.
Either can change the `pci` ETag, and the full snapshot's, while the hardware stays the same.

A match (or the literal `If-None-Match: *`) returns `304` with an empty body but the same headers a `200` would have carried, `Content-Type` included.
Matching is the weak comparison HTTP defines for `If-None-Match`: the digest must match exactly, and a `W/` prefix on either side is ignored. Use this for
routine polling: it lets you detect "did anything change" without paying to re-parse and re-transmit a full body every time.

## `?refresh=true`

By default every request is served from the agent's in-memory cache, populated by its own background schedule (`inventory.refresh.interval` and any
per-subsystem override). Adding `?refresh=true` instead forces a synchronous collection before the response is built. It is recognised only as that exact
literal string; `false`, an empty value, or omitting the parameter all behave identically to not supplying it at all.

This is materially more expensive than the cached path: it runs the actual probe (a `/sys`, `/proc` or `netlink` read) synchronously, inline with the
request, instead of returning a value already computed on a background timer. Use it when you have a specific reason to believe the cached value is stale
relative to a real-world change — for example, immediately after you know a device was hot-added or removed — not as a matter of routine on every poll;
doing that defeats the point of the agent's own refresh scheduling and adds load for no benefit on a host where nothing changed. On the snapshot route,
`refresh=true` refreshes every *enabled* subsystem, but never one disabled in configuration — the agent will not force a disabled subsystem's probe cost
just because a snapshot was requested with this parameter.

The agent runs at most one collection of a given subsystem at a time. If one is already in progress, a forced refresh waits for it to finish and then
collects again, so the response always reflects a probe that started after your request arrived. If another forced request probes that subsystem after
yours arrived but before your turn comes, your request reuses that newer result instead of probing again.

If you disconnect or cancel a forced refresh, the agent stops as soon as it notices: it starts no further subsystem probes and builds no response. A probe
that has already started cannot be interrupted, because the probes do not watch for cancellation, so it runs to completion; its
result is still stored, since it is valid inventory, and the next request benefits from it. The agent records such a request with status `499` (client
closed request) in `hwi_api_http_requests_total` and in its request log, rather than as a success.

## Correlating a response to a host

Do not build a correlation key out of DMI serial numbers as your primary or sole identifier. Prefer, in order:

1. **`host_id`** — an HMAC-derived identifier, stable for the life of the host's OS installation, that never exposes the underlying machine ID (which
   systemd itself documents as confidential). This is the agent's intended correlation key. It is `""` when the agent could not read a machine ID, so
   treat an empty value as "no key", never as a key shared by every such host.
2. **The hostname** — reported in the `os` payload (`data.hostname` on `/api/v1/inventory/os`, or `data.os.data.hostname` in a snapshot), and
   updated on each `os` refresh: after a rename, it shows the new name from the next scheduled `os` refresh, or at once with `?refresh=true`. Useful as
   a human-readable secondary signal, but not authoritative on its own (hostnames get reused, renamed and mistyped), which is why
   the envelope does not carry it.
3. **DMI serials and UUIDs — corroboration only, never a primary key.** They live inside the `product`, `baseboard` and `chassis` subsystem payloads
   (fields such as `serial_number` and `uuid`), not in the envelope, precisely because they are unreliable. Expect them to be **absent, vendor-placeholder,
   or duplicated across an entire batch** on whitebox and many Supermicro systems — hardware that never had these fields programmed at the factory ships
   with the same placeholder string on every unit.

### Reading the `identifiers` supplement, not the raw field

Two traps sit in the raw `serial_number`/`uuid` fields on `product`, `baseboard` and `chassis`:

- When the field is genuinely unreadable, the agent reports the **literal string `"unknown"`**, not an empty string: that is the agent's convention,
  which the `cpu` and `topology` payloads follow too. A check that only tests for `""` will treat `"unknown"` as if it were a real, usable serial.
- Even when a value is present and non-empty, it may still be a vendor placeholder (`"To Be Filled By O.E.M."`, a string of repeated digits, an
  all-zeros or all-`f` UUID, and similar) rather than a genuine per-unit value.

Rather than re-implement that blocklist yourself, read the `identifiers` object the agent adds alongside the raw fields on `product`, `baseboard` and
`chassis`. Each entry names the field it assesses and carries `value`, `usable`, an optional `reason`, and `raw` (the untouched original):

```jsonc
{
  "serial_number": "To Be Filled By O.E.M.",
  "uuid": "3f2a91c4-6b8d-4e17-9c05-a827f4d1b6e2",
  "identifiers": {
    "serial_number": { "value": "", "usable": false, "reason": "vendor placeholder or unset", "raw": "To Be Filled By O.E.M." },
    "uuid": { "value": "3f2a91c4-6b8d-4e17-9c05-a827f4d1b6e2", "usable": true, "raw": "3f2a91c4-6b8d-4e17-9c05-a827f4d1b6e2" }
  }
}
```

`value` has no `omitempty` on the `Identifier` struct, so it always serialises — as `""` for an unusable entry — never as an absent key. Only `reason` is
optional (`omitempty`), present only when the entry is unusable. A consumer that branches on the key's presence rather than on `usable` will misread every
unusable entry as if it were usable.

Check `identifiers.<field>.usable` before treating a DMI value as meaningful at all — and even then, treat it as corroboration alongside `host_id`, never
as the join key by itself. A batch of otherwise-identical whitebox machines can all report the same genuinely-programmed (not placeholder) serial number,
so "usable" is not the same claim as "unique."

## Errors

Every non-2xx response is a JSON body of the form `{"error": {"code": "...", "message": "..."}}`. `message` is deliberately generic — filesystem paths and
upstream error detail stay in the agent's own log, never in the response — so branch on `code`, not on the text of `message`.

| Status | Code                       | Meaning                                                                                      |
| ------ | --------------------------- | ------------------------------------------------------------------------------------------------ |
| 401    | `unauthorized`               | No credentials were presented at all.                                                            |
| 403    | `forbidden`                  | The peer address is not on the listener's allow list, or credentials were presented but refused. |
| 404    | `not_found`                  | The `{subsystem}` path segment is not a known subsystem name.                                    |
| 404    | `subsystem_disabled`         | The subsystem is real but switched off in this host's configuration.                             |
| 405    | `method_not_allowed`         | A method other than GET or HEAD was used.                                                        |
| 503    | `subsystem_unavailable`      | The subsystem has never collected successfully (cold start, or persistent failure with no prior data). |
| 503    | `too_many_requests`          | **Not a 429.** The server's own concurrency limit was hit and this request was rejected rather than queued. See below. |
| 500    | `internal`                   | An unexpected server-side failure (a recovered panic, or a response that could not be encoded).  |

### The `503`/`too_many_requests` pairing is deliberate

When the agent's configured `api.max_concurrent_requests` is exceeded, it answers `503` with error code `too_many_requests` and a `Retry-After: 1` header
— not the `429 Too Many Requests` status a client library might expect for a rate-limit condition. This pairing is intentional, documented behaviour, not
a bug to route around. If your HTTP client or framework has built-in "retry on 429" logic, it will not fire here; branch on the error `code` inside the
body (or on the `Retry-After` header's presence) rather than assuming status `429` will ever appear. The other condition that also produces `503`
(`subsystem_unavailable`) carries no `Retry-After` header, so its presence is itself a useful signal for which of the two you received.
