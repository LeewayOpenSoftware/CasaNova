package external

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	http2 "github.com/LeewayOpenSoftware/CasaNova-Common/utils/http"
)

func getAddress(addressFile string) (string, error) {
	buf, err := os.ReadFile(addressFile)
	if err != nil {
		return "", err
	}

	// Wave 35-X: reject empty / whitespace-only address files. A
	// crash mid-write (pre Wave-35-X atomic writes) used to leave
	// zero-byte address files visible briefly. Callers built URLs
	// like "http:///ping" which failed deep inside net/http with
	// cryptic errors; the address-file consumer cached an empty
	// string and kept failing until daemon restart. Return a clear
	// error so the cache layer above can re-read on the next call.
	addr := strings.TrimSpace(string(buf))
	if addr == "" {
		return "", errors.New("address file is empty: " + addressFile)
	}
	return addr, nil
}

func ping(address string, timeout time.Duration) error {
	response, err := http2.Get(address+"/ping", timeout)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return errors.New("failed to ping the service as address " + address)
	}

	return nil
}
