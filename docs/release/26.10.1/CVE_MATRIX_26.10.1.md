# CasaNova 26.10.1 — Matriz de CVE (23 filas)

*2026-10-24. Las 23 filas del alcance de 26.10.1: 10 Node.js, 1 Go, 2 containerd, 2 PHP, 2 Arcane, 2 Nginx UI, 2 SFTPGo, 1 FrankenPHP, 1 CoreDNS. Cada fila se clasificó contra su fuente oficial y la revisó con evidencia propia alguien distinto de su autor.*

**Productos ausentes con un control equivalente en CasaNova** (Arcane, Nginx UI, SFTPGo): además de constatar que el producto no está, se auditó el control equivalente en el código propio. El estado final es el más específico: si esa auditoría encontró y corrigió un hallazgo de la misma clase, la fila queda **Mitigado** con el fix (F-xx, ver `SECURITY_AUDIT_26.10.1.md` §5).

**Categorías:** (1) código propio · (2) dependencias incorporadas en artefactos distribuidos · (3) herramientas sólo de build/dev · (4) anfitrión o apps de terceros administradas por CasaNova. Un CVE de categoría 3 o 4 no implica vulnerabilidad explotable en CasaNova.

Columnas: CVE | Producto | Versión detectada | Ubicación | Estado | Evidencia | Acción | Validación | 27.04.

---

## Node.js — 10 filas

| CVE | Producto | Versión detectada | Ubicación | Estado | Evidencia | Acción | Validación | 27.04 |
|---|---|---|---|---|---|---|---|---|
| CVE-2026-111497 | Node.js — decodificador Brotli (`node:zlib`), OOB write → RCE (alta) | No distribuida en el producto. Máquina de build de 26.10: **Node v20.18.1** (EOL desde 2026-04-30), sin `.nvmrc`/`engines` | Compilación (cat. 3) | **No afectado** | Tarball de release: 10 binarios Go, 0 binario `node`, 0 `.js` fuera de `www/assets/` (`tar tzf` + grep); los 10 `systemd/*.service` apuntan sólo a `/usr/bin/casanova*`. Boletín oficial de nodejs.org (consultado 2026-10-24): afecta 26.x/24.x/22.x, corregido en 22.23.4/24.22.1/26.12.1 (2026-10-20/21, contrastado con `nodejs.org/dist/index.json`) | `make frontend` exige Node ≥ 22.23.4 / 24.22.1 / 26.12.1 (`check-node`); `engines` y `.nvmrc` en `CasaNova-UI-v2` | `node -v` ≥ 22.23.4; `make frontend`; vitest verde | Mismo cambio, pendiente de integrar |
| CVE-2026-105482 | Node.js `dns.resolve*()`/`dns.reverse()` truncan hostnames con NUL (alta) | Ídem | Compilación (cat. 3) | **No afectado** | El resolver DNS del producto es Go `net` (`CasaNova/pkg/utils/ip_helper`, `outbound_fetch.go`), nunca Node | Acción común | Ídem | Ídem |
| CVE-2026-105483 | Node.js `tls.checkServerIdentity()` con IP en dígitos fullwidth — **sólo 24.x/26.x** (alta) | Ídem (20.x no listada, y es EOL) | Compilación (cat. 3) | **No afectado** | El TLS del producto es `crypto/tls` de Go. Rango exacto (excluye 22.x) contrastado con el boletín oficial | Acción común | Ídem | Ídem |
| CVE-2026-106069 | Node.js bypass de hostname TLS con IPv4 no canónica — 26.x/24.x/22.x (alta) | Ídem | Compilación (cat. 3) | **No afectado** | `npm install` habla con `registry.npmjs.org` por nombre, no por IP literal; requeriría MITM activo sobre la máquina de build, no sobre el producto | Acción común | Ídem | Ídem |
| CVE-2026-105481 | Node.js `http.Agent` keep-alive acepta respuesta anticipada (media) | Ídem | Compilación (cat. 3) | **No afectado** | Los clientes HTTP del producto son Go `net/http`/resty | Acción común | Ídem | Ídem |
| CVE-2026-106072 | Node.js cert con IPv4 no canónica autenticado vía SAN DNS (media) | Ídem | Compilación (cat. 3) | **No afectado** | — | Acción común | Ídem | Ídem |
| CVE-2026-106074 | Node.js cachés de entropía de `randomUUID()`/`randomInt()` en snapshots/SEA (media) | Ídem | Compilación (cat. 3) | **No afectado** | CasaNova no construye snapshots ni SEA; los UUID del producto son Go (`google/uuid`) | Acción común | Ídem | Ídem |
| CVE-2026-106070 | Node.js `zlib.ZipFile` devuelve bytes ajenos tras fallo de validación de ZIP — **sólo 26.x** (baja) | Ídem (20.x no listada) | Compilación (cat. 3) | **No afectado** | Los ZIP del producto (app-stores, theme packs) los abre Go `archive/zip`. Rango 26.x-only contrastado con el boletín oficial | Acción común | Ídem | Ídem |
| CVE-2026-106071 | Node.js Permission Model: over-grant por comodín en radix lookup — **sólo 26.x** (baja) | Ídem | Compilación (cat. 3) | **No afectado** | No se usa `--allow-fs-*` en ningún script (`grep -r allow-fs` vacío) | Acción común | Ídem | Ídem |
| CVE-2026-106073 | Node.js Permission Model bypass vía `fs.realpathSync()` — 26.x/24.x/22.x (baja) | Ídem | Compilación (cat. 3) | **No afectado** | Mismo motivo que la anterior | Acción común | Ídem | Ídem |

