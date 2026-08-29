package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/constants"
	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/model"
	"gopkg.in/ini.v1"
)

var (
	ModManagementConfigFilePath = filepath.Join(constants.DefaultConfigPath, "mod-management.conf")

	Cfg            *ini.File
	ConfigFilePath string

	CommonInfo = &model.CommonModel{
		RuntimePath: constants.DefaultRuntimePath,
	}

	AppInfo = &model.APPModel{
		LogPath:     constants.DefaultLogPath,
		LogSaveName: common.ServiceName,
		LogFileExt:  "log",
	}
)

func InitSetup(config string, sample string) {
	ConfigFilePath = ModManagementConfigFilePath
	if len(config) > 0 {
		ConfigFilePath = config
	}

	// create default config file if not exist
	if _, err := os.Stat(ConfigFilePath); os.IsNotExist(err) {
		fmt.Println("config file not exist, create it")
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

	Cfg, err = ini.LoadSources(ini.LoadOptions{Insensitive: true, AllowShadows: true}, ConfigFilePath)
	if err != nil {
		panic(err)
	}

	mapTo("common", CommonInfo)
	mapTo("app", AppInfo)
}

func SaveSetup() error {
	reflectFrom("common", CommonInfo)
	reflectFrom("app", AppInfo)

	return Cfg.SaveTo(ConfigFilePath)
}

func mapTo(section string, v any) {
	err := Cfg.Section(section).MapTo(v)
	if err != nil {
		log.Fatalf("Cfg.MapTo %s err: %v", section, err)
	}
}

func reflectFrom(section string, v any) {
	err := Cfg.Section(section).ReflectFrom(v)
	if err != nil {
		log.Fatalf("Cfg.ReflectFrom %s err: %v", section, err)
	}
}
