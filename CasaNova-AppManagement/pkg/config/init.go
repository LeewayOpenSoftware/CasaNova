package config

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/constants"
	"gopkg.in/ini.v1"
)

var (
	CommonInfo = &model.CommonModel{
		RuntimePath: constants.DefaultRuntimePath,
	}

	AppInfo = &model.APPModel{
		AppStorePath: filepath.Join(constants.DefaultDataPath, "appstore"),
		AppsPath:     filepath.Join(constants.DefaultDataPath, "apps"),
		LogPath:      constants.DefaultLogPath,
		LogSaveName:  common.AppManagementServiceName,
		LogFileExt:   "log",
	}

	ServerInfo = &model.ServerModel{
		AppStoreList: []string{},
	}

	// Global is a map to inject environment variables to the app.
	Global   = make(map[string]string)
	GlobalMu sync.RWMutex

	CasaOSGlobalVariables = &model.CasaOSGlobalVariables{}

	Cfg               *ini.File
	ConfigFilePath    string
	GlobalEnvFilePath string

	// configMu protects Cfg, ServerInfo, CommonInfo, and AppInfo from
	// concurrent reads while ReloadConfig is writing them.
	configMu sync.RWMutex
)

// GetServerInfoAppStoreList returns a copy of the AppStoreList under configMu.
func GetServerInfoAppStoreList() []string {
	configMu.RLock()
	defer configMu.RUnlock()
	listCopy := make([]string, len(ServerInfo.AppStoreList))
	copy(listCopy, ServerInfo.AppStoreList)
	return listCopy
}

// SetServerInfoAppStoreList replaces the AppStoreList under configMu.
func SetServerInfoAppStoreList(list []string) {
	configMu.Lock()
	defer configMu.Unlock()
	ServerInfo.AppStoreList = list
}

func ReloadConfig() {
	configMu.Lock()
	defer configMu.Unlock()

	var err error
	Cfg, err = ini.LoadSources(ini.LoadOptions{Insensitive: true, AllowShadows: true}, ConfigFilePath)
	if err != nil {
		fmt.Println("failed to reload config", err)
	} else {
		mapTo("common", CommonInfo)
		mapTo("app", AppInfo)
		mapTo("server", ServerInfo)
	}
}

func InitSetup(config string, sample string) {
	ConfigFilePath = AppManagementConfigFilePath
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
	mapTo("server", ServerInfo)
}

func SaveSetup() error {
	reflectFrom("common", CommonInfo)
	reflectFrom("app", AppInfo)
	reflectFrom("server", ServerInfo)

	return Cfg.SaveTo(ConfigFilePath)
}

func InitGlobal(config string) {
	// read file
	// file content like this:
	// OPENAI_API_KEY=123456

	// read file
	GlobalEnvFilePath = AppManagementGlobalEnvFilePath
	if len(config) > 0 {
		ConfigFilePath = config
	}

	// from file read key and value
	// set to Global
	file, err := os.Open(GlobalEnvFilePath)
	// there can't to panic err. because the env file is a new file
	// very much user didn't have the file.
	if err != nil {
		// log.Fatal will exit the program. So we only can to log the error.
		log.Println("open global env file error:", err)
	} else {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		scanner.Split(bufio.ScanLines)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				Global[parts[0]] = parts[1]
			}
		}
	}
}

func SaveGlobal() error {
	// file content like this:
	// OPENAI_API_KEY=123456
	tmpPath := AppManagementGlobalEnvFilePath + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	writer := bufio.NewWriter(file)

	GlobalMu.RLock()
	for key, value := range Global {
		fmt.Fprintf(writer, "%s=%s\n", key, value)
	}
	GlobalMu.RUnlock()

	if err := writer.Flush(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}

	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}

	if err := file.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, AppManagementGlobalEnvFilePath); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return nil
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
