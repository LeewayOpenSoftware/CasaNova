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

// publicKeyTTL: the signing key rotates once per user-service boot, so a
// 10-second TTL bought nothing except an HTTP fetch every 10 s from every
// service that validates tokens — five services × 6/min, all against one
// endpoint, under a write lock.
const publicKeyTTL = 15 * time.Minute

func GetPublicKey(runtimePath string) (*ecdsa.PublicKey, error) {
	publicKeyMu.RLock()
	if cachedPublicKey != nil && time.Since(lastUpdate) < publicKeyTTL {
		key := cachedPublicKey
		publicKeyMu.RUnlock()
		return key, nil
	}
	publicKeyMu.RUnlock()

	publicKeyMu.Lock()
	defer publicKeyMu.Unlock()

	// Double-check after acquiring write lock
	if cachedPublicKey != nil && time.Since(lastUpdate) < publicKeyTTL {
		return cachedPublicKey, nil
	}

	// From here on, any failure serves the STALE key instead of failing:
	// this is a public verification key that rotates only when user-service
	// reboots. Returning an error here fails token validation in every
	// service at once — an auth outage — while returning a stale key merely
	// delays recognising a rotation until the next successful refresh.
	staleOr := func(err error) (*ecdsa.PublicKey, error) {
		if cachedPublicKey != nil {
			return cachedPublicKey, nil
		}
		return nil, err
	}

	address, err := getAddress(filepath.Join(runtimePath, UserServiceAddressFilename))
	if err != nil {
		return staleOr(err)
	}

	jwksURL, err := url.JoinPath(address, jwt.JWKSPath)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return staleOr(fmt.Errorf("failed to create JWKS request: %w", err))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return staleOr(fmt.Errorf("failed to fetch JWKS: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch JWKS: received status code %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return staleOr(fmt.Errorf("failed to read JWKS response: %w", err))
	}

	var jwks jwt.JWKS
	err = json.Unmarshal(body, &jwks)
	if err != nil {
		return staleOr(fmt.Errorf("failed to parse JWKS: %w", err))
	}

	// Use the first key in JWKS to validate the JWT
	if len(jwks.Keys) == 0 {
		return staleOr(fmt.Errorf("no keys found in JWKS"))
	}

	key := jwks.Keys[0]
	xBytes, err := base64.RawURLEncoding.DecodeString(key.X)
	if err != nil {
		return staleOr(fmt.Errorf("failed to decode JWK x value: %w", err))
	}

	yBytes, err := base64.RawURLEncoding.DecodeString(key.Y)
	if err != nil {
		return staleOr(fmt.Errorf("failed to decode JWK y value: %w", err))
	}

	cachedPublicKey = &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}

	lastUpdate = time.Now()

	return cachedPublicKey, nil
}
