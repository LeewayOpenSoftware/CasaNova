package route

// casanova-search — a lazy global file search.
//
// It exposes the SAME API surface ZimaOS's search service does (base path
// /v2/search: POST /file, GET /stats, GET /ping) so a ZimaOS-style client/UI
// works against a CasaNova box, but WITHOUT the heavy ZimaOS stack (Manticore
// engine + BERT vector search + Tesseract/pdfium content extraction). v1 is a
// bounded, on-demand filesystem NAME search over the storage roots, reusing
// the same walk caps the v1 file API already uses.
//
// ponytail: no persistent index. Add a SQLite FTS5 content index (the sqlite
// driver in the stack already has FTS5) ONLY when the live walk is measurably
// too slow on a real library. `support_ai` is reported false; the semantic /
// content-extraction layer is deliberately out of scope for v1.

import (
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"github.com/labstack/echo/v4"
)

const (
	searchDefaultCount = 10
	searchMaxCount     = 500
	searchWalkMaxFiles = 100_000
	searchWalkTimeout  = 30 * time.Second
)

// searchStorageRoots confines every walk to real user-storage mounts — never
// the whole filesystem (which would enumerate /etc, /root, other homes).
var searchStorageRoots = []string{"/DATA", "/mnt", "/media"}

// hitSource mirrors the ZimaOS HitsObjectSource schema verbatim (the wire
// contract a ZimaOS-style consumer expects). is_ai/hl_content are present but
// empty in v1 (no semantic or content search yet).
type hitSource struct {
	Name      string `json:"name"`
	Dir       string `json:"dir"`
	IsDir     bool   `json:"is_dir"`
	IsAI      bool   `json:"is_ai"`
	Mtime     int64  `json:"mtime"`
	Size      int64  `json:"size"`
	Type      string `json:"type"`
	HlName    string `json:"hl_name"`
	HlContent string `json:"hl_content"`
}

type hitObject struct {
	ID     int64     `json:"_id"`
	Score  float64   `json:"_score"`
	Source hitSource `json:"_source"`
}

// registerCasaNovaSearch mounts the search routes on the v2 echo router.
// Called from InitV2Router.
func registerCasaNovaSearch(e *echo.Echo) {
	e.GET("/v2/search/ping", searchPing)
	e.GET("/v2/search/stats", searchStats)
	e.POST("/v2/search/file", searchFile)
}

func searchPing(c echo.Context) error {
	return c.JSON(http.StatusOK, echo.Map{"message": "ok", "data": "Pong"})
}

func searchStats(c echo.Context) error {
	// Live-walk backend is always "ok" — there's no index to be initializing.
	return c.JSON(http.StatusOK, echo.Map{
		"message": "ok",
		"data":    echo.Map{"status": "ok", "support_ai": false},
	})
}

