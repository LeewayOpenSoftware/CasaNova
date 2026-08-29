package v1

import (
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/drivers/dropbox"
	"github.com/LeewayOpenSoftware/CasaNova/drivers/google_drive"
	"github.com/LeewayOpenSoftware/CasaNova/drivers/onedrive"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/oauthflow"
	"github.com/labstack/echo/v4"
)

// driverCatalogEntry pairs a display name with the provider the authority knows.
var driverCatalog = []struct {
	Name     string
	Provider oauthflow.Provider
	Icon     func() string
}{
	{"Google Drive", oauthflow.ProviderGoogleDrive, func() string { return google_drive.GetConfig().Icon }},
	{"Dropbox", oauthflow.ProviderDropbox, func() string { return dropbox.GetConfig().Icon }},
	{"OneDrive", oauthflow.ProviderOnedrive, func() string { return onedrive.GetConfig().Icon }},
}

// ListDriverInfo lists the cloud providers this build can connect to.
//
// L-02/G-01, sibling surface: this endpoint is the OTHER half of the callback
// vulnerability. It handed every caller a ready-made authorization URL whose
// OAuth `state` was the literal string "${HOST}/v1/recover/GoogleDrive" — a
// redirect target, not a nonce — which is exactly why the callback had nothing
// to verify. Fixing only the callback would have left this endpoint minting URLs
// that can no longer complete.
//
// Now the URL is minted through the same authority the callback consumes, so
// there is one place in the tree that can produce a usable authorization URL and
// it always binds a state token. Listing needs storage:read; a URL is only
// minted for a caller who also holds storage:write, because redeeming it mounts
// a filesystem as root. A caller without it still gets the provider list, with
// auth_url empty.
func ListDriverInfo(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermStorageRead); err != nil {
		return err
	}
	mayConnect := rbac.Allows(commonmw.RoleFromContext(ctx), rbac.PermStorageWrite)

	list := []model.Drive{}
	for _, entry := range driverCatalog {
		drive := model.Drive{Name: entry.Name, Icon: entry.Icon()}
		if mayConnect {
			// A provider this build cannot actually talk to, or a request whose
			// origin cannot be reconstructed, yields an empty auth_url rather
			// than a URL that dead-ends at the provider. POST
			// /v1/cloud/:provider/connect reports the reason.
			if authorizeURL, _, err := mintAuthorizeURL(ctx, entry.Provider); err == nil {
				drive.AuthUrl = authorizeURL
			}
		}
		list = append(list, drive)
	}
	return ctx.JSON(common_err.SUCCESS, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: list})
}