*Las dependencias embebidas en el binario `node` que el boletín también actualiza (c-ares, ca-cert eMudhra, OpenSSL, SQLite, Undici) no tienen CVE propio en el alcance. `undici` no es dependencia npm del proyecto (`grep -c '"undici"' package-lock.json` → 0).*

---

## Go — 1 fila

| CVE | Producto | Versión detectada | Ubicación | Estado | Evidencia | Acción | Validación | 27.04 |
|---|---|---|---|---|---|---|---|---|
| CVE-2026-111943 (GO-2026-6809) | Go stdlib `crypto/x509.Certificate.Verify` no desconfía de intermedias comprometidas | **go1.24.2** en 9 binarios de 26.10, **go1.24.9** en `casanova-app-management` (`go version` sobre los 10 binarios del tarball, checksum contrastado con `SHA256SUMS`) | Código + runtime (cat. 1/2: stdlib compilada dentro de los 10 binarios) | **Afectado en 26.10 → Mitigado en 26.10.1** | `vuln.go.dev/ID/GO-2026-6809.json`: fixed 1.26.10 / 1.27.3, sin fix en la serie 1.24. `govulncheck -mode=binary` (v1.9.0) sobre los binarios del tarball: `x509.Certificate.Verify` alcanzable. Alcanzable vía clientes HTTPS salientes (app-stores, GitHub, Docker Hub, nube, rclone); el servidor TLS del gateway no verifica certificados de cliente, así que no es la superficie expuesta | `toolchain go1.26.10` en los 11 `go.mod`; `GOTOOLCHAIN` exportado en el Makefile; `make build` termina con `check-go` | `go version -m` → go1.26.10 en los 10; `go test -count=1 ./...` verde en los 11 módulos; `govulncheck -mode=binary` ya no lista GO-2026-6809 ni ningún hallazgo de stdlib; `make check-go GO_TOOLCHAIN=go1.24.2` (prueba negativa) falla y lista cada binario | Mismo cambio, pendiente de integrar |

---

## containerd — 2 filas

