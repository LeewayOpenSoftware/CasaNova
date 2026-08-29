package external

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
)

const (
	UserServiceAddressFilename = "user-service.url"
)

var (
	cachedPublicKey *ecdsa.PublicKey
	lastUpdate      time.Time
	publicKeyMu     sync.RWMutex
)

func GetPublicKey(runtimePath string) (*ecdsa.PublicKey, error) {
	publicKeyMu.RLock()
	if cachedPublicKey != nil && time.Since(lastUpdate) < 10*time.Second {
		key := cachedPublicKey
		publicKeyMu.RUnlock()
		return key, nil
	}
	publicKeyMu.RUnlock()

	publicKeyMu.Lock()
	defer publicKeyMu.Unlock()

	// Double-check after acquiring write lock
	if cachedPublicKey != nil && time.Since(lastUpdate) < 10*time.Second {
		return cachedPublicKey, nil
	}

	address, err := getAddress(filepath.Join(runtimePath, UserServiceAddressFilename))
	if err != nil {
		return nil, err
	}

	jwksURL, err := url.JoinPath(address, jwt.JWKSPath)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create JWKS request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch JWKS: received status code %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read JWKS response: %w", err)
	}

	var jwks jwt.JWKS
	err = json.Unmarshal(body, &jwks)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JWKS: %w", err)
	}

	// Use the first key in JWKS to validate the JWT
	if len(jwks.Keys) == 0 {
		return nil, fmt.Errorf("no keys found in JWKS")
	}

	key := jwks.Keys[0]
	xBytes, err := base64.RawURLEncoding.DecodeString(key.X)
	if err != nil {
		return nil, fmt.Errorf("failed to decode JWK x value: %w", err)
	}

	yBytes, err := base64.RawURLEncoding.DecodeString(key.Y)
	if err != nil {
		return nil, fmt.Errorf("failed to decode JWK y value: %w", err)
	}

	cachedPublicKey = &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}

	lastUpdate = time.Now()

	return cachedPublicKey, nil
}
