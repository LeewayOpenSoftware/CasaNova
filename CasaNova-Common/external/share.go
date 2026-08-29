package external

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	http2 "github.com/LeewayOpenSoftware/CasaNova-Common/utils/http"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/internalauth"
)

const (
	APICasaOSShare = "/v1/samba/shares"
)

type ShareService interface {
	DeleteShare(id string) error
}
type shareService struct {
	addressFile string
}

func (n *shareService) DeleteShare(id string) error {
	if strings.ContainsAny(id, "/\\?&#\x00") || id == "" || id == "." || id == ".." {
		return fmt.Errorf("invalid share id: %q", id)
	}
	address, err := getAddress(n.addressFile)
	if err != nil {
		return err
	}

	url := strings.TrimSuffix(address, "/") + APICasaOSShare + "/" + id

	// Prove this is a peer service; the samba teardown endpoint used to trust
	// loopback and was reachable unauthenticated through the gateway.
	hdr, err := internalauth.Header()
	if err != nil {
		return err
	}
	response, err := http2.DeleteWithHeader(url, []byte("{}"), 30*time.Second, hdr)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return errors.New("failed to send share (status code: " + fmt.Sprint(response.StatusCode) + ")")
	}
	return nil
}

func NewShareService(runtimePath string) ShareService {
	return &shareService{
		addressFile: filepath.Join(runtimePath, CasaOSURLFilename),
	}
}
