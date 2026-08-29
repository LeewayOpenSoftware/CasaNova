# CasaNova-Virt

KVM/QEMU virtual-machine management microservice for CasaNova OS (a CasaOS fork).
It is a clean-room reimplementation of ZimaOS's `zimaos-virt-management`,
reverse-engineered as the ground-truth oracle (see [ANALYSIS.md](./ANALYSIS.md)).

The service talks to libvirt over its RPC unix socket using the **pure-Go**
`github.com/digitalocean/go-libvirt` client, so it builds fully static with
`CGO_ENABLED=0` (the oracle used the CGO bindings, which are incompatible with
CasaNova's static build policy). Disk images are provisioned by shelling out to
`qemu-img`.

## Build

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
```

Requires the sibling `../CasaNova-Common` checkout (wired via a `replace`
directive, exactly like the other CasaNova services).

## Architecture

```
main.go            listener on 127.0.0.1:<ephemeral>; registers /v2/virtualization
                   with the gateway; Type=notify sd_notify(READY=1); graceful shutdown
route/             echo router + JWT (loopback-skipping) → handlers
  v2.go            middleware + route table
  v2/*.go          domain / storage / host HTTP handlers
service/           business logic over libvirt
  service.go       Services aggregator (Domain / Storage / Host / Gateway)
  domain.go        VM lifecycle (list/get/define/undefine/action/status/autostart/console)
  storage.go       pools + volumes
  host.go          hypervisor host info
  xml.go           builds libvirt domain XML from a VirtualMachineConfiguration
  xmlparse.go      reads back disks/NICs/graphics/os from domain XML
pkg/
  config/          ini config (/etc/casaos/virt-management.conf)
  libvirtconn/     lazy, self-healing qemu:///system connection over the unix socket
model/             JSON API types (mirror the oracle's codegen.* types)
common/            ServiceName / Version constants
build/sysroot/     systemd unit + conf sample (installed into the image)
```

Request flow: **gateway → echo route → handler → service → go-libvirt → virtqemud**.

## API (base path `/v2/virtualization`)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/info` | dashboard/host summary |
| GET | `/host` | hypervisor host info (CPU, memory, libvirt version, domain counts) |
| GET | `/domain` | list all VMs (state, memory, vcpu, autostart) |
| POST | `/domain` | define a VM from a `VirtualMachineConfiguration` |
| GET | `/domain/:name` | full VM detail (disks, NICs, graphics, OS) |
| DELETE | `/domain/:name` | stop (if running) and undefine |
| GET | `/domain/:name/status` | current libvirt state |
| PUT | `/domain/:name/status` | apply action: `start`/`shutdown`/`reboot`/`stop`/`suspend`/`resume` |
| POST | `/domain/:name/autostart` | toggle start-on-boot |
| GET | `/domain/:name/vnc` | VNC/SPICE console coordinates (host/port/password) |
| GET | `/pool` | list storage pools |
| GET | `/pool/:name/volume` | list volumes in a pool |

### Create example

```bash
curl -X POST http://<host>/v2/virtualization/domain -d '{
  "name": "ubuntu-vm",
  "memory": {"value": 4096},
  "vcpu": {"count": 2},
  "os": {"firmware": "uefi", "machine": "q35"},
  "disks": [
    {"device": "disk", "size_gib": 40, "bus": "virtio"},
    {"device": "cdrom", "source": "/DATA/Downloads/ISO/ubuntu-24.04.iso", "bus": "sata"}
  ],
  "interfaces": [{"type": "network", "source": "default", "model": "virtio"}],
  "graphics": [{"type": "vnc", "autoport": true, "listen": "0.0.0.0"}]
}'
```

An absent `disks[].source` on a `disk` device is auto-assigned under
`DiskDir` and a qcow2 of `size_gib` is created via `qemu-img`.

## Implemented vs stubbed

**Implemented (real libvirt-backed):**
- Connect to `qemu:///system` via go-libvirt over the unix socket (self-healing).
- List domains + per-domain state/memory/vcpu/autostart.
- Define a VM from ISO + qcow2 (qcow2 auto-provisioned by `qemu-img`), q35, BIOS
  or UEFI (`<os firmware='efi'>`), virtio disk/NIC, VNC/SPICE, optional emulated TPM.
- Lifecycle: start / graceful shutdown / reboot / force-stop / suspend / resume /
  undefine, plus autostart toggle.
- Storage pools + volumes (capacity/allocation/paths).
- Host info (hostname, hypervisor type, libvirt version, CPU model/count, memory,
  domain counts).
- Console address (VNC/SPICE host/port/password) parsed from the live domain XML.
- Gateway route registration + self-heal watchdog; systemd `Type=notify`.

**Stubbed / not yet implemented** (present in the oracle; deliberately omitted
rather than faked — see ANALYSIS.md §7):
- OS-image catalog + downloader (`/osim/*`, the `ZVM-VM-List` integration).
- Disk / NIC hot-plug & hot-unplug (`/domain/:name/disk`, `/interface`).
- VNC **websocket proxy** bridge (`/domain/:name/vnc/proxy`) — we return the
  console address; the noVNC ws relay is future work.
- Guided installation flow (`/domain/:name/installation`) and `sendkey`
  (`/domain/:name/key`).
- Host NIC status endpoint (`/network`) and the `/web/*` UI helpers.
- Message-bus lifecycle event publishing.

## Roadmap (remaining depth)

1. **Snapshots** — `DomainSnapshotCreateXML` / list / revert / delete
   (`external`/internal snapshot XML; go-libvirt exposes the RPCs).
2. **cloud-init** — attach a NoCloud seed ISO (build via `genisoimage`) or a
   config-drive so headless guests get SSH keys / hostname on first boot.
3. **OS-image catalog + downloader** — resumable downloads with the oracle's
   progress model, landing in `/DATA/Downloads/ISO`.
4. **Live migration** — `DomainMigrate3` to a peer host over the libvirt TLS/TCP
   sockets (`libvirtd-tls.socket` is already shipped by the image).
5. **USB / PCI passthrough** — enumerate node devices (`NodeListDevices`) and
   attach `<hostdev>` (IOMMU/VFIO); GPU passthrough for the media VMs.
6. **Resource hot-plug** — vCPU (`DomainSetVcpusFlags`) and memory
   (`DomainSetMemoryFlags`) live adjustment.
7. **VNC websocket console** + message-bus events for the UI.

## systemd

`build/sysroot/usr/lib/systemd/system/casanova-virt.service` — `Type=notify`,
ordered after `casanova-gateway`, `casanova-message-bus` and **`virtqemud.socket`**
(`Requires=`). It carries the mandatory `RuntimeDirectory=casanova casaos` +
`RuntimeDirectoryMode=0755` + **`RuntimeDirectoryPreserve=yes`** — without the
last, a restart wipes the shared `/run/casaos` and crash-loops the whole stack.

> **Note:** do not deploy to the live server from here — the main agent reviews
> and deploys.
