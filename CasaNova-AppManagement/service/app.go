package service

import (
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

type App types.ServiceConfig

func (a *App) StoreInfo() (codegen.AppStoreInfo, error) {
	var storeInfo codegen.AppStoreInfo

	// v0.1: read canonical x-casanova first, fall back to legacy
	// x-casaos so compose files from upstream CasaOS stores still
	// surface store metadata. Use common.ComposeExtension to keep
	// the resolution rule in one place.
	ex, ok := common.ComposeExtension(a.Extensions)
	if !ok {
		logger.Error("compose store extension (x-casanova / x-casaos) not found")
		// return storeInfo, ErrComposeExtensionNameXCasaOSNotFound
	}

	// add image to store info for check stable version function.
	storeInfo.Image = a.Image

	if err := loader.Transform(ex, &storeInfo); err != nil {
		return storeInfo, err
	}

	return storeInfo, nil
}
