# CasaNova 26.10.1 — Resultados de regresión

**Base:** release 26.10 · **Toolchain:** go1.26.10 (`GOTOOLCHAIN=go1.26.10`) · **Fecha:** 2026-10-24

En entornos aislados que prohíben sockets unix, 4 tests de `CasaNova-Virt/pkg/libvirtconn` fallan con `listen unix: operation not permitted`; en una máquina normal pasan.

## Rondas

| Ronda | Contenido | go mod verify | go vet ./... | go test ./... -count=1 |
|---|---|---|---|---|
| 1 | 8 fixes de seguridad + gate docker + go1.26.10 | — | 11/11 | 11/11 |
| 2 | + 8 fixes (auth, límites, gateway, multi-descarga) | — | 11/11 | 11/11 |
| 3 | + entrada de papelera `..` (F-17) + bumps de dependencias (x/net, x/crypto, x/text, x/image, jwt/v4, websocket, xz, grpc, go-jose, protobuf) + check-node | 11/11 | 11/11 | 11/11 |

Además, en la ronda 2, `gofmt -l` sobre los `.go` cambiados no dio salida.

## Tests de seguridad rojo→verde

- Cada fix F-01..F-17 trae un test nuevo. El autor lo vio fallar sobre el código anterior y pasar con el fix.
- Una revisión independiente lo verificó por su cuenta en los 17.
- F-01 (avatar: 3 rojos → 5/5 verdes) y el bypass `..` de la papelera (rojo antes de F-17) se reprodujeron además una tercera vez antes de integrar.

## Toolchain y dependencias

- **Binarios:** los 10 están compilados con go1.26.10 (target `check-go`). El marcador de la denylist de GO-2026-6809 está presente en los 10.
- **govulncheck -mode=binary:** los avisos de stdlib pasan de 48-63 a 0 por binario. Los de módulos, por ejemplo: gateway 51→7, casanova 70→12, local-storage 95→49, app-management 102→50.
- **Sonda TLS:** las cadenas públicas reales verifican. Los casos expired, self-signed, untrusted y wrong-host se rechazan.
- **UI:** `vue-tsc` + `vite build` OK y vitest 148/148, con Node 22.23.4.

## Paquete

- `make package` → `casanova-18.6.3-RELEASE-26.10.1-20261024-x86_64.tar.gz`: checksums internos OK, go1.26.10 ×10, `make test-package` OK, la UI muestra 26.10.1. Validación en una instalación real: `PRODUCTION_VALIDATION_26.10.1.md`.