| CVE | Producto | Versión detectada | Ubicación | Estado | Evidencia | Acción | Validación | 27.04 |
|---|---|---|---|---|---|---|---|---|
| CVE-2026-95837 (GO-2026-6791) | containerd — plugin CRI, checkpoint-restore elude el contexto de seguridad del destino | `containerd/containerd/v2 v2.1.4` indirecto en `CasaNova-AppManagement` (vía `docker/compose/v2` + `moby/buildkit`). Anfitrión de referencia: **containerd 2.4.2** | Código (dep. Go indirecta, nunca importada) + anfitrión (`containerd.io` vía apt, sin pin) | **No afectado** en código propio ni en el anfitrión | Rango oficial GHSA-p7v4-vr35-mj6f: `2.1.0→<2.2.7` y `2.3.0→<2.3.4`. Tres pruebas coinciden en que el plugin CRI no está enlazado: `go list -deps` sin `pkg/cri`/`plugins/cri`; `govulncheck -mode=binary` sólo a nivel de módulo; `strings` sobre la pclntab del binario stripped → 0 ocurrencias de `pkg/cri`/`plugins/cri`/`internal/cri`. CasaNova sólo invoca `Up`/`Pull`/`Create`. Anfitrión 2.4.2 posterior al fix según la cronología de releases | Ninguna de seguridad en 26.10.1. Higiene para 27.04: subir `docker/compose/v2` para que un SBOM deje de marcar el módulo (`v2.1.4` cae en el rango del GHSA aunque el código vulnerable no esté enlazado) | `go list -deps ./... \| grep -i "pkg/cri\|plugins/cri"` → vacío; `strings -n 24 bin/casanova-app-management \| grep -c -E 'containerd/v2/(pkg/cri\|plugins/cri\|internal/cri)'` → 0 | Mismo patrón de dependencia esperado |
| CVE-2026-95838 (GO-2026-6796) | containerd — DoS por whiteouts opacos repetidos al desempaquetar capas | Igual: `v2.1.4` indirecto, sólo en `casanova-app-management`. Anfitrión de referencia **2.4.2** | Código (dep. Go indirecta) + anfitrión (desempaqueta cada imagen que el usuario instala o actualiza) | **No afectado** en código propio ni en el anfitrión | Rango oficial GHSA-rp3h-jf77-q9p4: `<1.7.35`, `<2.0.12`, `2.2.0→<2.2.8`, `2.3.0→<2.3.5` — la rama 2.1.x no está en ningún rango, así que "govulncheck no lo lista" no prueba nada por sí solo. La prueba válida: `go list -deps` no alcanza `pkg/archive` (sólo `pkg/archive/compression`, que no es el código de unpack), reproducido sobre el binario stripped (pclntab) → 0 ocurrencias de `pkg/archive`. Anfitrión 2.4.2 posterior a las 4 versiones corregidas | Ninguna | Igual que la fila anterior | Ídem |

---

## PHP — 2 filas

| CVE | Producto | Versión detectada | Ubicación | Estado | Evidencia | Acción | Validación | 27.04 |
|---|---|---|---|---|---|---|---|---|
| CVE-2026-109618 | PHP core — OOM → corrupción de memoria, RCE. **CVSS 3.1 9.8 CRITICAL** (NVD) | N/A en código propio. En la instalación de referencia, tres contenedores de apps de usuario por debajo del fix: `wordpress:latest` (PHP **8.2.17** < 8.2.35), `nextcloud:34.0` (PHP **8.5.8** < 8.5.12), `jokobsk/netalertx:latest` (PHP **8.3.29** < 8.3.36) | N/A código propio; **categoría 4** para las apps (instaladas por el usuario; CasaNova no mantiene catálogo propio) | **No afectado** (cat. 1-3); **categoría 4** para WordPress/Nextcloud/NetAlertX | NVD API 2.0, publicado 2026-10-21, rangos CPE `php` 7.3.0→<8.2.35 / 8.3.0→<8.3.36 / 8.4.0→<8.4.27 / 8.5.0→<8.5.12; `php.net/ChangeLog-8.php` confirma 8.2.35/8.3.36/8.4.27/8.5.12 publicadas el 22 Oct 2026. Código propio: el único "php" del árbol es una regex de nombres de archivo índice en `CasaNova-Gateway/route/static_route.go:100` (no ejecuta PHP). Los catálogos por defecto (`app-management.conf.sample`) son IceWhale (jsdelivr) y BigBear (GitHub) | Ninguna de código. Aviso a administradores en `RELEASE_NOTES_26.10.1.md`: actualizar la imagen de cada app desde la UI | Grep reproducible sobre el repo; NVD/php.net consultados | Mismo resultado esperado |
| CVE-2026-109619 | PHP core — estado global obsoleto entre requests, RCE. **CVSS 3.1 8.1 HIGH** (NVD) | Igual que la fila anterior | Igual | Igual | Misma evidencia | Igual | Igual | Igual |

