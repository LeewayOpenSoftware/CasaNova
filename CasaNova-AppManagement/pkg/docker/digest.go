/*
credit: https://github.com/containrrr/watchtower
*/
package docker

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Docker manifest media types
const (
	MediaTypeManifestV2     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeManifestListV2 = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// RegistryCredentials is a credential pair used for basic auth
type RegistryCredentials struct {
	Username string
	Password string // usually a token rather than an actual password
}

// ContentDigestHeader is the key for the key-value pair containing the digest header
const ContentDigestHeader = "Docker-Content-Digest"

// ManifestList represents a Docker manifest list (fat manifest)
type ManifestList struct {
	SchemaVersion int                `json:"schemaVersion"`
	MediaType     string             `json:"mediaType"`
	Manifests     []ManifestPlatform `json:"manifests"`
}

// ManifestPlatform represents a single platform entry in a manifest list
type ManifestPlatform struct {
	MediaType string   `json:"mediaType"`
	Digest    string   `json:"digest"`
	Size      int64    `json:"size"`
	Platform  Platform `json:"platform"`
}

// Platform describes the platform a manifest targets
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// CompareDigest ...
func CompareDigest(ctx context.Context, imageName string, repoDigests []string) (bool, error) {
	var digest string

	token, url, err := tokenAndURL(ctx, imageName)
	if err != nil {
		return false, err
	}

	if digest, err = GetDigest(ctx, url, token); err != nil {
		return false, err
	}

	for _, dig := range repoDigests {
		parts := strings.Split(dig, "@")
		if len(parts) < 2 {
			continue
		}
		localDigest := parts[1]

		if localDigest == digest {
			return true, nil
		}
	}

	return false, nil
}

// TransformAuth from a base64 encoded json object to base64 encoded string
func TransformAuth(registryAuth string) string {
	b, err := base64.StdEncoding.DecodeString(registryAuth)
	if err != nil {
		return registryAuth
	}
	credentials := &RegistryCredentials{}
	if err := json.Unmarshal(b, credentials); err != nil {
		return registryAuth
	}

	if credentials.Username != "" && credentials.Password != "" {
		ba := []byte(fmt.Sprintf("%s:%s", credentials.Username, credentials.Password))
		registryAuth = base64.StdEncoding.EncodeToString(ba)
	}

	return registryAuth
}

// GetDigest from registry using a HEAD request to prevent rate limiting
func GetDigest(ctx context.Context, url string, token string) (string, error) {
	if token == "" {
		return "", errors.New("could not fetch token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create HEAD request: %w", err)
	}
	addDefaultHeaders(&req.Header, token)

	res, err := httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		wwwAuthHeader := res.Header.Get("www-authenticate")
		if wwwAuthHeader == "" {
			wwwAuthHeader = "not present"
		}
		return "", fmt.Errorf("registry responded to head request with %q, auth: %q", res.Status, wwwAuthHeader)
	}

	// A 200 WITHOUT THE DIGEST HEADER IS NOT AN EMPTY DIGEST.
	//
	// Some registries and most caching proxies answer a HEAD with 200 and no
	// Docker-Content-Digest (pull-through caches, some Harbor and Nexus
	// configurations). This used to return ("", nil): a digest that matches
	// nothing. CompareDigest then compared "" against well-formed RepoDigests,
	// never matched, and answered false with no error — and its consumer reads
	// that as `!match`, i.e. "an update is available". Permanently. The app
	// wore an update badge forever and the nightly auto-update re-pulled the
	// same image every night, because the registry never actually answered the
	// question and nothing said so.
	digest := res.Header.Get(ContentDigestHeader)
	if digest == "" {
		return "", fmt.Errorf("registry answered %q without a %s header, so the image digest is unknown", res.Status, ContentDigestHeader)
	}
	return digest, nil
}

func GetManifest(ctx context.Context, imageName string) (interface{}, string, error) {
	token, url, err := tokenAndURL(ctx, imageName)
	if err != nil {
		return nil, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create GET request: %w", err)
	}
	addDefaultHeaders(&req.Header, token)

	res, err := httpClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("registry responded to manifest request with %q", res.Status)
	}

	contentType := res.Header.Get("Content-Type")

	buf, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, "", err
	}

	switch contentType {
	case MediaTypeManifestListV2, v1.MediaTypeImageIndex:
		var ml ManifestList
		if err := json.Unmarshal(buf, &ml); err != nil {
			return nil, contentType, fmt.Errorf("failed to unmarshal manifest list: %w", err)
		}
		return ml, contentType, nil

	case MediaTypeManifestV2, v1.MediaTypeImageManifest:
		var m map[string]interface{}
		if err := json.Unmarshal(buf, &m); err != nil {
			return nil, contentType, fmt.Errorf("failed to unmarshal manifest: %w", err)
		}
		return m, contentType, nil

	default:
		return nil, contentType, fmt.Errorf("unknown content type: %s", contentType)
	}
}

func tokenAndURL(ctx context.Context, imageName string) (string, string, error) {
	opts, err := GetPullOptions(imageName)
	if err != nil {
		return "", "", err
	}

	registryAuth := TransformAuth(opts.RegistryAuth)
	challenge, err := GetChallenge(ctx, imageName)
	if err != nil {
		return "", "", err
	}

	token, err := GetToken(ctx, challenge, registryAuth, imageName)
	if err != nil {
		return "", "", err
	}

	url, err := BuildManifestURL(imageName)
	if err != nil {
		return "", "", err
	}

	return token, url, nil
}

func httpClient() *http.Client {
	// The per-phase timeouts below only bound dialing and the TLS handshake.
	// A registry that completes both and then stops writing would hang the
	// caller forever, which is exactly how GET /appstore/upgradable wedged.
	// The overall Timeout is the backstop.
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		DisableKeepAlives:     true,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
	}}
}

func addDefaultHeaders(header *http.Header, token string) {
	header.Add("Authorization", token)
	header.Add("Accept", MediaTypeManifestListV2)
	header.Add("Accept", v1.MediaTypeImageIndex)
	header.Add("Accept", MediaTypeManifestV2)
	header.Add("Accept", v1.MediaTypeImageManifest)
}
