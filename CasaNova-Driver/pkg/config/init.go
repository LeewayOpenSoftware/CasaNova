package config

import (
	"fmt"
	"log"
	"os"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/constants"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/model"
	"gopkg.in/ini.v1"
)

var (
	CommonInfo = &model.CommonModel{
		RuntimePath: constants.DefaultRuntimePath,
	}

	AppInfo = &model.APPModel{
		LogPath:     constants.DefaultLogPath,
		LogSaveName: "driver",
		LogFileExt:  "log",
	}

	ServerInfo = &model.ServerModel{
		DriverDataRoot: "/opt/casanova/drivers",
		DriverCLI:      "casanova-driver",
		GPUSelectBin:   "/usr/bin/nvidia-driver-select",
		PCIIDsPath:     "/usr/share/hwdata/pci.ids",
	}
)

var (
	Cfg            *ini.File
	ConfigFilePath string
)

// InitSetup loads driver.conf, creating it from the embedded sample on first run.
func InitSetup(configPath string, sample string) {
	ConfigFilePath = DriverConfigFilePath
	if len(configPath) > 0 {
		ConfigFilePath = configPath
	}

	if _, err := os.Stat(ConfigFilePath); os.IsNotExist(err) {
		fmt.Println("config file does not exist, creating:", ConfigFilePath)
		tmpPath := ConfigFilePath + ".tmp"
		file, err := os.Create(tmpPath)
		if err != nil {
			panic(err)
		}
		if _, err = file.WriteString(sample); err != nil {
			file.Close()
			os.Remove(tmpPath)
			panic(err)
		}
		if err = file.Sync(); err != nil {
			file.Close()
			os.Remove(tmpPath)
			panic(err)
		}
		file.Close()
		if err = os.Rename(tmpPath, ConfigFilePath); err != nil {
			os.Remove(tmpPath)
			panic(err)
		}
	}

	var err error
	Cfg, err = ini.Load(ConfigFilePath)
	if err != nil {
		panic(err)
	}

	mapTo("common", CommonInfo)
	mapTo("app", AppInfo)
	mapTo("server", ServerInfo)
}

func mapTo(section string, v interface{}) {
	if err := Cfg.Section(section).MapTo(v); err != nil {
		log.Fatalf("Cfg.MapTo %s err: %v", section, err)
	}
}