---

## Arcane — 2 filas

| CVE | Producto | Versión detectada | Ubicación | Estado | Evidencia | Acción | Validación | 27.04 |
|---|---|---|---|---|---|---|---|---|
| CVE-2026-96627 (GHSA-pw5q-rjx4-7f7p / GO-2026-6807) | Arcane `<2.6.0` — `users:update` delegado permite tomar la cuenta admin vía reset de contraseña | N/A — Arcane no está en el repo. Corregido en **2.6.0** | N/A (producto ausente) + control equivalente en UserService | **Mitigado** (F-10) | No existe delegación de permisos (`rbac.go:30-32,127-161`, 3 roles fijos, `PermUserManage` sólo admin). `PutUserPassword` sólo cambia la propia contraseña y exige la anterior; `PutUserInfo` fuerza el id del JWT y conserva el rol — no hay reset de contraseña ajena. Hallazgo real de la misma clase: un cambio de rol no revocaba sesiones existentes y los servicios confían en el claim `role` del JWT, así que un admin degradado seguía actuando como admin hasta ~3 h | F-10: el cambio de rol revoca las sesiones del usuario | `TestPutUserRoleRevokesTargetSessions` rojo→verde | Portado, pendiente de integrar |
| CVE-2026-96628 (GHSA-mp22-58ff-6fhr / GO-2026-6806) | Arcane `<2.11.0` — credencial alternativa llega a `/api/debug/pprof` eludiendo la restricción de admin | N/A. Corregido en **2.11.0** | N/A (producto ausente) + control equivalente en todos los servicios | **No afectado** | `grep -rln "net/http/pprof\|debug/pprof"` vacío en todo el monorepo. El endpoint de diagnóstico equivalente, `GET /v1/sys/debug`, exige `requireSystemPrincipal(PermSystemConfig)`: admin y principal real; el secreto interno entre servicios salta el JWT pero el handler ve el id 0 de guest y lo rechaza; los tokens refresh y `oauth2-delegated` los rechaza `jwt.Validate` | Ninguna | Grep + lectura del handler; test de login sin código 2FA que no emite nada | Igual |

---

## Nginx UI — 2 filas

| CVE | Producto | Versión detectada | Ubicación | Estado | Evidencia | Acción | Validación | 27.04 |
|---|---|---|---|---|---|---|---|---|
| CVE-2026-84321 (GHSA-wq4w-vwvq-q3x9 / GO-2026-6801) | Nginx UI `<2.4.1` — tokens revocados persisten en caché | N/A — Nginx UI no está en el repo. Corregido en 2.4.1 | N/A + control equivalente (revocación de tokens en Common y en todos los servicios) | **Mitigado** (F-12, F-10) | `jwt.RevokedForClaims(claims)` en el middleware JWT, falla cerrado; todos los servicios consultan la lista compartida (releída ≤1 s). Huecos reales de la misma clase: (a) el `id_token` OIDC se firmaba sin claim `id` y `jwt.Validate` lo aceptaba como access token de id 0, fuera de toda revocación; (b) cambio de rol sin revocar (misma fila que CVE-2026-96627); (c) logout sólo revoca la familia de refresh y el access token vive hasta expirar — riesgo documentado (R-02), no corregido | F-12 y F-10; (c) documentado | `TestValidateRejectsTokenWithoutUserID`, `TestIssuedIDTokenIsNotAnAccessToken` rojo→verde | Portado, pendiente de integrar |
| CVE-2026-84360 (GHSA-vc32-4gwf-77xp / GO-2026-6799) | Nginx UI `<2.4.1` — interfaz alternativa (MCP) sin el step-up de OTP | N/A. Corregido en 2.4.1 | N/A + control equivalente (2FA de UserService) | **Mitigado** (F-14) | `PostUserLogin` es el único creador de sesiones y llama `totpEnforceLogin` antes de emitir token; el proveedor OAuth2/OIDC y el refresh emiten tokens pero dependen de una sesión que ya pasó por `PostUserLogin`; websocket, puerto de compatibilidad 9527 y CLI no emiten tokens; desactivar 2FA exige admin + código válido. Hueco real: el mismo código TOTP valía de nuevo dentro de su ventana (~90 s) | F-14: un código TOTP sólo vale una vez | `TestTOTPLoginCodeCannotBeReplayed` rojo→verde | Portado, pendiente de integrar |

