package v2

import (
	"github.com/LeewayOpenSoftware/CasaNova/codegen"
	"github.com/LeewayOpenSoftware/CasaNova/service"
)

type CasaOS struct {
	fileUploadService *service.FileUploadService
}

func NewCasaOS() codegen.ServerInterface {
	s := service.NewFileUploadService()
	// Publish the running upload service so anything that needs the live view
	// of in-flight uploads (the maintenance scanner, the busy-check) reads the
	// same bitmaps these handlers write, under the same lock, instead of
	// falling back to a stale on-disk registry. Without this call
	// liveUploadService stays nil and every reader takes the file path — which
	// is exactly how a read-only busy-check could delete the registry.
	service.RegisterUploadService(s)
	return &CasaOS{
		fileUploadService: s,
	}
}
