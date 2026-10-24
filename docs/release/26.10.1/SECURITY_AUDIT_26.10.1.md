# CasaNova 26.10.1 — Auditoría de seguridad

*2026-10-24 · Base: la línea 26.10 con los 17 fixes de código (F-01..F-17), F-D1, el toolchain go1.26.10, los bumps compatibles de dependencias, `check-node` el cambio de versión (BASE 18.6.3 / DISPLAY 26.10.1, canal beta) y los arreglos de la UI en teléfonos (sólo maquetación, sin cambio de seguridad). Paquete: `casanova-18.6.3-RELEASE-26.10.1-20261024-x86_64.tar.gz` (build `18.6.3.beta1.2410261443`, sha256 `300cf73a1d1c623daac201dfe6d5f6b6fefa9b0c2c6ff8b38c7372764e4314a8`). Sin secretos ni direcciones reales: la instalación de referencia se nombra "prod".*

Documentos relacionados (en `docs/release/26.10.1/`): `CVE_MATRIX_26.10.1.md` (23 filas), `MAINTENANCE_CHANGELOG_26.10.1.md` (lista técnica de cambios, toolchain y dependencias), `REGRESSION_RESULTS_26.10.1.md`, `PRODUCTION_VALIDATION_26.10.1.md`, `RELEASE_NOTES_26.10.1.md`.

---

## 1. Alcance

- **Producto auditado**: CasaNova 26.10 tal como corre en una instalación de referencia (paquete `18.6.2.beta1.0310261227`, display 26.10): 10 binarios Go + frontend estático + instalador/actualizador.
- **Pedido**: 23 CVE concretos de los boletines de octubre de 2026 (10 Node.js, 1 Go, 2 containerd, 2 PHP, 8 de productos relacionados), inventario en cuatro categorías, auditoría reproducible del código propio (FS, gateway/servicios, Docker), sólo fixes de seguridad y mantenimiento, sincronización con 27.04.
- **Fuera de alcance**: features, refactors, bumps mayores de dependencias, cambios en el motor Docker del anfitrión, apps de terceros instaladas por el usuario (categoría 4) más allá de documentarlas.

## 2. Método

1. **Inventario primero** (§3): versiones reales leídas del tarball de release (`go version -m`, listado del tar, units systemd) y del repo (`go.mod`, `package-lock.json`, Makefile), no de memoria.
2. **Cada CVE contra su fuente oficial, consultada el 2026-10-24**: boletín de nodejs.org + `dist/index.json`; `vuln.go.dev` (JSON) y `pkg.go.dev/vuln`; GitHub Advisory Database para los GHSA de containerd, Arcane, Nginx UI, SFTPGo, FrankenPHP y CoreDNS; NVD API 2.0 y `php.net/ChangeLog-8.php` para PHP; releases de containerd para la cronología. Nada se clasificó sin fuente; donde no la hubiera, la fila habría quedado "Indeterminado" (no ocurrió).
3. **Evidencia por fila**: `archivo:línea`, versión o `comando + salida`. Reachabilidad en binario con `govulncheck -mode=binary` (v1.9.0) y, donde la base de datos no cubre la versión embebida, con `strings` sobre la pclntab del binario stripped.
4. **Auditoría de código propio con pruebas**: cada hallazgo "Confirmado" tiene un test Go que falla sobre la base y pasa con el fix; los "Sospechados" sólo tienen evidencia estática y se declaran como tales. Los tests no tocan `/var/lib/casaos`, `/etc/casanova`, `/run` ni `/DATA`.
5. **Revisión independiente cruzada** (nada se aprueba por consenso): cada grupo de filas de la matriz lo revisó con evidencia propia alguien distinto de su autor, y cada fix lo revisó alguien distinto de quien lo escribió (§8).
6. **Integración**: un tema por rama, sin tocar la instalación de referencia hasta la validación final autorizada.

## 3. Inventario resumido

