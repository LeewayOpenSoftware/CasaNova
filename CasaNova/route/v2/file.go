package v2

import (
	"errors"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"net/http"
	"strconv"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/codegen"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/labstack/echo/v4"
)

// Path: route/v2/file.go

// GetFileTest was a debug helper that hardcoded http.ServeFile on
// /DATA/test.img and then double-wrote a "pong" string to the response.
// Wave 27: neutralized — it bypassed every path-validation gate, served
// an unauthenticated arbitrary file, and corrupted its own response.
// The route is still wired in codegen (regenerating the OpenAPI is out
// of scope), so the handler stays, but it now returns 410 Gone with no
// body. Delete the codegen entry on the next OpenAPI regeneration.
func (s *CasaOS) GetFileTest(ctx echo.Context) error {
	return ctx.NoContent(http.StatusGone)
}

func (c *CasaOS) CheckUploadChunk(ctx echo.Context, params codegen.CheckUploadChunkParams) error {
	// Same gate as the POST sibling (and the v1 twin GetFileUpload): upload
	// state is only the business of callers who can write files. No loopback
	// trust — the gateway proxies LAN traffic over 127.0.0.1.
	if !rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermFileWrite) {
		return ctx.JSON(http.StatusForbidden, map[string]string{"message": "insufficient permissions"})
	}
	identifier := ctx.QueryParam("identifier")
	chunkNumber, err := strconv.ParseInt(ctx.QueryParam("chunkNumber"), 10, 64)
	if err != nil {
		return ctx.NoContent(http.StatusBadRequest)
	}

	err = c.fileUploadService.TestChunk(ctx, identifier, chunkNumber)
	if err != nil {
		return ctx.NoContent(http.StatusNoContent)
	}
	return ctx.NoContent(http.StatusOK)
}

// PostUploadFile receives a chunk of a multipart upload.
//
// Wave 33-A CRITICAL: this handler used to read `path` from the form
// body and pass it straight into FileUploadService.UploadFile() with
// no validation, no traversal check, and no permission gate. The v2
// router only installs JWT middleware (with a loopback skipper), so
// any authenticated user — and any unauthenticated local process via
// loopback — could POST `path=/root/.ssh`, `relativePath=authorized_keys`,
// `filename=authorized_keys` and write attacker-controlled bytes to
// /root/.ssh/authorized_keys as root. Single multipart POST → full
// system takeover. The v1 sibling PostFileUpload always validated the
// path; only the v2 endpoint was missing the gate.
//
// Fix: enforce three things before touching FileUploadService:
//
//  1. utils.SanitizeFilePath cleans the path, rejects empty / null
//     bytes / .. traversal, and bans every entry in the canonical
//     utils.BlockedSystemPrefixes (/etc, /root, /boot, /usr, /run,
//     /var — the JWT key — etc.), matching what route/v2.go::InitFile
//     enforces on the download side. Single source of truth = no drift.
//  2. PermFileWrite is required for all callers (lines 93-98). Loopback
//     is NOT trusted, preventing unauthenticated file uploads; authenticated
//     users must hold PermFileWrite (denied to read-only operator roles).
func (c *CasaOS) PostUploadFile(ctx echo.Context) error {
	rawPath := ctx.FormValue("path")
	cleanPath, err := utils.SanitizeFilePath(rawPath)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]string{"message": err.Error()})
	}
	// Through the storage authority as well: the sanitiser above only proves
	// the destination avoids the blocked system prefixes, which still permits
	// /home, /tmp and every unlisted top-level directory.
	if d := storageauth.AuthorizeStorage(storageauth.Principal{
		ID:    strconv.Itoa(commonmw.UserIDFromContext(ctx)),
		Role:  string(commonmw.RoleFromContext(ctx)),
		Admin: rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermUserManage),
	}, storageauth.OpUpload|storageauth.OpCreate|storageauth.OpWrite, cleanPath); !d.Allowed {
		return ctx.JSON(http.StatusForbidden, map[string]interface{}{
			"message": string(d.Reason), "reason": string(d.Reason),
			"detail": d.Detail, "remediable": d.Remediable,
		})
	}
	// Do NOT trust loopback here: the gateway proxies every LAN request
	// over 127.0.0.1, so a loopback allowance would let any unauthenticated
	// LAN caller upload arbitrary files as root. File upload has no internal
	// tokenless caller — require PermFileWrite for everyone. Real WebUI
	// traffic carries a JWT that resolves to the uploader's role.
	if !rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermFileWrite) {
		return ctx.JSON(http.StatusForbidden, map[string]string{"message": "insufficient permissions"})
	}
	path := cleanPath

	// handle the request
	chunkNumber, err := strconv.ParseInt(ctx.FormValue("chunkNumber"), 10, 64)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, err)
	}
	chunkSize, err := strconv.ParseInt(ctx.FormValue("chunkSize"), 10, 64)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, err)
	}
	currentChunkSize, err := strconv.ParseInt(ctx.FormValue("currentChunkSize"), 10, 64)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, err)
	}
	totalChunks, err := strconv.ParseInt(ctx.FormValue("totalChunks"), 10, 64)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, err)
	}
	totalSize, err := strconv.ParseInt(ctx.FormValue("totalSize"), 10, 64)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, err)
	}

	identifier := ctx.FormValue("identifier")
	fileName := ctx.FormValue("filename")
	relativePath := ctx.FormValue("relativePath")
	bin, err := ctx.FormFile("file")

	if err != nil {
		return ctx.JSON(http.StatusBadRequest, err)
	}

	err = c.fileUploadService.UploadFile(
		ctx,
		path,
		chunkNumber,
		chunkSize,
		currentChunkSize,
		totalChunks,
		totalSize,
		identifier,
		relativePath,
		fileName,
		bin,
	)
	if err != nil {
		// 409 = resume state invalid, restart the file from chunk 1. The
		// client handles this specifically; everything else is a plain 500.
		// (Marshal the STRING — encoding a bare `error` yields "{}".)
		switch {
		case errors.Is(err, service.ErrSessionReset):
			return ctx.JSON(http.StatusConflict, map[string]string{"message": err.Error()})
		case errors.Is(err, service.ErrTooManyUploadSessions):
			// 429, not 500: the registry is at its admission limit, the
			// request is well-formed, and retrying later is the right move.
			// Uploads already in flight are unaffected — only the creation of
			// a NEW session is refused.
			return ctx.JSON(http.StatusTooManyRequests, map[string]string{"message": err.Error()})
		case errors.Is(err, service.ErrUploadPathRejected):
			// 400: the destination resolves somewhere the file API must never
			// write (traversal, blocked prefix, or a symlinked directory
			// component planted inside the upload area). Client error, and the
			// message names the reason without echoing the raw path.
			return ctx.JSON(http.StatusBadRequest, map[string]string{"message": err.Error()})
		}
		return ctx.JSON(http.StatusInternalServerError, map[string]string{"message": err.Error()})
	}
	return ctx.NoContent(http.StatusOK)
}
