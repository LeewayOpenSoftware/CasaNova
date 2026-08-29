/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-07-26 11:21:14
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-08-18 11:16:25
 * @FilePath: /CasaOS/service/shares.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package service

import (
	"path/filepath"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/command"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova/service/model"
	model2 "github.com/LeewayOpenSoftware/CasaNova/service/model"
	"gorm.io/gorm"
)

type SharesService interface {
	GetSharesList() (shares []model2.SharesDBModel)
	GetSharesByPath(path string) (shares []model2.SharesDBModel)
	GetSharesByName(name string) (shares []model2.SharesDBModel)
	CreateShare(share model2.SharesDBModel)
	DeleteShare(id string)
	UpdateConfigFile()
	InitSambaConfig()
	DeleteShareByPath(path string)
}

type sharesStruct struct {
	db *gorm.DB
}

func (s *sharesStruct) DeleteShareByPath(path string) {
	s.db.Where("path LIKE ?", path+"%").Delete(&model.SharesDBModel{})
	s.UpdateConfigFile()
}

func (s *sharesStruct) GetSharesByName(name string) (shares []model2.SharesDBModel) {
	s.db.Select("anonymous,path,id").Where("name = ?", name).Find(&shares)

	return
}

func (s *sharesStruct) GetSharesByPath(path string) (shares []model2.SharesDBModel) {
	s.db.Select("anonymous,path,id").Where("path = ?", path).Find(&shares)
	return
}

func (s *sharesStruct) GetSharesList() (shares []model2.SharesDBModel) {
	s.db.Select("anonymous,path,id").Find(&shares)
	return
}

func (s *sharesStruct) CreateShare(share model2.SharesDBModel) {
	s.db.Create(&share)
	s.InitSambaConfig()
	s.UpdateConfigFile()
}

func (s *sharesStruct) DeleteShare(id string) {
	s.db.Where("id= ?", id).Delete(&model.SharesDBModel{})
	s.UpdateConfigFile()
}

func (s *sharesStruct) UpdateConfigFile() {
	shares := []model2.SharesDBModel{}
	s.db.Select("anonymous,path").Find(&shares)
	// generated config file
	configStr := ""
	for _, share := range shares {
		// Validate share path to prevent path-traversal attacks.
		cleanPath := filepath.Clean(share.Path)
		if !filepath.IsAbs(cleanPath) || strings.Contains(cleanPath, "..") {
			continue // skip invalid / suspicious paths
		}

		// Sanitize the directory name used as the SMB section header:
		// strip newlines and brackets that could inject extra INI sections.
		dirName := filepath.Base(cleanPath)
		dirName = strings.NewReplacer("\n", "", "\r", "", "[", "", "]", "").Replace(dirName)
		if dirName == "" || dirName == "." {
			continue
		}

		guestOk := "No"
		if share.Anonymous {
			guestOk = "Yes"
		}
		configStr += `
[` + dirName + `]
comment = CasaNova share ` + dirName + `
path = ` + cleanPath + `
browseable = Yes
read only = No
guest ok = ` + guestOk + `
create mask = 0755
directory mask = 0755
valid users = root

`
	}
	// write config file
	file.WriteToPath([]byte(configStr), "/etc/samba", "smb.casa.conf")
	// restart samba
	command.OnlyExec("source " + config.AppInfo.ShellPath + "/helper.sh ;RestartSMBD")
}

func (s *sharesStruct) InitSambaConfig() {
	if file.Exists("/etc/samba/smb.conf") {
		str := file.ReadLine(1, "/etc/samba/smb.conf")
		if strings.Contains(str, "# Copyright (c) 2021-2022 CasaOS Inc. All rights reserved.") || strings.Contains(str, "# CasaNova") {
			return
		}
		file.MoveFile("/etc/samba/smb.conf", "/etc/samba/smb.conf.bak")
		smbConf := ""
		smbConf += `# Copyright (c) 2021-2022 CasaOS Inc. All rights reserved.
#
#
#                          ______     _______
#                        (  __  \   (  ___  )
#                        | (  \  )  | (   ) |
#                        | |   ) |  | |   | |
#                        | |   | |  | |   | |
#                        | |   ) |  | |   | |
#                        | (__/  )  | (___) |
#                        (______/   (_______)
#
#                   _          _______   _________
#                  ( (    /|  (  ___  )  \__   __/
#                  |  \  ( |  | (   ) |     ) (
#                  |   \ | |  | |   | |     | |
#                  | (\ \) |  | |   | |     | |
#                  | | \   |  | |   | |     | |
#                  | )  \  |  | (___) |     | |
#                  |/    )_)  (_______)     )_(
#
#   _______    _______    ______    _________   _______
#  (       )  (  ___  )  (  __  \   \__   __/  (  ____ \  |\     /|
#  | () () |  | (   ) |  | (  \  )     ) (     | (    \/  ( \   / )
#  | || || |  | |   | |  | |   ) |     | |     | (__       \ (_) /
#  | |(_)| |  | |   | |  | |   | |     | |     |  __)       \   /
#  | |   | |  | |   | |  | |   ) |     | |     | (           ) (
#  | )   ( |  | (___) |  | (__/  )  ___) (___  | )           | |
#  |/     \|  (_______)  (______/   \_______/  |/            \_/
#
#
# IMPORTANT: CasaOS will not provide technical support for any issues
#            caused by unauthorized modification to the configuration.

[global]
## fruit settings
   min protocol = SMB2
   ea support = yes
## vfs objects = fruit streams_xattr
   fruit:metadata = stream
   fruit:model = Macmini
   fruit:veto_appledouble = no
   fruit:posix_rename = yes
   fruit:zero_file_id = yes
   fruit:wipe_intentionally_left_blank_rfork = yes
   fruit:delete_empty_adfiles = yes
   map to guest = bad user
   include=/etc/samba/smb.casa.conf`
		file.WriteToPath([]byte(smbConf), "/etc/samba", "smb.conf")
	}
}

func NewSharesService(db *gorm.DB) SharesService {
	return &sharesStruct{db: db}
}