| Categoría | Qué hay | Hecho relevante para esta auditoría |
|---|---|---|
| **(1) Código propio** | 11 módulos Go (`CasaNova`, Gateway, MessageBus, UserService, LocalStorage, JobManagement, AppManagement, Driver, Virt, Common, CLI), frontend Vue 3 (`CasaNova-UI-v2`), `build/tools/*.sh`, `CasaNova-Installer/*.sh` | Todos los servicios corren como root; el gateway TLS es la única superficie pública |
| **(2) Incorporado en artefactos** | stdlib Go **1.24.2** (9 binarios) / **1.24.9** (app-management); 374 módulos Go distintos embebidos (55 en más de una versión); 88 paquetes npm bundleados en `www/` | stdlib sin parches recientes → §7. `containerd/v2 v2.1.4` sólo como cliente de registro dentro de app-management |
| **(3) Sólo build/dev** | Go 1.24.2 en la máquina de build; **Node v20.18.1 (EOL) + npm 10.8.2**; 337 paquetes npm `dev`; make/bash/tar | Node no viaja al servidor: compila el frontend y nada más |
| **(4) Anfitrión / terceros administrados** | Instalación de referencia: Ubuntu 24.04.5, Docker 29.9.2 / containerd 2.4.2 / runc 1.5.2 (repo oficial Docker, instalados sin pin por `install.sh`); apps de usuario con Node/PHP dentro de contenedores (WordPress, Nextcloud, NetAlertX, Nginx Proxy Manager, Gladys, Immich) | CasaNova no instala Node ni PHP ni un containerd propio; no hay catálogo de apps propio: el release configura por defecto las tiendas de IceWhale y BigBear |

Diferencias 26.10 ↔ 27.04 que afectan al forward-port: `go.mod` idénticos salvo `CasaNova/go.mod` (kin-openapi 0.124, echo-jwt directo, oapi-codegen runtime); `CasaNova-JobManagement` no existe en 27.04 (absorbido en `casanova`); frontend idéntico.

## 4. Resumen de la matriz de CVE (23 filas, `CVE_MATRIX_26.10.1.md`)

| Grupo | Filas | Estado final | Nota |
|---|---|---|---|
| Node.js (boletín 21/10/2026) | 10 | **No afectado** (×10) | Node es categoría 3. Exposición de build cerrada: `make frontend` exige un Node parcheado (`check-node`), `engines`/`.nvmrc` fijados |
| Go — CVE-2026-111943 / GO-2026-6809 | 1 | **Afectado en 26.10 → Mitigado en 26.10.1** | `x509.Certificate.Verify` alcanzable en los 10 binarios; fix sólo en Go 1.26.10/1.27.3; `toolchain go1.26.10` en los 11 `go.mod` + `GOTOOLCHAIN` en el Makefile; binarios recompilados: 0 hallazgos de stdlib en govulncheck |
| containerd — CVE-2026-95837 / 95838 | 2 | **No afectado** (código y anfitrión) | Embebido sólo como cliente en app-management: ni `pkg/cri` ni `pkg/archive` enlazados (pclntab); anfitrión 2.4.2 posterior a los fixes. Higiene SBOM para 27.04: la `v2.1.4` cae en el rango del GHSA |
| PHP — CVE-2026-109618 (9.8) / 109619 (8.1) | 2 | **No afectado** en cat. 1-3; **categoría 4** | Sin PHP en producto ni en el anfitrión. Apps de usuario con PHP por debajo del fix (WordPress, Nextcloud, NetAlertX en la instalación de referencia) → aviso a administradores en las release notes |
| Arcane — 96627 / 96628 | 2 | **Mitigado** / **No afectado** | Producto ausente; el control equivalente destapó F-10 (cambio de rol sin revocar sesiones), corregido. Sin pprof; `/v1/sys/debug` exige admin y principal real |
| Nginx UI — 84321 / 84360 | 2 | **Mitigado** / **Mitigado** | Producto ausente; controles equivalentes destaparon F-12 (`id_token` aceptado como access token) y F-14 (replay de TOTP), corregidos; R-02 documentado |
| SFTPGo — 103028 / 77375 | 2 | **No afectado hoy, patrón latente** / **No afectado** | Sin servidor SSH/SFTP propio; recorridos recursivos autorizan sólo el nivel superior (R-05), no explotable mientras DENY/grants no tengan llamadores |
| FrankenPHP — 82416 · CoreDNS — 62994 | 2 | **No aplica** | Ausentes en repo, catálogo e instalación de referencia |

Incidental fuera de los 23: GO-2026-6597 (containerd, amplificación de índice OCI) en app-management a nivel módulo, no alcanzable; documentado como pendiente de la pila containerd/docker (§7).

## 5. Hallazgos propios corregidos en 26.10.1 (F-01..F-17, por severidad)

Cada uno con test de regresión rojo→verde y la suite del módulo en verde.

