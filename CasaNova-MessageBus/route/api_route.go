package route

import (
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/service"
	jsoniter "github.com/json-iterator/go"
)

type APIRoute struct {
	services *service.Services
}

var json = jsoniter.ConfigCompatibleWithStandardLibrary

// The generated server contract is still enforced at compile time; the
// concrete type is returned so the router can also register handlers
// that are not part of the generated spec (the liveness endpoint).
var _ codegen.ServerInterface = (*APIRoute)(nil)

func NewAPIRoute(services *service.Services) *APIRoute {
	return &APIRoute{
		services: services,
	}
}
