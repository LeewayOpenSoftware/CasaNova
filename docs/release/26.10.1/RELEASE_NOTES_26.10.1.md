# CasaNova 18.6.3 — RELEASE 26.10.1

**Version:** 26.10.1 (build `18.6.3.beta1.2410261443`, channel beta) · **Code freeze:** 2026-10-24 · **Tests:** 2026-10-24 (all 11 modules green; package checks passed)

By **Leeway Open Software**, in collaboration with Claude and GPT. A fork of CasaOS by IceWhale Technology (Apache 2.0).

26.10.1 is a **security and maintenance update** for CasaNova 26.10. No new features, no data migrations. It is the result of a directed audit of the October 2026 security bulletins (Node.js, Go, containerd, PHP) and of CasaNova's own file, session and gateway code.

## What is fixed

**Build toolchain.** Every CasaNova binary is now built with Go 1.26.10. This closes the October Go advisory in the certificate verifier (CVE-2026-111943) and every other standard-library advisory that applied to the Go 1.24 build of 26.10. Installing a newer Go on your server does nothing on its own; the update ships rebuilt binaries.

**Access to files and credentials.** Several ways in which a signed-in account could read files it was not entitled to, or make the server write outside its storage, are closed:

- the user avatar setting and the direct file endpoint used by downloads;
- the per-user "system disk" switch, which now withholds the appliance's own keys, databases and host-tool credentials even inside the application data directory;
- public share links, multi-file downloads, archive extraction, file creation and the trash, which no longer follow symbolic links placed inside storage by another process;
- crafted entries in a disk's trash folder, which could make deleting, emptying or the daily cleanup erase that disk's whole trash.

**Sessions and two-factor authentication.** Changing a user's role now ends that user's existing sessions. A one-time code can no longer be reused within its validity window. A token type issued for single-sign-on clients can no longer be presented as a regular session token.

**Dependencies.** Go libraries with a known, reachable vulnerability were moved to their patched patch or minor release (networking, cryptography, text, image decoding, tokens, WebSockets, compression, gRPC). The web interface is now built only with a patched Node.js release. Larger library upgrades are planned for 27.04.

**Phones.** The web interface is usable on a phone (tested at 390 px, iPhone 13). Pages no longer scroll sideways; the top bar folds Files, Notes, Apps, App Library, Terminal and Settings into a "More" menu; Settings opens as a list and each page has a way back to it; Files shows the file list first, with the sidebar behind a "Places" button; rows in Settings (containers, users, storage, general options, update report) stack instead of overlapping. The desktop layout is unchanged.

**Resilience.** Oversized request bodies to the sign-in endpoints and oversized frames on the event stream are rejected instead of exhausting memory. Gateway error pages no longer reveal internal addresses. The app-store downloader refuses to follow a redirect from HTTPS to plain HTTP.

The complete technical list is in `MAINTENANCE_CHANGELOG_26.10.1.md`; the audit itself is in `SECURITY_AUDIT_26.10.1.md` and `CVE_MATRIX_26.10.1.md`.

## Changes you may notice

- **You will be asked to sign in again** after an administrator changes your role.
- **A 2FA code works once.** Entering the same code twice within about 90 seconds is refused; wait for the next code.
- **Search shows only what you may open.** Non-administrators without storage permissions get an empty search result instead of other users' file names.
- **A 403 where a file used to download** means the account has no right to that path; this is the storage permission being enforced on the direct download endpoint as it already was elsewhere.
- **Trash folders that contain a symbolic link are skipped** by the automatic cleanup and noted in the log instead of being cleaned.
- **Creating a file where a dangling symbolic link exists** fails with "file or dir exists".
- **Extracting an archive stops** with an error if a symbolic link is found in the destination path.
- **Very large sign-in requests** (over 1 MiB) are rejected.
- **An app store that redirects from HTTPS to HTTP** stops downloading. Stores registered with an `http://` address keep working (see Known risks).
- Gateway "Backend unavailable" pages no longer show the backend address.

## Notice for administrators: third-party apps running Node.js or PHP

CasaNova itself does not ship or run Node.js or PHP: the server runs only Go services and serves a static web interface. The October 2026 Node.js and PHP security releases therefore do **not** affect CasaNova directly.

They do affect **apps you install**, because many of them bundle their own Node.js or PHP inside their container image. On a typical installation that includes, for example, **WordPress, Nextcloud and NetAlertX** (PHP) and **Nginx Proxy Manager, Gladys and Immich** (Node.js). Images published before late October 2026 carry the vulnerable runtimes.

What to do: open each app in CasaNova and **update it to the latest image**; for apps pinned to a specific version, move to a release that bundles PHP ≥ 8.2.35 / 8.3.36 / 8.4.27 / 8.5.12 or Node.js ≥ 22.23.4 / 24.22.1 / 26.12.1, as published by each app's maintainer.

Also note that the app catalogs configured by default are the community **IceWhale CasaOS-AppStore** and **BigBearCasaOS** stores. They are maintained by third parties; CasaNova does not review or pin the images they reference. You can change or add catalogs in `/etc/casanova/app-management.conf`.

## Known risks (documented, not changed in 26.10.1)

- **Global IP allow-list and tunnels.** The global allow-list exempts local connections, and traffic that reaches CasaNova through a Cloudflare tunnel arrives as a local connection. If you expose CasaNova through a tunnel, do not rely on the global IP allow-list as your only access control; use the tunnel provider's access policies (for example Cloudflare Access). The exemption will be removed in 27.04, with notice.
- **Signing out does not revoke the current access token immediately**; it stays valid until it expires (at most 3 hours). Refresh is revoked at once.
- **App stores registered over plain `http://`** are still accepted so existing installations keep working. Prefer `https://` catalogs.
- **Web framework advisory in the gateway (CVE-2026-55677).** The fix requires a major rewrite of the authentication layer and is planned for 27.04. The vulnerable condition (a protected route group serving a broader static directory) does not occur in CasaNova's gateway.
- **Console WebSockets (VNC and SSH terminal) have no frame size limit.** Both require a signed-in session. A limit is under consideration.

## Installing the update

The procedure is the same as for 26.10. Keep the package and its `.sha256` file in the same directory:

```
sha256sum -c casanova-18.6.3-RELEASE-26.10.1-20261024-x86_64.tar.gz.sha256
sudo bash /usr/share/casanova/tools/casanova-update.sh --verify-only casanova-18.6.3-RELEASE-26.10.1-20261024-x86_64.tar.gz
sudo bash /usr/share/casanova/tools/casanova-update.sh casanova-18.6.3-RELEASE-26.10.1-20261024-x86_64.tar.gz
```

A backup is taken automatically; roll back with `sudo bash casaos-to-casanova.sh --rollback-upgrade`. This package is an **update** for an existing CasaNova install, not a fresh installer.

## Files

| File | SHA-256 |
|---|---|
| `casanova-18.6.3-RELEASE-26.10.1-20261024-x86_64.tar.gz` | `300cf73a1d1c623daac201dfe6d5f6b6fefa9b0c2c6ff8b38c7372764e4314a8` |
