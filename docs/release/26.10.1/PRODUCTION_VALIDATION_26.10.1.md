# CasaNova 26.10.1 — Validación en producción

*2026-10-24 · Instalación de referencia "prod" (x86_64, Ubuntu 24.04.5), en uso real con 30 contenedores de apps. Sin secretos ni direcciones: el host se nombra "prod". La instalación se autorizó explícitamente antes de sustituir servicios.*

## 1. Paquete

**Build final instalado: `18.6.3.beta1.2410261443` (§9).** Esta sección y las §2-§7 describen la primera instalación, con el build `18.6.3.beta1.2410260213`; las reinstalaciones con el arreglo de la UI móvil están en §9.

| Campo | Valor |
|---|---|
| Archivo | `casanova-18.6.3-RELEASE-26.10.1-20261024-x86_64.tar.gz` (323 MB) |
| sha256 | `d05d00bf3ee3f04ee9bec84951fc5add848eda0455b186299b029ed83149eb9b` |
| Build | `18.6.3.beta1.2410260213`, display `26.10.1`, canal `beta`, built_at 2026-10-24T05:14:51Z |
| Verificación local | checksums internos OK; go1.26.10 ×10; denylist GO-2026-6809 ×10; `make test-package` OK; UI muestra 26.10.1 |

## 2. Preparación

- Versión previa `18.6.2.beta1.0310261227` (26.10, beta); 10 unidades casanova activas.
- Disco: 350 GB libres en `/`; RAM y carga normales.
- Mecanismo de actualización: `/usr/share/casanova/tools/casanova-update.sh` (backup verificado por censo + rollback automático).
- Rollback previo comprobado: último backup con censo 21/21 ficheros con sha256 correcto.
- Línea base: 27/30 contenedores en ejecución, gateway 200.

## 3. Transferencia

- `scp` (SSH, cifrado); sha256 idéntico en origen y destino.
- `casanova-update.sh --verify-only`: exit 0 (10 binarios, 246 ficheros de frontend, 10 unidades).

## 4. Instalación

- Comando: `sudo bash /usr/share/casanova/tools/casanova-update.sh <paquete>` → **exit 0**, 2026-10-24 15:09Z.
- Backup creado en `/var/lib/casanova/backups/upgrade-<fecha>`.
- Rollback disponible: `sudo bash casaos-to-casanova.sh --rollback-upgrade`. No fue necesario.

## 5. Pruebas funcionales — verificación independiente del informe del actualizador

| Prueba | Resultado |
|---|---|
| `manifest.json` | `18.6.3.beta1.2410260213` / `26.10.1` / `beta` |
| sha256 de los 10 binarios en `/usr/bin` vs paquete | 10/10 idénticos |
| Unidades | 10 activas, 0 failed, NRestarts=0 (binarios nuevos en ejecución) |
| Contenedores | 27/30 antes → 27/30 después |
| Gateway / frontend | 200; `AboutSettings` desplegado contiene `26.10.1` |
| journal casanova* desde la instalación | 0 panic / fatal |

## 6. Pruebas de seguridad no destructivas

Ejecutadas en el propio host contra el gateway (`localhost:85`). Sin fuerza bruta ni explotación.

| Prueba | Esperado | Resultado |
|---|---|---|
| POST `/v1/users/login` con cuerpo de 2,2 MB | 413 (F-06) | **413** |
| POST `/v1/users/login` `{}` | 4xx | 400 |
| GET avatar con `path=` a la clave JWT, sin sesión | 401 | 401 |
| Igual con token falso | 401 | 401 |
| `/v1/sys/debug`, `/v3/file`, `/v2/local_storage/search`, `/v1/users/current` sin sesión | 401 | 401 ×4 |
| Denylist x509 GO-2026-6809 en binarios instalados | 1 por binario | 10/10 |
| Avatar con sesión válida (F-01): `PUT /v1/users/current {"avatar":"<ruta de la clave JWT>"}` y luego `GET /v1/users/avatar` | ruta ignorada, sin contenido | **OK** — el PUT responde 200 pero el avatar guardado sigue vacío (la ruta se ignora); el GET responde 404, sin contenido de la clave. Verificado por el mantenedor reproduciendo la ruta del ataque con una sesión real (§9) |

