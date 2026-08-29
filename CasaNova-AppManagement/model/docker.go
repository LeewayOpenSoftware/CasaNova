/*@Author: LinkLeong dev@lucentopen.org
 *@Date: 2021-12-08 18:10:25
 *@LastEditors: LinkLeong
 *@LastEditTime: 2022-07-13 10:49:16
 *@FilePath: /CasaOS/model/docker.go
 *@Description:
 *@Website: https://casanova.dev
 *Copyright (c) 2022-2026 Leeway Open Software.
 */
package model

type DockerStatsModel struct {
	Icon     string      `json:"icon"`
	Title    string      `json:"title"`
	Data     interface{} `json:"data"`
	Previous interface{} `json:"previous"`
}

// reference - https://docs.docker.com/engine/reference/commandline/dockerd/#daemon-configuration-file
type DockerDaemonConfigurationModel struct {
	// e.g. `/var/lib/docker`
	Root string `json:"data-root,omitempty"`
}
