/*
credit: https://github.com/containrrr/watchtower
*/
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/distribution/reference"
)

type TokenResponse struct {
	Token string `json:"token"`
}

// ChallengeHeader is the HTTP Header containing challenge instructions
const ChallengeHeader = "WWW-Authenticate"

// GetChallenge fetches a challenge for the registry hosting the provided image.
//
// Takes a context because checking one image for updates makes THREE outbound
// registry calls — challenge, token, digest — and none of them used to carry
// one. Each was capped only by the shared client's 30 second timeout, so a
// slow or rate-limited registry cost up to 90 seconds per image that nothing
// could cancel, inside a loop over every installed app.
func GetChallenge(ctx context.Context, imageName string) (string, error) {
	var err error
	var URL url.URL

	if URL, err = GetChallengeURL(imageName); err != nil {
		return "", err
	}

	var req *http.Request
	if req, err = GetChallengeRequest(ctx, URL); err != nil {
		return "", err
	}

	client := httpClient()
	var res *http.Response
	if res, err = client.Do(req); err != nil {
		return "", err
	}
	defer res.Body.Close()

	v := res.Header.Get(ChallengeHeader)

	return strings.ToLower(v), nil
}

func GetToken(ctx context.Context, challenge string, registryAuth string, imageName string) (string, error) {
	if strings.HasPrefix(challenge, "basic") {
		if registryAuth == "" {
			return "", fmt.Errorf("no credentials available")
		}

		return fmt.Sprintf("Basic %s", registryAuth), nil
	}

	if strings.HasPrefix(challenge, "bearer") {
		return GetBearerHeader(ctx, challenge, imageName, registryAuth)
	}

	return "", errors.New("unsupported challenge type from registry")
}

// GetChallengeRequest creates a request for getting challenge instructions
func GetChallengeRequest(ctx context.Context, URL url.URL) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, URL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "CasaOS")
	return req, nil
}

// GetBearerHeader tries to fetch a bearer token from the registry based on the challenge instructions
func GetBearerHeader(ctx context.Context, challenge string, img string, registryAuth string) (string, error) {
	client := httpClient()
	if strings.Contains(img, ":") {
		img = strings.Split(img, ":")[0]
	}

	authURL, err := GetAuthURL(challenge, img)
	if err != nil {
		return "", err
	}

	var r *http.Request
	if r, err = http.NewRequestWithContext(ctx, http.MethodGet, authURL.String(), nil); err != nil {
		return "", err
	}

	if registryAuth != "" {
		r.Header.Add("Authorization", fmt.Sprintf("Basic %s", registryAuth))
	}

	var authResponse *http.Response
	if authResponse, err = client.Do(r); err != nil {
		return "", err
	}
	defer authResponse.Body.Close()

	tokenResponse := &TokenResponse{}

	body, err := io.ReadAll(authResponse.Body)
	if err != nil {
		return "", err
	}

	err = json.Unmarshal(body, tokenResponse)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Bearer %s", tokenResponse.Token), nil
}

// GetAuthURL from the instructions in the challenge
func GetAuthURL(challenge string, img string) (*url.URL, error) {
	loweredChallenge := strings.ToLower(challenge)
	raw := strings.TrimPrefix(loweredChallenge, "bearer")

	pairs := strings.Split(raw, ",")
	values := make(map[string]string, len(pairs))

	for _, pair := range pairs {
		trimmed := strings.Trim(pair, " ")
		kv := strings.Split(trimmed, "=")
		if len(kv) < 2 {
			continue // skip malformed pairs
		}
		key := kv[0]
		val := strings.Trim(kv[1], "\"")
		values[key] = val
	}

	if values["realm"] == "" || values["service"] == "" {
		return nil, fmt.Errorf("challenge header did not include all values needed to construct an auth url")
	}

	authURL, err := url.Parse(values["realm"])
	if err != nil {
		return nil, fmt.Errorf("invalid realm URL in challenge: %w", err)
	}
	if authURL.Scheme != "https" && authURL.Scheme != "http" {
		return nil, fmt.Errorf("realm URL must use http or https, got %q", authURL.Scheme)
	}
	q := authURL.Query()
	q.Add("service", values["service"])

	scopeImage := GetScopeFromImageName(img, values["service"])

	scope := fmt.Sprintf("repository:%s:pull", scopeImage)
	q.Add("scope", scope)

	authURL.RawQuery = q.Encode()
	return authURL, nil
}

// GetScopeFromImageName normalizes an image name for use as scope during auth and head requests
func GetScopeFromImageName(img, svc string) string {
	parts := strings.Split(img, "/")

	if len(parts) > 2 {
		if strings.Contains(svc, "docker.io") {
			return fmt.Sprintf("%s/%s", parts[1], strings.Join(parts[2:], "/"))
		}
		return strings.Join(parts, "/")
	}

	if len(parts) == 2 {
		if strings.Contains(parts[0], "docker.io") {
			return fmt.Sprintf("library/%s", parts[1])
		}
		return strings.Replace(img, svc+"/", "", 1)
	}

	if strings.Contains(svc, "docker.io") {
		return fmt.Sprintf("library/%s", parts[0])
	}
	return img
}

// GetChallengeURL creates a URL object based on the image info
func GetChallengeURL(img string) (url.URL, error) {
	normalizedNamed, err := reference.ParseNormalizedNamed(img)
	if err != nil {
		return url.URL{}, err
	}
	host, err := NormalizeRegistry(normalizedNamed.String())
	if err != nil {
		return url.URL{}, err
	}

	URL := url.URL{
		Scheme: "https",
		Host:   host,
		Path:   "/v2/",
	}
	return URL, nil
}
