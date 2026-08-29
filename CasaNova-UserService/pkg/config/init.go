package config

import (
	"fmt"
	"log"
	"os"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/constants"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/model"
	"gopkg.in/ini.v1"
)

// models with default values

var (
	CommonInfo = &model.CommonModel{
		RuntimePath: constants.DefaultRuntimePath,
	}

	AppInfo = &model.APPModel{
		DBPath:       constants.DefaultDataPath,
		UserDataPath: constants.DefaultDataPath,
		LogPath:      constants.DefaultLogPath,
		LogSaveName:  "user",
		LogFileExt:   "log",
	}

	Cfg            *ini.File
	ConfigFilePath string
)

func InitSetup(config string, sample string) {
	ConfigFilePath = UserServiceConfigFilePath
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

	Cfg, err = ini.Load(ConfigFilePath)
	if err != nil {
		fmt.Printf("Fail to read file: %v", err)
		os.Exit(1)
	}

	mapTo("common", CommonInfo)
	mapTo("app", AppInfo)
}

func SaveSetup(config string) {
	reflectFrom("common", CommonInfo)
	reflectFrom("app", AppInfo)

	configFilePath := UserServiceConfigFilePath
	if len(config) > 0 {
		configFilePath = config
	}

	if err := Cfg.SaveTo(configFilePath); err != nil {
		fmt.Printf("Fail to save file: %v", err)
		os.Exit(1)
	}
}

func mapTo(section string, v interface{}) {
	err := Cfg.Section(section).MapTo(v)
	if err != nil {
		log.Fatalf("Cfg.MapTo %s err: %v", section, err)
	}
}

func reflectFrom(section string, v interface{}) {
	err := Cfg.Section(section).ReflectFrom(v)
	if err != nil {
		log.Fatalf("Cfg.ReflectFrom %s err: %v", section, err)
	}
}
