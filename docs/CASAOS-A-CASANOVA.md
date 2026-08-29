# De CasaOS a CasaNova — Qué cambió y por qué

> **Documento tentativo / vivo.** Explica en qué se diferencia CasaNova de su
> base CasaOS (IceWhale), qué buscamos con el fork, y por qué se tomaron
> ciertas decisiones. No es un changelog exhaustivo; es el "para qué".

---

## 0. En una frase

CasaNova es un fork endurecido y modernizado de CasaOS: mismo linaje
(microservicios Go + UI Vue), pero con **la seguridad reparada de raíz**, una
**UI rediseñada**, **features de plataforma que CasaOS no tiene** (VMs, drivers,
multi-store), **compatibilidad con los clientes de ZimaOS**, y un rumbo
deliberado hacia la **cadencia de plataforma moderna** (Docker/containerd/
cgroup v2, runtime-agnóstico). No es cosmético: es corregir, modernizar y
adelantarse.

## 1. Por qué no es "CasaOS con otro logo"

Tres razones de fondo:

1. **Seguridad.** CasaOS arrastra una clase de vulnerabilidad crítica y
   sistémica (ver §3). No era un bug puntual: era un patrón repetido en todo el
   stack. Repararlo bien exige tocar la capa de autenticación de cada servicio,
   no maquillar.
2. **Legado técnico.** Baselines viejos (Docker 20), scripts con phone-homes a
   infra de IceWhale, un self-updater que podía tumbar la caja, dependencias
   deprecadas, y restos no estándar (comentarios/strings dejados por el
   desarrollo original). Todo eso se sanea, no se hereda.
3. **Rumbo propio.** Queremos adelantarnos a la cadencia de la plataforma
   (contenedores rootless/Podman, containerd v2, cgroup v2), no quedar clavados
   donde CasaOS quedó.

## 2. Política de nombres (por qué hay strings `zima*`/`icewhale*`)

**Regla:** nuestro código es genérico o `casanova-*`. Los únicos strings
`zima…`/`icewhale…` que se envían son los que un cliente **literalmente exige
en el protocolo** (paths de endpoints y nombres de campos JSON que el cliente
parsea). Todo lo interno lleva nombre propio.

- **Por qué se mantienen algunos:** son contrato de cable, no branding. Ej:
  `/v2/zimaos/device/info`, el campo `icewhale_settings` en las tarjetas YSK,
  `/modules/icewhale…`. Si los renombramos, el cliente sin modificar deja de
  funcionar. La verdad-terreno sale de decompilar los binarios cliente en
  `GIFTABLES/`, no de docs.
- **Por qué el resto se renombró:** evitar problemas legales con IceWhale y
  dejar de arrastrar su marca. Ej: `RANW_NAME` "IceWhale-RemoteAccess" →
  "CasaNova-RemoteAccess".

## 3. El gran arreglo: la clase de bypass de autenticación por loopback

**El problema (heredado de CasaOS):** el gateway proxya *toda* petición del
browser a los backends por `127.0.0.1`. Entonces `IsLoopback(ctx)` es **siempre
true** para tráfico real. Cualquier guard de auth/RBAC con un atajo
`if loopback { return allow }` es, por diseño, **bypasseable por cualquiera en
la LAN** → auto-promoverse a admin, RCE root sin auth, enumerar archivos, etc.
No era un bug: era el patrón por defecto en múltiples servicios.

**El arreglo correcto (patrón de 3 partes, aplicado a todo el stack):**
1. El *skipper* del JWT saltea SOLO loopback **sin token** (no todo loopback),
   si no las peticiones del browser (loopback pero *con* token) se saltean el
   JWT y el rol nunca se puebla.
2. *Bearer-strip* en el token-lookup (echo no quita el prefijo `Bearer ` para
   lookups custom → `jwt.Validate` recibía `"Bearer <jwt>"` y fallaba siempre).
