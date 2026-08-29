/*@Author: LinkLeong dev@lucentopen.org
 *@Date: 2021-12-07 17:14:41
 *@LastEditors: LinkLeong
 *@LastEditTime: 2022-08-17 18:46:43
 *@FilePath: /CasaOS/service/model/o_disk.go
 *@Description:
 *@Website: https://casanova.dev
 *Copyright (c) 2022-2026 Leeway Open Software.
 */
package model

type Volume struct {
	ID         uint   `gorm:"column:id;primary_key" json:"id"`
	UUID       string `json:"uuid"` // Filesystem UUID (stable across reboots/port changes)
	MountPoint string `json:"mount_point"`
	CreatedAt  int64  `json:"created_at"`

	// Added in CasaNova v0.5.0 for stable disk identity
	Serial     string `json:"serial,omitempty"`      // Physical disk serial number
	DevicePath string `json:"device_path,omitempty"` // Last known /dev/sdX path (informational)
	FsType     string `json:"fs_type,omitempty"`     // Filesystem type (ext4, xfs, btrfs)
	Label      string `json:"label,omitempty"`       // Filesystem label (if set)
}

func (p *Volume) TableName() string {
	return "o_disk" // legacy table name - :(
}