R-01 (lista blanca de loopback + túnel) se documenta sin cambio en 26.10.1, por eso no se prueba aquí.

## 7. Estabilidad

Observación de 30 min, muestra cada 5 min:

| UTC | failed | restarts | contenedores | gw | panics | load | RSS 5 servicios |
|---|---|---|---|---|---|---|---|
| 15:16 | 0 | 0 | 27 | 200 | 0 | 0.03 | 279 MB |
| 15:21 | 0 | 0 | 27 | 200 | 0 | 0.28 | 286 MB |
| 15:26 | 0 | 0 | 27 | 200 | 0 | 0.32 | 279 MB |
| 15:31 | 0 | 0 | 27 | 200 | 0 | 0.10 | 285 MB |
| 15:36 | 0 | 0 | 27 | 200 | 0 | 0.22 | 279 MB |
| 15:41 | 0 | 0 | 27 | 200 | 0 | 0.17 | 285 MB |

Memoria estable, sin reinicios ni errores.

## 8. Datos

No se tocaron `/var/lib/casaos`, `/var/lib/casanova/*.json`, `/etc/casanova`, contenedores ni volúmenes. El actualizador sólo reemplaza binarios, frontend y unidades systemd.

## 9. Reinstalaciones con el arreglo de la UI móvil y versión final

Tras la primera instalación, la UI resultó inutilizable en un teléfono (iPhone 13). El arreglo es sólo de frontend (`CasaNova-UI-v2`) y se integró en dos rondas; cada una se empaquetó e instaló con el mismo actualizador.

| Ronda | Build | sha256 del paquete | Estado |
|---|---|---|---|
| Primera instalación (§1-§7) | `18.6.3.beta1.2410260213` | `d05d00bf3ee3f04ee9bec84951fc5add848eda0455b186299b029ed83149eb9b` | Sustituido |
| Móvil, ronda 1 | `18.6.3.beta1.2410261427` | `757fc1177059094a1a44e718b51ed1c8d4af77fa250915ee0c1288958929cfe4` | Sustituido |
| **Móvil, ronda 2 — final** | **`18.6.3.beta1.2410261443`** (built_at 2026-10-24T17:45:08Z) | **`300cf73a1d1c623daac201dfe6d5f6b6fefa9b0c2c6ff8b38c7372764e4314a8`** | **Instalado** |

Los tres paquetes tienen el mismo nombre de archivo: distinguirlos siempre por build o sha256.

- **Instalación final**: 2026-10-24 ~17:47Z, con backup automático. Rollback disponible con `casaos-to-casanova.sh --rollback-upgrade`; no fue necesario.
- **Contenido**: entre el primer build y el final cambia sólo el frontend; los binarios Go se compilan de la misma fuente.
- **Prueba móvil** (Playwright, perfil iPhone 13, 390 px, sesión real): `scrollWidth == clientWidth` (390) en **12/12 rutas**; menú "Más" de la barra superior, Ajustes lista → detalle con vuelta atrás y "Ubicaciones" en Archivos funcionan. Tras la ronda 2, con datos reales: Contenedores (filas de apps compose sin solape), Usuarios, General y Actualizaciones ("Revertir" dentro de la tarjeta) OK. En escritorio (1440 px) la UI no cambia.
- **Avatar autenticado (F-01)**: el mantenedor reprodujo la ruta del ataque con una sesión real, apuntando el avatar a la clave JWT real (bajo el `DBPath` del user service): `PUT /v1/users/current` → 200 con el avatar guardado vacío (ruta ignorada) y `GET /v1/users/avatar` → 404, sin contenido de la clave.