| ID | Sev. | Hallazgo (una línea) | Cambio visible |
|---|---|---|---|
| F-01 | **Crítica** | Avatar: `PUT /v1/users/current {"avatar":"<ruta>"}` + `GET /v1/users/avatar` servía cualquier fichero del host como root (clave JWT incluida) a cualquier cuenta con sesión, guest incluido | Ninguno para usuarios legítimos |
| F-02 | **Crítica** | El switch de disco del sistema dejaba leer la clave JWT y la BD de usuarios dentro del techo AppData, y no retenía el secreto interno entre servicios ni credenciales de herramientas del host | Esos ficheros siguen listables pero ya no legibles con el switch |
| F-03 | Alta | `GET /v3/file` (handler con `?token=`) no consultaba la autoridad de almacenamiento y seguía symlinks fuera de `/DATA` | Un usuario sin permiso sobre una ruta recibe 403 donde antes recibía el fichero |
| F-04 | Alta | La purga de la papelera seguía un `.Trash-casanova/files` plantado como symlink y borraba fuera de la papelera | Una raíz de papelera con symlink se salta y se registra. El bypass por nombre `..` hallado en la revisión se cerró con F-17 |
| F-05 | Alta | Extraer un archivo seguía un symlink de directorio plantado en el destino | La extracción responde 400 ante un symlink intermedio |
| F-06 | Media-alta | Sin tope de cuerpo en `/v1/users/login` y rutas pre-auth: un POST de GB tumbaba UserService | Cuerpos > 1 MiB responden 413 en esas rutas; uploads exentos |
| F-07 | Media | Enlaces públicos de carpeta servían a internet symlinks hacia carpetas hermanas y ficheros sustituidos por symlink | Un share cuyo origen fue reemplazado por symlink responde 404 |
| F-08 | Media | `POST /v2/search/file` listaba nombres, tamaños y fechas de todas las áreas a cualquier `user` | La búsqueda de un no-admin devuelve sólo lo que puede listar (vacío si no tiene grants) |
| F-09 | Media | WebSocket de eventos del message bus leía frames de cualquier tamaño a memoria | Ninguno (la UI no envía datos por ese socket) |
| F-10 | Media | Cambio de rol no revocaba sesiones: un admin degradado seguía siendo admin ~3 h | Quien cambia de rol debe volver a iniciar sesión |
| F-11 | Media | `POST /v1/file` a través de un symlink colgante creaba el fichero fuera del techo | Crear sobre un symlink colgante responde "file or dir exists" |
| F-12 | Baja-media | El `id_token` OIDC era aceptado como access token de id 0, fuera de toda revocación | Ninguno para clientes OIDC correctos |
| F-13 | Baja-media | El descargador de tiendas de apps aceptaba redirecciones https→http | Una tienda https que redirige a http deja de descargarse |
| F-14 | Baja | El mismo código TOTP valía varias veces en su ventana (~90 s) | El segundo uso de un código en la ventana se rechaza; un reinicio reabre una ventana de ~90 s |
| F-15 | Baja | El 502 del gateway devolvía la dirección interna del backend | Cuerpo genérico "Backend unavailable" |
| F-16 | Baja-media | Descarga multi-elemento seguía symlinks hacia hermanos no seleccionados bajo el prefijo común | Ninguno para selecciones sin symlinks |
| F-17 | Alta | Papelera: una entrada de `info/` llamada `...trashinfo` (o `..trashinfo`, `.trashinfo`) daba el nombre `..`/`.`/vacío y `Delete`/`Empty`/`Purge` borraban la papelera entera del disco; `Restore` movía la papelera completa al destino | Ninguno: esas entradas dejan de listarse |
| F-D1 | Baja (defensa en profundidad) | `GetDockerNetworks` y `AppUsageList` sin `requirePerm` explícito (inalcanzable sin JWT, pero rompía el patrón del fichero) | Ninguno |

**Instalaciones 26.10**: F-01 y F-02 están presentes en todo 26.10; actualizar a 26.10.1 los cierra. En la instalación de referencia, la prueba autenticada de F-01 tras actualizar dio el resultado esperado (no se sirve contenido fuera del directorio del usuario).

## 6. Riesgos confirmados sin fix en 26.10.1 (R-*)

