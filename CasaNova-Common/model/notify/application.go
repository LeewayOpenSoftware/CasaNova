/*@Author: LinkLeong dev@lucentopen.org
 *@Date: 2022-05-27 15:01:58
 *@LastEditors: LinkLeong
 *@LastEditTime: 2022-05-31 14:51:21
 *@FilePath: /CasaOS/model/notify/application.go
 *@Description:
 *@Website: https://casanova.dev
 *Copyright (c) 2022-2026 Leeway Open Software.
 */
package notify

type Application struct {
	Name       string            `json:"name"`
	State      string            `json:"state"`
	Type       string            `json:"type"`
	Icon       string            `json:"icon"`
	Message    string            `json:"message"`
	Finished   bool              `json:"finished"`
	Success    bool              `json:"success"`
	Properties map[string]string `json:"properties"`
}