---

## SFTPGo — 2 filas

| CVE | Producto | Versión detectada | Ubicación | Estado | Evidencia | Acción | Validación | 27.04 |
|---|---|---|---|---|---|---|---|---|
| CVE-2026-103028 (GHSA-wwv9-g5x4-966q / GO-2026-6802) | SFTPGo `2.5.0–2.7.5` — filtros de archivo validados sólo sobre el destino de una copia, no sobre cada hijo | N/A — SFTPGo no está en el repo. Corregido en 2.7.6 | N/A + patrón equivalente en la file API propia | **No afectado hoy; patrón latente documentado** (R-05) | `sftp` en el repo es sólo cliente (restic/rclone hacia un servidor externo). Copia y movimiento validan origen y destino, pero compresión, descarga-como-archivo, copia recursiva y búsqueda no revisan cada hijo — hoy no explotable porque las reglas que ocultarían un hijo (DENY de operador, grants) no tienen ningún llamador en producción (`storageauth.AddOperatorDeny`/`AddGrant` sin uso fuera de tests) | Documentado como riesgo: exigir autorización por entrada si DENY/grants se activan | Sondas de auditoría que quedan en rojo a propósito como advertencia | No activar DENY/grants sin resolverlo antes |
| CVE-2026-77375 (GHSA-q7pc-356p-hggc / GO-2026-6792) | SFTPGo `<=2.7.4` — DoS por paquete `channel-open` SSH malformado | N/A. Corregido en 2.7.5 | N/A + código propio | **No afectado** | Ningún `ssh.NewServerConn`/`ssh.ServerConfig`/gliderlabs/servidor `pkg/sftp` en el árbol; `golang.org/x/crypto/ssh` sólo como cliente a 127.0.0.1; `github.com/pkg/sftp` es indirecta en LocalStorage y `go mod why` la da como no usada | Ninguna | Grep + `go mod why` | Igual |

---

## FrankenPHP y CoreDNS — 2 filas

| CVE | Producto | Versión detectada | Ubicación | Estado | Evidencia | Acción | Validación | 27.04 |
|---|---|---|---|---|---|---|---|---|
| CVE-2026-82416 (GHSA-v3ph-cgqh-r8p5) | FrankenPHP `<=1.12.4` — fuga de session id entre requests en workers | N/A — FrankenPHP no está en el repo. Corregido en 1.12.5 | N/A | **No aplica** | Ningún `go.mod` lo incluye, tampoco Caddy; mismo grep de la fila PHP (`\bphp\b\|frankenphp` → 1 falso positivo) | Ninguna | Grep reproducible | Mismo resultado esperado |
| CVE-2026-62994 (GHSA-74w3-63xv-x9mv) | CoreDNS `1.9.4–1.14.4` — caída con AXFR en plugin `k8s_external` | N/A — CoreDNS no está en el repo. Corregido en 1.14.5 | N/A | **No aplica** | `grep -rliE "coredns"` vacío en todo el monorepo; no está en el catálogo local `CasaNova-AppLibrary` ni entre los contenedores de la instalación de referencia | Ninguna | Grep reproducible | Mismo resultado esperado |

---

## Fuera de las 23 filas

- **GO-2026-6597** — "Containerd has image-pull DoS via crafted OCI index graph amplification" (`containerd/containerd/v2 v2.1.4`, en `casanova-app-management`). Aparece a nivel de módulo, no alcanzable (mismo patrón que GO-2026-6791: el código afectado no está enlazado). Queda pendiente con el resto de la pila containerd/docker para 27.04.
- **govulncheck sobre los 10 binarios de 26.10 — 210 avisos distintos en la unión**, la mayoría stdlib go1.24.2 (cerrada por el bump a go1.26.10 de la fila Go) y el resto módulos de terceros. Es deuda de dependencias, no CVE puntuales; lo aplicado y lo diferido está en `MAINTENANCE_CHANGELOG_26.10.1.md` ("Dependencies") y en `SECURITY_AUDIT_26.10.1.md` §7.