3. El guard por endpoint usa `rbac.Allows(RoleFromContext, perm)` **sin confiar
   en loopback**. Un atacante tokenless cae con rol vacío → denegado; el token
   admin de la UI → permitido.

**Alcance:** cerrado en los 5 servicios de cara al usuario + 2 de infra
(gateway whitelist global que nunca se llamaba; rebind DoS del puerto) +
MessageBus (que ya usaba un socket unix 0600 para distinguir publishers
internos — ese diseño se respetó, no se le aplicó el patrón). El *search* ahora
**exige login** (no solo permiso), porque el rol guest tiene `PermFileRead` y un
chequeo de permiso solo dejaba enumerar nombres de archivo desde la LAN.

**Por qué importa la decisión:** preservamos los llamadores internos legítimos
(3 verificados, vía un helper `requirePermLoopbackOK` acotado) en vez de romper
todo. Evidencia antes que blanket-changes.

## 4. Features de plataforma que CasaOS no tiene

Construidas con la imagen de ZimaOS 1.6.2 como oráculo (disassemble), pero con
implementación propia:

- **CasaNova-Virt** — gestión de VMs KVM/QEMU. Como compilamos `CGO_ENABLED=0`,
  usamos el `go-libvirt` **puro-Go** (RPC sobre el socket de libvirt) en vez de
  los bindings CGO que usa ZimaOS. Base `/v2/virtualization`.
- **CasaNova-Driver** — gestión de hardware/drivers vía sysfs/proc + modprobe.
  PCI/USB enum, load/unload de módulos, GPU.
- **Multi-store de apps** — no quedás atado al store de IceWhale. UI en
  *Ajustes → Fuentes de apps* para agregar/priorizar/borrar catálogos. El store
  de CasaOS queda como default de compat (protegido), pero podés apuntar al tuyo
  y cortar la infra de IceWhale por completo.

## 5. Compatibilidad con clientes ZimaOS (interop, no rebranding)

Objetivo: que las apps **Windows/Android de ZimaOS sin modificar** puedan
hablarle a un server CasaNova mientras construimos las nuestras.

- **Android** conecta por IP a un gate TCP en **:9527** que devuelve un
  `DeviceInfo` **plano** (sin envelope). Trampa clave: el campo `ip4` está
  tipado como lista de *objetos* — mandarlo como lista de strings rompe el
  parser Gson → se **omite** `ip4`.
- **Windows** hace **discovery por UDP broadcast** en :9527 (no mDNS, como se
  creía). Reverse-engineered del `.exe` (Electron) cruzado con el smali del
  APK. Nuestro responder UDP contesta cualquier datagrama con el DeviceInfo +
  el eco `request_ip` (sin el cual el cliente descarta la respuesta). Resultado:
  una caja CasaNova **auto-aparece** en el cliente Windows.
- **Remoto** (ZeroTier/`*.zimaspace.com`) es infra propietaria → diferido; foco
  LAN-first.

## 6. Robustez operativa (cosas que en CasaOS te tumbaban la caja)

- **Crash-loop de runtime dir:** reiniciar el servicio principal borraba
  `/var/run/casaos/` y crasheaba todo el stack (RuntimeDirectory compartido con
  `Preserve=no`). Fix: `RuntimeDirectoryPreserve=yes` en las 7 units.
- **Self-updater que dejaba el server offline:** tocar "Instalar" paraba los 9
  servicios y el script moría antes de rearrancarlos (SIGPIPE por heredar el
  stdout de un servicio que se estaba matando; después, un audit encontró 5
  bugs más en el path de update/rollback/migración). Ver el
  [BUG-LEDGER](BUG-LEDGER.md). Ahora el update/rollback/migración se
  auto-recupera ante cualquier fallo, con ERR traps y `systemd-run --scope`.

## 7. Modernización de plataforma (adelantarse a la cadencia)

