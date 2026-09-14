/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-07-26 18:13:22
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-08-04 20:10:31
 * @FilePath: /CasaOS/service/connections.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package service

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova/service/model"
	model2 "github.com/LeewayOpenSoftware/CasaNova/service/model"
	"github.com/moby/sys/mount"
	"golang.org/x/sys/unix"
	"gorm.io/gorm"
)

type ConnectionsService interface {
	GetConnectionsList() (connections []model2.ConnectionsDBModel)
	GetConnectionByHost(host string) (connections []model2.ConnectionsDBModel)
	GetConnectionByID(id string) (connections model2.ConnectionsDBModel)
	CreateConnection(connection *model2.ConnectionsDBModel)
	DeleteConnection(id string)
	UpdateConnection(connection *model2.ConnectionsDBModel)
	MountSmaba(username, host, directory, port, mountPoint, password string) error
	UnmountSmaba(mountPoint string) error
}

type connectionsStruct struct {
	db *gorm.DB
}

func (s *connectionsStruct) GetConnectionByHost(host string) (connections []model2.ConnectionsDBModel) {
	s.db.Select("username,host,status,id").Where("host = ?", host).Find(&connections)
	return
}

func (s *connectionsStruct) GetConnectionByID(id string) (connections model2.ConnectionsDBModel) {
	s.db.Select("username,password,host,status,id,directories,mount_point,port").Where("id = ?", id).First(&connections)
	return
}

func (s *connectionsStruct) GetConnectionsList() (connections []model2.ConnectionsDBModel) {
	s.db.Select("username,host,port,status,id,mount_point").Find(&connections)
	return
}

func (s *connectionsStruct) CreateConnection(connection *model2.ConnectionsDBModel) {
	s.db.Create(connection)
}

func (s *connectionsStruct) UpdateConnection(connection *model2.ConnectionsDBModel) {
	s.db.Save(connection)
}

func (s *connectionsStruct) DeleteConnection(id string) {
	s.db.Where("id= ?", id).Delete(&model.ConnectionsDBModel{})
}

// ErrCIFSOptionUnrepresentable is returned when a credential cannot be placed
// in a CIFS option string at all. The kernel's cifs option parser splits on
// "," and NUL-terminates, and it offers NO escape: a password containing a
// comma cannot be expressed here, only misparsed. mount.cifs(8) solves this
// with credentials=<file>, which is not reachable from a direct unix.Mount.
//
// So this refuses instead of mounting something other than what was asked for.
// Silently sending "password=a,b" hands the kernel "password=a" plus a bogus
// option "b": the mount fails, or worse succeeds with the wrong credential,
// and nothing says why. Refusing with a message the user can act on is the
// only honest answer an API that cannot express the value can give.
var ErrCIFSOptionUnrepresentable = errors.New("samba: credential contains a character that CIFS mount options cannot carry")

var cifsPortRe = regexp.MustCompile(`^[0-9]{1,5}$`)

// cifsMountOptions builds the option string for a CIFS mount.
//
// Wave: `port` used to be accepted by the API, stored in the database, passed
// into this function and then DROPPED on the floor -- every mount went to 445.
// A NAS on a non-standard port could be configured, saved and displayed, and
// never once mounted.
func cifsMountOptions(username, password, port string) (string, error) {
	for _, v := range []string{username, password} {
		if strings.ContainsAny(v, ",\x00") {
			return "", ErrCIFSOptionUnrepresentable
		}
	}
	opts := fmt.Sprintf("username=%s,password=%s", username, password)
	// Empty or 445 keeps the previous wire behaviour exactly; anything else is
	// the port the user asked for and was being ignored. The digits check is
	// what keeps a crafted "port" from becoming extra mount options.
	if port != "" && port != "445" {
		if !cifsPortRe.MatchString(port) {
			return "", fmt.Errorf("samba: invalid port %q", port)
		}
		opts += ",port=" + port
	}
	return opts, nil
}

func (s *connectionsStruct) MountSmaba(username, host, directory, port, mountPoint, password string) error {
	opts, err := cifsMountOptions(username, password, port)
	if err != nil {
		return err
	}
	return unix.Mount(
		fmt.Sprintf("//%s/%s", host, directory),
		mountPoint,
		"cifs",
		unix.MS_NOATIME|unix.MS_NODEV|unix.MS_NOSUID,
		opts,
	)
}

func (s *connectionsStruct) UnmountSmaba(mountPoint string) error {
	return mount.Unmount(mountPoint)
}

func NewConnectionsService(db *gorm.DB) ConnectionsService {
	return &connectionsStruct{db: db}
}
