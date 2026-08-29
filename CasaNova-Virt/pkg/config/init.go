package config

import (
	"fmt"
	"os"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/constants"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
	"gopkg.in/ini.v1"
)

// Defaults mirror the ZimaOS oracle: modular libvirt daemon socket, the
// /DATA/Downloads/ISO image directory recovered from the binary, and the
// libvirt default storage pool directory.
var (
	CommonInfo = &model.CommonModel{
		RuntimePath: constants.DefaultRuntimePath,
	}

	AppInfo = &model.APPModel{
		LogPath:     constants.DefaultLogPath,
		LogSaveName: "virt-management",
		LogFileExt:  "log",
	}

	ServerInfo = &model.ServerModel{
		LibvirtSocket: "/var/run/libvirt/virtqemud-sock",
		ImageDir:      "/DATA/Downloads/ISO",
		DiskDir:       "/var/lib/libvirt/images",
	}
)

var (
	Cfg            *ini.File
	ConfigFilePath string
)

// InitSetup loads (creating from the embedded sample if absent) the .conf file
// and maps its sections onto the config structs. Mirrors
// CasaNova-LocalStorage/pkg/config.InitSetup.
func InitSetup(configPath, sample string) {
	ConfigFilePath = VirtConfigFilePath
	if len(configPath) > 0 {
		ConfigFilePath = configPath
	}

	if _, err := os.Stat(ConfigFilePath); os.IsNotExist(err) {
		fmt.Println("config file not exist, creating:", ConfigFilePath)
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
		panic(err)
	}
}