- **Baseline de Docker: 20 → 24.** 24.0 es donde cgroup v2 + driver systemd +
  overlay2 son los defaults estables. La plataforma ya es cgroup-v2-only
  (Debian 12, Ubuntu 22.04+, Fedora). El check de versión crudo (`:0:2`, que se
  rompía con majors de 1 dígito) se endureció.
- **Instalar Docker si falta**, del **repo apt oficial con el codename
  detectado en vivo** (no hardcodeado). Esto cierra un footgun real: un
  `do-release-upgrade` renombra los repos de terceros a `*.distUpgrade` (los
  apaga), y un codename stale (ej. `jammy` en una caja `noble`) **congela Docker
  en silencio**. El installer ahora detecta y limpia ese caso.
- **Runtime-agnóstico (rumbo).** Ya hablamos al runtime vía `client.FromEnv`
  (socket `DOCKER_HOST`) con negociación de API → **Podman rootless funciona
  apuntando el socket, casi sin tocar código**. Lo único que ata a Docker es el
  `Requires=docker.service` de systemd. Dirección: relajarlo a `Wants=` +
  detección de runtime genérico.
- **Sanitización:** eliminado el phone-home `get.casaos.io | bash` (que además
  era RCE-a-root), 258 comentarios/prints CJK, 5 chars invisibles, y strings de
  marca. Se conservaron los contratos de cable y los catálogos i18n (como
  language packs). Auditoría Unicode/Trojan-Source: limpia.

## 8. Rediseño de la UI (Vue 3 + PrimeVue + Tailwind)

CasaOS tenía una UI funcional pero anticuada. CasaNova rehízo:

- **Dashboard** con datos de hardware completos (motherboard, NICs con IP/MAC/
  velocidad, puertos físicos, uptime).
- **File Manager estilo Windows 11**: marco de ventana anclado (el chrome —
  tabs, breadcrumb, toolbar, status bar — fijo a los bordes; solo el contenido
  scrollea), ancho completo, controles en una línea, y un HUD de notificaciones
  en la tira superior.
- **Ajustes reorganizados**: sección **Apariencia** propia (tema, acento, packs
  de iconos, estilo de app-icons, fondos), dejando General con idioma, buscador,
  consejos, modos e info del sistema.
- **Sistema de iconos**: packs seleccionables (lucide/phosphor), estilo de
  file-icons **Clásico** (carpeta amarilla tradicional rellena, estilo
  ZimaOS/CasaOS) vs Línea, acento + glow opcionales, y app-icons con estilo
  Mosaico o Tradicional (transparente). Se mataron los overlays "playdoh".
- **Fondo de login per-device** que sobrevive al logout (antes se borraba con
  las prefs al desloguear, y el login solo se ve deslogueado → nunca cambiaba).

## 9. Versionado

- **Versión de display** (calendario, estilo Ubuntu): `26.07`, cableada desde
  una sola fuente (Makefile `DISPLAY_VERSION`), no hardcodeada.
- **Versión de build:** `18.5.0.dev1.<timestamp>` inyectada por ldflags.
- CasaOS usaba `0.1.0` / `26.04`; el salto refleja el trabajo acumulado y
  desacopla nuestro versionado del upstream.

---

## Principios de decisión (el "por qué" detrás del "qué")

1. **Runtime = éxito.** Un build verde no es éxito; el comportamiento visible
   funcionando en la topología real, sí.
2. **Evidencia antes que culpa.** No se rompe algo externo hasta probar cada
   capa; no se hacen blanket-changes sin verificar los llamadores internos.
3. **Interop, no apropiación.** Compat con clientes ZimaOS para aprender y dar
   continuidad, con nombre e implementación propios.
4. **Adelantarse, no arrastrar.** Cada decisión de plataforma mira la cadencia
   futura (rootless, containerd v2, cgroup v2), no el status quo de CasaOS.
5. **Sanear en vez de heredar.** Phone-homes, restos de marca y footguns se
   remueven, no se toleran.
