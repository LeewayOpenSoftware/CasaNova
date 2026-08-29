/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2021-09-30 18:18:14
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-08-31 17:04:02
 * @FilePath: /CasaOS/pkg/config/config.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package config

import (
	"path/filepath"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/constants"
)

var CasaOSConfigFilePath = filepath.Join(constants.DefaultConfigPath, "casaos.conf")
