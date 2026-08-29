package service

import (
	json2 "encoding/json"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/httper"
	"github.com/tidwall/gjson"
)

type CasaService interface {
	GetCasaosVersion() model.Version
}

type casaService struct{}

/**
 * @description: get remote version
 * @return {model.Version}
 */
func (o *casaService) GetCasaosVersion() model.Version {
	keyName := "casa_version"
	var dataStr string
	var version model.Version
	if result, ok := Cache.Get(keyName); ok {
		dataStr, ok = result.(string)
		if ok {
			data := gjson.Get(dataStr, "data")
			json2.Unmarshal([]byte(data.String()), &version)
			return version
		}
	}

	// CasaNova ships ServerApi blank by default — version check via
	// remote endpoint is opt-in. Without an explicit URL we skip the
	// HTTP call entirely (avoids "GET https:///v1/sys/version" log
	// spam and a guaranteed failure on every poll). Operators who run
	// their own update server set ServerApi in casanova.conf.
	if config.ServerInfo.ServerApi == "" {
		return version
	}

	v := httper.OasisGet(config.ServerInfo.ServerApi + "/v1/sys/version")
	data := gjson.Get(v, "data")
	json2.Unmarshal([]byte(data.String()), &version)

	if len(version.Version) > 0 {
		Cache.Set(keyName, v, time.Minute*20)
	}

	return version
}

func NewCasaService() CasaService {
	return &casaService{}
}
