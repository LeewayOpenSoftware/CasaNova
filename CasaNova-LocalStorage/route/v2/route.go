package v2

import (
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/codegen"
)

type LocalStorage struct{}

func NewLocalStorage() codegen.ServerInterface {
	return &LocalStorage{}
}
