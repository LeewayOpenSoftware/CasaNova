/*@Author: link a624669980@163.com
 *@Date: 2022-05-16 17:37:08
 *@LastEditors: LinkLeong
 *@LastEditTime: 2022-07-13 10:46:38
 *@FilePath: /CasaOS/model/category.go
 *@Description:
 */
package model

type ServerCategoryList struct {
	Item []Category `json:"item"`
}
type Category struct {
	ID uint `gorm:"column:id;primary_key" json:"id"`
	//CreatedAt time.Time `json:"created_at"`
	//
	//UpdatedAt time.Time `json:"updated_at"`
	Font  string `json:"font"` // @tiger - if this is frontend-related it does not belong in the backend response; the frontend should define it
	Name  string `json:"name"`
	Count uint   `json:"count"` // @tiger - count is dynamic info; it should live in a separate response struct (see the other note on static/dynamic response fields)
}