func searchFile(c echo.Context) error {
	// File search requires an authenticated user. Walking the storage roots
	// enumerates every filename under /DATA,/mnt,/media, which is an
	// information leak to an unauthenticated LAN caller — and because the
	// gateway proxies LAN traffic over 127.0.0.1, a loopback allowance or the
	// guest-held PermFileRead alone would let a tokenless caller through.
	// UserIDFromContext is 0 unless the JWT middleware validated a real token,
	// so this fails closed for tokenless/guest-default callers. There is no
	// internal tokenless caller of the search API (UI-only), so requiring auth
	// breaks nothing.
	if commonmw.UserIDFromContext(c) == 0 {
		return c.JSON(http.StatusUnauthorized, echo.Map{"message": "authentication required"})
	}
	if !rbac.Allows(commonmw.RoleFromContext(c), rbac.PermFileRead) {
		return c.JSON(http.StatusForbidden, echo.Map{"message": "permission denied"})
	}

	var req struct {
		Keyword string `json:"keyword"`
		Count   int    `json:"count"`
		Cursor  string `json:"cursor"`
		Dir     string `json:"dir"`
		Opt     string `json:"opt"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, echo.Map{"message": "invalid request body"})
	}
	keyword := strings.TrimSpace(req.Keyword)
	if keyword == "" {
		return c.JSON(http.StatusBadRequest, echo.Map{"message": "keyword is required"})
	}
	count := req.Count
	if count <= 0 {
		count = searchDefaultCount
	}
	if count > searchMaxCount {
		count = searchMaxCount
	}

	roots, ok := resolveSearchRoots(req.Dir)
	if !ok {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"message": "dir must be under a storage path (/DATA, /mnt, /media)",
		})
	}
	// The storage authority decides what this account may list, as the v1
	// search does: PermFileRead alone let any user enumerate every other
	// user's names, sizes and mtimes. A named dir must be listable; with no
	// dir, each storage root is walked only if listable, otherwise just the
	// account's own grants beneath it.
	p := storageauth.Principal{
		ID:    strconv.Itoa(commonmw.UserIDFromContext(c)),
		Role:  string(commonmw.RoleFromContext(c)),
		Admin: rbac.Allows(commonmw.RoleFromContext(c), rbac.PermUserManage),
	}
	if strings.TrimSpace(req.Dir) != "" {
		if d := storageauth.AuthorizeListing(p, roots[0]); !d.Allowed {
			return c.JSON(http.StatusForbidden, echo.Map{
				"message": string(d.Reason), "reason": string(d.Reason), "detail": d.Detail,
			})
		}
	} else {
		roots = authorizedSearchRoots(p, roots)
	}

	start := time.Now()
	hits, total, timedOut := walkSearch(roots, keyword, count)
	took := time.Since(start).Milliseconds()

	return c.JSON(http.StatusOK, echo.Map{
		"message":   "ok",
		"took":      took,
		"timed_out": timedOut,
		"data": echo.Map{
			"cursor": "", // v1: no cursor pagination
			"total":  total,
			"hits":   hits,
		},
	})
}

// resolveSearchRoots returns the directories to walk. An empty dir means "all
// storage roots"; a supplied dir must resolve UNDER a storage root (else the
// caller could walk /etc, /root, another user's home, etc.). Returns ok=false
// on an out-of-bounds dir.
func resolveSearchRoots(dir string) ([]string, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		out := make([]string, 0, len(searchStorageRoots))
		for _, r := range searchStorageRoots {
			if fi, err := os.Stat(r); err == nil && fi.IsDir() {
				out = append(out, r)
			}
		}
		return out, true
	}
	clean := filepath.Clean(dir)
	for _, r := range searchStorageRoots {
		if clean == r || strings.HasPrefix(clean, r+string(filepath.Separator)) {
			// Defense in depth: resolve symlinks and re-check the REAL path is
			// still under a storage root, so /DATA/link→/etc can't smuggle the
			// walk out of bounds. If the path can't be resolved (doesn't exist
			// yet), there's nothing to walk, so the lexical result is safe.
			real, err := filepath.EvalSymlinks(clean)
			if err != nil {
				return []string{clean}, true
			}
			for _, rr := range searchStorageRoots {
				if real == rr || strings.HasPrefix(real, rr+string(filepath.Separator)) {
					return []string{real}, true
				}
			}
			return nil, false
		}
	}
	return nil, false
}

// authorizedSearchRoots narrows the storage roots to what p may list: a root
// the authority allows as a whole, else the account's grants under it (nested
// grants collapse into their parent so no hit is returned twice).
func authorizedSearchRoots(p storageauth.Principal, roots []string) []string {
	under := func(parent, child string) bool {
		return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
	}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if storageauth.AuthorizeListing(p, r).Allowed {
			out = append(out, r)
			continue
		}
		grants := storageauth.EffectiveRoots(p)
		for _, g := range grants {
			if !under(r, g.Path) || !storageauth.AuthorizeListing(p, g.Path).Allowed {
				continue
			}
			nested := false
			for _, o := range grants {
				if o.Path != g.Path && under(o.Path, g.Path) {
					nested = true
					break
				}
			}
			if !nested {
				out = append(out, g.Path)
			}
		}
	}
	return out
}

// walkSearch runs a bounded, case-insensitive filename walk across roots and
// returns ZimaOS-shaped hits. Bounds mirror the v1 file API: max files, max
// results, wall-clock deadline.
func walkSearch(roots []string, keyword string, limit int) (hits []hitObject, total int, timedOut bool) {
	hits = make([]hitObject, 0, 64)
	kl := strings.ToLower(keyword)
	deadline := time.Now().Add(searchWalkTimeout)
	visited := 0
	var id int64

	for _, root := range roots {
		if len(hits) >= limit || time.Now().After(deadline) {
			break
		}
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				if info != nil && info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			visited++
			if visited > searchWalkMaxFiles || time.Now().After(deadline) {
				timedOut = true
				return io.EOF
			}
			name := info.Name()
			// Skip hidden trees (dotfiles, .Trash, .temp scratch).
			if strings.HasPrefix(name, ".") {
				if info.IsDir() && p != root {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.Contains(strings.ToLower(name), kl) {
				id++
				hits = append(hits, hitObject{
					ID:    id,
					Score: 1,
					Source: hitSource{
						Name:   name,
						Dir:    filepath.Dir(p),
						IsDir:  info.IsDir(),
						Mtime:  info.ModTime().Unix(),
						Size:   info.Size(),
						Type:   classifyFileType(name, info.IsDir()),
						HlName: highlightMatch(name, keyword),
					},
				})
				if len(hits) >= limit {
					timedOut = true // signal "there may be more"
					return io.EOF
				}
			}
			return nil
		})
	}
	return hits, len(hits), timedOut
}

// classifyFileType maps a filename to the ZimaOS type enum.
func classifyFileType(name string, isDir bool) string {
	if isDir {
		return "dir"
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".doc", ".docx", ".odt", ".rtf", ".xls", ".xlsx", ".ods", ".ppt", ".pptx", ".odp":
		return "doc"
	case ".pdf":
		return "pdf"
	case ".txt", ".md", ".log", ".csv", ".json", ".xml", ".yaml", ".yml", ".ini", ".conf":
		return "txt"
	case ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp", ".svg", ".heic", ".tiff", ".ico":
		return "img"
	case ".mp4", ".mkv", ".avi", ".mov", ".webm", ".flv", ".wmv", ".m4v", ".mpg", ".mpeg":
		return "video"
	case ".mp3", ".flac", ".wav", ".aac", ".ogg", ".m4a", ".wma", ".opus":
		return "audio"
	case ".zip", ".tar", ".gz", ".bz2", ".xz", ".7z", ".rar", ".tgz", ".zst":
		return "archive"
	default:
		return "other"
	}
}

// highlightMatch wraps the (case-insensitive) matched substring in <b>…</b>,
// HTML-escaping the rest so a hostile filename can't inject markup.
func highlightMatch(name, keyword string) string {
	idx := strings.Index(strings.ToLower(name), strings.ToLower(keyword))
	end := idx + len(keyword)
	// idx/end are byte offsets in the LOWERCASED string. strings.ToLower can
	// change a string's byte length for some runes (Kelvin sign K→k, Turkish
	// İ→i̇), so those offsets may not be valid indexes into the original
	// `name`. If they don't line up, skip highlighting rather than slice out
	// of bounds — name[:idx] with idx>len(name) would panic. The whole string
	// is HTML-escaped either way, so no markup injection.
	// ponytail: byte-offset heuristic; exotic-Unicode matches lose the <b>
	// wrap but never crash. Rune-accurate mapping only if users complain.
	if idx < 0 || idx > len(name) || end > len(name) {
		return html.EscapeString(name)
	}
	return html.EscapeString(name[:idx]) + "<b>" + html.EscapeString(name[idx:end]) + "</b>" + html.EscapeString(name[end:])
}
