# CasaNova-Driver

Hardware & kernel-driver management microservice for CasaNova OS (a CasaOS fork).
Reverse-engineered from ZimaOS's driver tooling as the ground-truth oracle — see
[`ANALYSIS.md`](./ANALYSIS.md) for the full teardown.

It enumerates PCI/USB hardware, reports loaded kernel modules and their driver
bindings, detects GPUs and their active driver, lists installed out-of-tree driver
packages, and can load/unload modules and drive the GPU driver-branch selector.

Built `CGO_ENABLED=0` static: it reads `/sys` and `/proc` and shells out to system
tools (`modprobe`, `rmmod`, `modinfo`, `nvidia-driver-select`, `nvidia-smi`) rather
than linking any C library.

## Architecture

Mirrors CasaNova-LocalStorage: `main.go` registers route prefixes with the gateway
(via `management.url` -> gateway on :85) and runs the self-heal watchdog; requests
are dispatched by a `HandlerMultiplexer` (`v1` / `v2` / `doc`) into echo routers.

```
main.go                 gateway registration + watchdog, multiplexer, SdNotify
common/constants.go     ServiceName = "driver", version
model/                  PCIDevice, USBDevice, KernelModule, GPU, InstalledDriver
pkg/config/             driver.conf loader ([common]/[app]/[server])
pkg/pci/                pure-Go pci.ids name resolver
service/                route-agnostic system logic:
  device.go             PCI + USB enumeration from sysfs
  module.go             /proc/modules + modprobe/rmmod
  gpu.go                GPU detect + nvidia-smi enrich + branch select
  driver.go             installed out-of-tree driver packages (state-dir read)
route/v1.go route/v2.go echo routers + JWT/loopback middleware
route/v1/ route/v2/     handlers (v2 canonical; v1 delegates)
build/sysroot/          systemd unit + driver.conf.sample
```

## API

All endpoints served under `/v2/driver` (registered with the gateway). The
service used to also register `/v1/driver`, but the main CasaNova service owns
that prefix (cloud storage providers, the original CasaOS v1 endpoint); the
gateway keeps one target per prefix, so the duplicate claim made the route flap
between the two backends every 30s. Auth: shared CasaNova JWT, **skipped for
loopback** (gateway-proxied) requests. Responses use the common
`{success,message,data}` envelope.

| Method | Path | Purpose |
|---|---|---|
| GET | `/v2/driver/devices/pci` | list PCI devices (id/class/names/bound driver) |
| GET | `/v2/driver/devices/usb` | list USB devices (id/strings/per-interface drivers) |
| GET | `/v2/driver/modules` | list loaded kernel modules (`/proc/modules`) |
| POST | `/v2/driver/modules/{name}/load` | `modprobe {name}` |
| POST | `/v2/driver/modules/{name}/unload` | `modprobe -r {name}` (rmmod fallback) |
| DELETE | `/v2/driver/modules/{name}` | alias for unload |
| GET | `/v2/driver/gpu` | detect GPUs + bound driver (+ NVIDIA branch/version/uuid) |
| POST | `/v2/driver/gpu/select?load=true` | run the GPU driver-branch selector |
| GET | `/v2/driver/drivers` | list installed out-of-tree driver packages |
| GET | `/doc/v2/driver` | machine-readable route listing |

## Build

Requires the sibling `../CasaNova-Common` checkout (private module, wired via a
`replace` directive in `go.mod` — same as every other CasaNova service).

```sh
cd CasaNova-Driver
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...        # library check
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o casanova-driver .
```
Produces a statically-linked ELF (`ldd` -> "not a dynamic executable").

## Implemented vs stubbed

**Implemented & runtime-verified** (against real `/sys` + `/proc`):
- PCI enumeration from `/sys/bus/pci/devices` with `pci.ids` name resolution and
  bound-driver detection via the `driver` symlink.
- USB enumeration from `/sys/bus/usb/devices` with manufacturer/product/serial and
  per-interface driver bindings.
- Kernel-module listing from `/proc/modules` (size/refcount/used_by/state), with
  out-of-tree detection via `modinfo` filename path.
- Module load/unload (`modprobe`/`modprobe -r`/`rmmod`) with name validation.
- GPU detection (PCI display class `03`) + vendor mapping + NVIDIA enrichment via
  `nvidia-smi` (`external.NvidiaGPUInfoList`) + active branch from `driver.env`.
- GPU driver-branch selection (shells the configured selector).
- Installed out-of-tree driver listing by reading the generic state layout under
  `/opt/casanova/drivers` (mirrors `zimaos-driver list`: state/runtime/payload/modules).
- Gateway route registration + self-heal watchdog (mirrors LocalStorage `main.go`).

**Stubbed / not yet wired:**
- **Message-bus event registration** — `main.go` documents the hook but does not
  register events. It needs the generated message-bus client (`oapi-codegen` against
  the private message-bus OpenAPI), out of scope for a clean offline build.
- **Driver package install/enable/disable/remove** — the write side of the oracle's
  package manager is not exposed over HTTP (listing is). Design intent: shell to a
  `casanova-driver` CLI (config `DriverCLI`) or a port of the oracle's flow.
- **Candidate-module resolution** (`modules.alias` matching) for a PCI device with no
  bound driver returns empty rather than guessing.
- **Live GPU telemetry** (`/gpu/status`: memory/util/temp/power/clock) — needs NVML.
- **Full OpenAPI document** — `/doc` returns a route listing, not a spec.

## Roadmap
1. **Message-bus + udev hotplug events** — generate the message-bus client; publish
   `driver:*` / `device:*` events; watch udev (`github.com/pilebones/go-udev`, already
   used by LocalStorage) for add/remove and emit hotplug notifications.
2. **Driver package lifecycle** — HTTP install/enable/disable/remove backed by a
   ported package manager (kernel-vermagic gate, `.ko`/tarball/`.raw` sysext, depmod).
3. **Firmware management** — enumerate `/lib/firmware`, install firmware payloads,
   report missing-firmware for bound devices.
4. **Per-vendor GPU handling** — AMD (amdgpu/ROCm) and Intel (i915/xe) alongside
   NVIDIA; live telemetry via NVML/`/sys/class/drm`; `/gpu/applications` app binding.
