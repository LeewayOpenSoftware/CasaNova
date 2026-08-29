package v1

import (
	"errors"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"net/http"
	"sync"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/trash"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/labstack/echo/v4"
)

// trashManager is a package-level singleton so all handlers share the same
// retention policy. Initialized lazily so we don't touch the filesystem
// until the user actually hits a trash endpoint.
var (
	trashManager     *trash.Manager
	trashManagerOnce sync.Once
)

func getTrashManager() *trash.Manager {
	trashManagerOnce.Do(func() {
		trashManager = trash.NewManager()
		// Restore writes to the path recorded in the .trashinfo file, which is
		// untrusted (a container with a /DATA bind mount can plant one). Gate
		// the destination through the same canonical sanitiser the rest of the
		// write API uses: it resolves symlinks and refuses any path that lands
		// in a blocked system prefix, so a restore can never drop a root-owned
		// file into /etc, /var or /usr. Without this the .trashinfo Path= was a
		// write-anywhere-as-root primitive.
		trashManager.DestValidator = func(originalPath string) (string, error) {
			clean, err := utils.SanitizeFilePath(originalPath)
			if err != nil {
				return "", err
			}
			// And through the storage authority's ceiling. The sanitiser only
			// proves the destination is not in a blocked system prefix, which
			// leaves /home, /tmp and every unlisted top-level directory open —
			// the exact gap LEG-001 exists to close. A .trashinfo written by
			// the vulnerable release is persisted hostile state, so this is
			// also the rule that a fixed build must not execute what a broken
			// one planted.
			//
			// Ceiling only: a restore has no requesting principal at this
			// point. The handler authorizes the user separately.
			if d := storageauth.AuthorizeSystem(storageauth.OpCreate|storageauth.OpWrite, clean); !d.Allowed {
				return "", errors.New(string(d.Reason) + ": " + d.Detail)
			}
			return clean, nil
		}
	})
	return trashManager
}

// GetTrashList returns all items currently in the recycle bin across every
// mounted filesystem. Newest deletions first.
//
// GET /v1/trash
func GetTrashList(ctx echo.Context) error {
	// Trash listing exposes filenames the user previously deleted —
	// gate behind file-read permission so a read-only role can use it
	// but an unauthenticated session can't.
	if err := requirePerm(ctx, rbac.PermFileRead); err != nil {
		return err
	}
	mgr := getTrashManager()
	entries, err := mgr.List(nil) // nil = auto-discover mountpoints from /proc/mounts
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to list trash: " + err.Error(),
		})
	}

	// Build a user-facing summary with total count and size.
	var totalBytes int64
	for i := range entries {
		totalBytes += entries[i].Size
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"entries":     entries,
			"count":       len(entries),
			"total_bytes": totalBytes,
		},
	})
}

// trashActionRequest is the body for restore/delete endpoints.
// Clients pass the list of trashed-name IDs to act on.
type trashActionRequest struct {
	IDs []string `json:"ids"`
}

// PostTrashRestore moves items from trash back to their original locations.
// Fails if the destination already has a file with the same name (no silent
// overwrites during a restore — the user should resolve the conflict).
//
// POST /v1/trash/restore
// Body: {"ids": ["vacation.mp4", "photos"]}
func PostTrashRestore(ctx echo.Context) error {
	// Restore writes files back into user-visible space — same gate
	// as any other file-mutating endpoint.
	if err := requirePerm(ctx, rbac.PermFileWrite); err != nil {
		return err
	}
	var req trashActionRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	if len(req.IDs) == 0 {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "ids cannot be empty",
		})
	}

	mgr := getTrashManager()
	all, err := mgr.List(nil)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}

	// Build a map for fast lookup by ID.
	byID := make(map[string]*trash.Entry, len(all))
	for i := range all {
		byID[all[i].ID] = &all[i]
	}

	// Collect successes and per-item errors so the client can show a
	// meaningful result even if some items failed (e.g. restore collision).
	type result struct {
		ID      string `json:"id"`
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(req.IDs))
	successCount := 0

	for _, id := range req.IDs {
		entry, ok := byID[id]
		if !ok {
			results = append(results, result{ID: id, Success: false, Error: "not found in trash"})
			continue
		}
		if err := mgr.Restore(entry); err != nil {
			results = append(results, result{ID: id, Success: false, Error: err.Error()})
			continue
		}
		results = append(results, result{ID: id, Success: true})
		successCount++
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"results":  results,
			"restored": successCount,
			"total":    len(req.IDs),
		},
	})
}

// DeleteTrashItem permanently removes specific items from the trash.
// No recovery after this. Empty IDs list = empty entire trash.
//
// DELETE /v1/trash
// Body: {"ids": ["vacation.mp4"]}  — delete specific items
// Body: {"ids": []}                  — empty all trash
func DeleteTrashItem(ctx echo.Context) error {
	// Permanent deletion is irreversible — gate behind the same
	// PermFileDelete that the move-to-trash endpoint uses, so a
	// read-only role can browse the bin without nuking it.
	if err := requirePerm(ctx, rbac.PermFileDelete); err != nil {
		return err
	}
	var req trashActionRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}

	mgr := getTrashManager()

	// Empty everything if no specific IDs given.
	if len(req.IDs) == 0 {
		count, err := mgr.Empty(nil)
		if err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: err.Error(),
			})
		}
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "trash emptied",
			Data:    map[string]int{"deleted": count},
		})
	}

	// Delete specific items.
	all, err := mgr.List(nil)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}
	byID := make(map[string]*trash.Entry, len(all))
	for i := range all {
		byID[all[i].ID] = &all[i]
	}

	deleted := 0
	for _, id := range req.IDs {
		if entry, ok := byID[id]; ok {
			if err := mgr.Delete(entry); err == nil {
				deleted++
			}
		}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    map[string]int{"deleted": deleted, "requested": len(req.IDs)},
	})
}
