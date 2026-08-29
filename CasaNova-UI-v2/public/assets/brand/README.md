# CasaNova brand assets

These are the first-pass illustrative icons for the CasaNova UI, copied
from the user's working set in `CasaNova-UI/resources/`. They're used
as the brand mark, login avatar, and widget illustrations.

## Files

| File | Use |
|---|---|
| `casanova.png` | Primary brand mark: house with glowing orb. Used in TopBar wordmark + Login avatar |
| `casanova2.png` | Secondary brand mark: galaxy swirl. Reserved for future use (loading screens, splash) |
| `server.png` | Ornate lantern-style server. Planned use: system state widget hero illustration |
| `library.png` | Glowing bookshelf cabinet. Planned use: file manager / docs section hero |
| `storage1.png` | Drive with folder + HDD on clouds (small) |
| `storage2.png` | Flat drive on clouds (mid) |
| `storage3.png` | Isometric drive on clouds — **primary Storage widget icon** |
| `storage4.png` | Folder with papers on clouds |
| `windows.png` | Window frame showing a sky. Reserved for Windows VM tiles when VMs land |

## Known technical debt

These files are **not production-ready as-is**. They're copied verbatim
from the source pack at 512×512–1024×1024 and weigh 128KB–2MB each
(~8MB total). Before v1.0 we need to:

1. **Resize to multiple widths** — 80, 128, 256, 512 px variants per asset
2. **Convert to WebP** (with PNG fallback for older browsers) — 50-70% size reduction
3. **Serve responsive `srcset`** in the components that display them so mobile doesn't download 1024px originals
4. **Optionally AVIF** for modern browsers — another 20-30% reduction on top of WebP

The current integration is a **dev-time test** to validate the visual
direction. Once we decide the illustration style is final, we'll run
them through a Sharp-based optimization script as a one-time pass and
commit the optimized outputs.

## Attribution

Created by the CasaNova project for Leeway Open Software. The style is
illustrative fantasy (reminiscent of game UI art), chosen to differentiate
CasaNova from the flat-icon aesthetic that dominates home-server UIs.