| ID | Sev. | Riesgo | Decisión 26.10.1 | 27.04 |
|---|---|---|---|---|
| R-01 | Media | La lista blanca global de IPs exime loopback y todo el tráfico de un túnel llega por loopback: lista blanca "solo LAN" + túnel sigue expuesta a internet | Documentado en `RELEASE_NOTES_26.10.1.md` (Known risks), sin cambio de comportamiento. Operador: con túnel, usar las políticas de acceso del proveedor (p. ej. Cloudflare Access) y no la lista blanca global como frontera | La excepción de loopback valdrá sólo sin cabeceras de reenvío |
| R-02 | Baja (diseño) | Logout revoca la familia de refresh; el access token vive hasta expirar (≤3 h) | Documentado | Revocar el jti del access token en logout |
| R-03 | Baja | Autorización por claim `role` sin revalidar contra BD (tras F-10 sólo queda el caso teórico de cambios fuera de `PutUserRole`) | Documentado | Rediseño de validación de claims |
| R-04 | Baja-media | Se aceptan tiendas `http://` al registrarlas (admin; las dos por defecto son https); rechazarlas rompería instalaciones existentes | Documentado; F-13 cierra la redirección https→http | Aviso en UI / migración |
| R-05 | Media (latente) | Recorridos recursivos (comprimir, descargar como archivo, copia recursiva, búsqueda) autorizan sólo el nivel superior | No explotable: DENY/grants sin llamadores en producción | Autorizar por entrada antes de exponer DENY/grants |
| R-06 | Media (latente) | Un grant de solo lectura autorizaría escrituras (`g.Ops&op == 0`) | Ídem (sin grants en producción) | Exigir `g.Ops&op == op` |
| R-07 | Baja-media | Puntos de montaje/merge validados léxicamente: un symlink plantado permite tapar `/etc` (acción de admin) | Documentado | `EvalSymlinks` antes de montar |
| R-09 | Baja | WS de consola VNC y terminal SSH sin `SetReadLimit` (ambos exigen sesión) | Pendiente de decisión: un tope de ~8 MiB / ~1 MiB cortaría pegados grandes | Según decisión |
| R-10 | Baja | `POST /v1/sys/2fa/validate` sin gate de permiso; bloqueo por IP que vía túnel es siempre loopback | Sospechado, sin test | Gate de admin + bloqueo por cuenta |

Sospechados sin test (S-01..S-11: papelera sin aislamiento por usuario, shares Samba sin storageauth, TOCTOU por ruta, enlaces duros, código OAuth2 tras cambio de contraseña, throttle de login por `RealIP`, goroutines sin `recover`, `exec` sin contexto, UPS por http, script de actualización sin `--proto-redir`, `PutUserImage`): ninguno se presenta como corregido ni como vulnerabilidad confirmada.

## 7. Hallazgo transversal: dependencias y stdlib (govulncheck sobre los binarios reales)

`govulncheck -mode=binary` sobre los 10 binarios del paquete 26.10 reporta entre **91 (cli) y 145 (local-storage) vulnerabilidades con símbolo alcanzable por binario, 210 avisos distintos en total**. La mayoría es stdlib go1.24.2 (`crypto/tls`, `crypto/x509`, `net/http`, `net/url`, `archive/*`, `encoding/asn1`, `html/template`, `os`…); **el bump a go1.26.10 cierra toda la stdlib** (verificado sobre los binarios recompilados: 0 hallazgos de stdlib). El resto son módulos de terceros antiguos (`golang.org/x/crypto` 0.23, `x/net` 0.25, echo 4.12, jwt/v4 4.5.0, websocket 1.5.0, kin-openapi, klauspost, xz, go-getter 1.7.0, rclone 1.62, docker 23/28, containerd lib 2.1.4…), con módulos sin fix publicado (`golang-jwt/jwt` v3, `mholt/archiver/v3`, `rardecode`, `go-smb2`, `aws-sdk-go` v1, varios de `docker/docker` y `otel`).

- **Decisión de alcance**: 26.10.1 lleva el toolchain y los bumps **PATCH/MINOR compatibles** con vulnerabilidad alcanzable; los bumps mayores y las sustituciones de módulos sin fix van a 27.04 o a un 26.10.x posterior.
- **Aplicado** (detalle en `MAINTENANCE_CHANGELOG_26.10.1.md`, "Dependencies"): x/net v0.60.0, x/crypto v0.57.0, x/text v0.42.0 (los 11 módulos); x/image v0.45.0; golang-jwt/jwt/v4 v4.5.2; gorilla/websocket v1.5.3; ulikunitz/xz v0.5.15; grpc v1.83.1 y go-jose/v4 v4.1.4 (AppManagement); protobuf v1.33.0 (LocalStorage). Los `go.mod` pasan a `go 1.26.0` con `godebug default=go1.24` para conservar el comportamiento de runtime de 26.10. Vulnerabilidades alcanzables por módulo (modo fuente), antes → después: Common 23→4, Gateway 15→5, MessageBus 17→6, UserService 16→6, LocalStorage 58→27, JobManagement 17→7, AppManagement 60→30, Driver 10→1, Virt 10→1, CasaNova 34→7, CLI 10→9. En binario: stdlib 48–63 → 0 en todos.
- **Riesgo aceptado para 26.10.1**: `labstack/echo/v4` v4.12.0, GO-2026-6293 / CVE-2026-55677 en Gateway. El fix (4.15.x) elimina `middleware.JWTWithConfig`, la capa de autenticación de 9 servicios; la precondición del aviso (grupo protegido + raíz estática más amplia) no se da en el único `StaticFS` (`CasaNova-Gateway/route/static_route.go:126`, UI pública). Va a 27.04 con la migración a `echo-jwt`.
- **Diferido a 27.04 / 26.10.x**: kin-openapi, golang-jwt/jwt v3 y archiver/rardecode (sin fix), rclone, pila containerd/docker (incluido GO-2026-6597), azidentity/ntlmssp/otel/go-smb2, x/crypto/openpgp (sin fix).
- **Build**: `make frontend` exige ahora Node ≥22.23.4 / 24.22.1 / 26.12.1 (`check-node`). El paquete se construye además con entradas que aún no están en el repositorio (`build/full/`: instalador, actualizador, catálogo), tomadas sin cambios del paquete 26.10.

## 8. Revisión independiente

| Revisión | Resultado |
|---|---|
| Filas Node/Go, revisadas por un segundo auditor | 12/12 confirmadas con evidencia propia (govulncheck reproducido, boletín y vulndb re-consultados, tarball re-extraído); una corrección menor de conteo (10 units systemd, no 11); incidental GO-2026-6597 |
| Filas containerd/PHP/relacionados, revisadas por un segundo auditor | 8 confirmadas, 4 corregidas en evidencia (prueba de no importación por pclntab; tres contenedores PHP vulnerables en la instalación de referencia, no uno; endpoint `/v1/sys/debug`; rutas OAuth2/refresh que emiten tokens), 0 indeterminadas; ningún estado cambió |
| F-01 (avatar), reproducido aparte | 3 tests rojos en código viejo, 5/5 verdes con fix, verificado antes de integrar |
| Los 17 fixes, revisión adversarial (dos lotes + re-revisión) | **17/17 confirmados** con rojo→verde propio y búsqueda de bypass. En el primer lote encontró un bypass de F-04 (nombre de entrada `..` desde `...trashinfo`, con test rojo); corregido como F-17 y re-revisado: **F-04 cerrado**, incluida la variante de `Restore`. Dos observaciones no bloqueantes: sin tope de cuerpo en rutas autenticadas de otros módulos y una nota secundaria sobre tiendas |

## 9. Estado respecto a los criterios de aceptación

| Criterio | Estado |
|---|---|
| 23 CVE clasificados con evidencia verificable | Hecho (`CVE_MATRIX_26.10.1.md`), con revisión cruzada de todas las filas |
| Aplicables corregidos o documentados como riesgo | Go: mitigado. F-01..F-17 + F-D1 corregidos; R-01..R-10 documentados; echo GO-2026-6293 como riesgo aceptado (§7) |
| Compila; tests corridos | Hecho. `REGRESSION_RESULTS_26.10.1.md`: 3 rondas de `go mod verify` + `go vet` + `go test -count=1` en los 11 módulos, todas verdes. Paquete final: checksums internos OK, go1.26.10 ×10, marcador de la denylist GO-2026-6809 ×10, `make test-package` OK, la UI muestra 26.10.1 |
| Fixes presentes en 27.04 | Portados a la línea 27.04 (F-01..F-17, R-01, F-D1, toolchain, dependencias y `check-node`), pendientes de integrar en su release |
| Docs completas | Este documento + `MAINTENANCE_CHANGELOG_26.10.1.md` + `RELEASE_NOTES_26.10.1.md` + `CVE_MATRIX_26.10.1.md` + `REGRESSION_RESULTS_26.10.1.md` + `PRODUCTION_VALIDATION_26.10.1.md` |
| Instalación de referencia sin alterar hasta la validación | Cumplido: sólo lecturas hasta la instalación autorizada |
