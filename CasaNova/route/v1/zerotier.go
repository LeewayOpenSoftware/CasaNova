package v1

import (
	"encoding/json"
	"fmt"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova/common"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/httper"
	"github.com/labstack/echo/v4"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// zeroTierAllowedPathPrefixes is the allow-list of ZeroTier admin API paths
// that can be reached through /v1/zt/*. This prevents an authenticated
// client from using the proxy to hit unrelated endpoints on localhost or
// to pivot into the host's loopback services.
var zeroTierAllowedPathPrefixes = []string{
	"/status",
	"/network",
	"/controller",
	"/peer",
	"/moon",
}

// stripClientHeaders is the set of client-supplied headers we refuse to
// forward to the ZeroTier backend. Authorization and Cookie would leak
// the caller's CasaNova session to a different trust domain; Host and
// X-Forwarded-* would let the client influence the request framing.
var stripClientHeaders = map[string]bool{
	"Authorization":     true,
	"Cookie":            true,
	"Host":              true,
	"X-Forwarded-For":   true,
	"X-Forwarded-Host":  true,
	"X-Forwarded-Proto": true,
	"X-Real-Ip":         true,
	"X-Zt1-Auth":        true, // we set this ourselves from the secret file
}

// stripResponseHeaders are never mirrored back to the client — they would
// leak backend-specific cookies or auth material.
var stripResponseHeaders = map[string]bool{
	"Set-Cookie":    true,
	"Authorization": true,
	"X-Zt1-Auth":    true,
}

func ZerotierProxy(ctx echo.Context) error {
	// This is a blanket Any("/*url") reverse proxy onto the local ZeroTier
	// controller with its authtoken attached, and it carried no gate: every
	// verb, every controller path — creating and deleting networks, changing
	// membership, reading the identity — was reachable by an unauthenticated
	// caller through the gateway. Network configuration is PermSystemNetwork.
	if err := requirePerm(ctx, rbac.PermSystemNetwork); err != nil {
		return err
	}
	w := ctx.Response().Writer
	r := ctx.Request()

	port, err := os.ReadFile("/var/lib/zerotier-one/zerotier-one.port")
	if err != nil {
		// Previously the error was written to w and execution fell through,
		// leading to a second dial against an empty port — often a panic.
		// Return an echo error instead so the handler exits cleanly.
		return ctx.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "zerotier not running",
		})
	}

	// Path allow-list: the suffix after /v1/zt must start with a known
	// ZeroTier admin API prefix. This blocks a caller from using the proxy
	// to reach arbitrary localhost paths (e.g. /v1/zt/../etc-backdoor).
	path := strings.TrimPrefix(r.URL.Path, "/v1/zt")
	if path == "" {
		path = "/"
	}
	if !isAllowedZTPath(path) {
		return ctx.JSON(http.StatusForbidden, map[string]string{
			"error": "path not allowed through zerotier proxy",
		})
	}

	targetURL := fmt.Sprintf("http://localhost:%s%s", strings.TrimSpace(string(port)), path)
	req, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, map[string]string{
			"error": "build backend request failed",
		})
	}

	authToken, err := os.ReadFile("/var/lib/zerotier-one/authtoken.secret")
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, map[string]string{
			"error": "zerotier auth token unreadable",
		})
	}
	req.Header.Set("X-ZT1-AUTH", strings.TrimSpace(string(authToken)))

	// Forward ONLY content-negotiation headers from the client. Previously
	// we copied every header including Authorization and Cookie, which
	// would send the caller's CasaNova session into the ZeroTier process.
	for key, values := range r.Header {
		if stripClientHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}

	client := http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ctx.JSON(http.StatusBadGateway, map[string]string{
			"error": "zerotier backend unreachable",
		})
	}
	defer resp.Body.Close()

	// Mirror safe response headers only. Drop Set-Cookie / auth headers
	// so a backend response can't seed a cookie on the CasaNova origin.
	for key, values := range resp.Header {
		if stripResponseHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logger.Error("zerotier proxy copy body", zap.Error(err))
	}
	return nil
}

func isAllowedZTPath(p string) bool {
	// Reject any traversal attempt before prefix matching.
	if strings.Contains(p, "..") {
		return false
	}
	for _, prefix := range zeroTierAllowedPathPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

func CheckNetwork() {
	logger.Info("start check network")
	respBody, err := httper.ZTGet("/controller/network")
	if err != nil {
		logger.Error("get network error", zap.Error(err))
		return
	}
	networkId := ""
	address := ""
	networkNames := gjson.ParseBytes(respBody).Array()
	routers := ""
	for _, v := range networkNames {
		res, err := httper.ZTGet("/controller/network/" + v.Str)
		if err != nil {
			logger.Error("get network error", zap.Error(err))
			return
		}
		name := gjson.GetBytes(res, "name").Str
		if name == common.RANW_NAME {
			networkId = gjson.GetBytes(res, "id").Str
			routers = gjson.GetBytes(res, "routes.0.target").Str
			break
		}
	}
	ip, s, e, c := getZTIP(routers)
	logger.Info("ip", zap.Any("ip", ip))
	if len(networkId) == 0 {
		if len(address) == 0 {
			address = GetAddress()
		}
		networkId = CreateNet(address, s, e, c)
	}
	res, err := httper.ZTGet("/network")
	if err != nil {
		logger.Error("get network error", zap.Error(err))
		return
	}
	joined := false
	networks := gjson.GetBytes(res, "#.id").Array()
	for _, v := range networks {
		if v.Str == networkId {
			joined = true
			break
		}
	}
	logger.Info("joined", zap.Any("joined", joined))
	if !joined {
		JoinAndUpdateNet(address, networkId, ip)
	}
}

func GetAddress() string {
	nodeRes, err := httper.ZTGet("/status")
	if err != nil {
		logger.Error("get status error", zap.Error(err))
		return ""
	}
	return gjson.GetBytes(nodeRes, "address").String()
}

func JoinAndUpdateNet(address, networkId, ip string) {
	logger.Info("start join network", zap.Any("ip", ip))
	_, err := httper.ZTPost("/network/"+networkId, "")
	if err != nil {
		logger.Error(" get network error", zap.Error(err))
		return
	}

	if len(address) == 0 {
		address = GetAddress()
	}
	type ztMemberAuth struct {
		Authorized    bool     `json:"authorized"`
		ActiveBridge  bool     `json:"activeBridge"`
		IPAssignments []string `json:"ipAssignments"`
	}
	payload := ztMemberAuth{Authorized: true, ActiveBridge: true, IPAssignments: []string{ip}}
	b, err := json.Marshal(payload)
	if err != nil {
		logger.Error("marshal member auth payload error", zap.Error(err))
		return
	}
	_, err = httper.ZTPost("/controller/network/"+networkId+"/member/"+address, string(b))
	if err != nil {
		logger.Error("join network error", zap.Error(err))
		return
	}
}

func CreateNet(address, s, e, c string) string {
	type ztAssignMode struct {
		ZT bool `json:"zt"`
	}
	type ztIPPool struct {
		IPRangeStart string `json:"ipRangeStart"`
		IPRangeEnd   string `json:"ipRangeEnd"`
	}
	type ztRoute struct {
		Target string `json:"target"`
	}
	type ztRule struct {
		EtherType int    `json:"etherType,omitempty"`
		Not       bool   `json:"not,omitempty"`
		Or        bool   `json:"or,omitempty"`
		Type      string `json:"type"`
	}
	type ztV6AssignMode struct {
		RFC4193 bool `json:"rfc4193"`
	}
	type ztNetConfig struct {
		Name              string         `json:"name"`
		Private           bool           `json:"private"`
		V4AssignMode      ztAssignMode   `json:"v4AssignMode"`
		IPAssignmentPools []ztIPPool     `json:"ipAssignmentPools"`
		Routes            []ztRoute      `json:"routes"`
		Rules             []ztRule       `json:"rules"`
		V6AssignMode      ztV6AssignMode `json:"v6AssignMode"`
	}
	payload := ztNetConfig{
		Name:         common.RANW_NAME,
		Private:      false,
		V4AssignMode: ztAssignMode{ZT: true},
		IPAssignmentPools: []ztIPPool{
			{IPRangeStart: s, IPRangeEnd: e},
		},
		Routes: []ztRoute{
			{Target: c},
		},
		Rules: []ztRule{
			{EtherType: 2048, Not: true, Or: false, Type: "MATCH_ETHERTYPE"},
			{EtherType: 2054, Not: true, Or: false, Type: "MATCH_ETHERTYPE"},
			{EtherType: 34525, Not: true, Or: false, Type: "MATCH_ETHERTYPE"},
			{Type: "ACTION_DROP"},
			{Type: "ACTION_ACCEPT"},
		},
		V6AssignMode: ztV6AssignMode{RFC4193: true},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Error("marshal network config error", zap.Error(err))
		return ""
	}
	createRes, err := httper.ZTPost("/controller/network/"+address+"______", string(body))
	if err != nil {
		logger.Error("post network error", zap.Error(err))
		return ""
	}
	return gjson.GetBytes(createRes, "id").Str
}

func GetZTIPs() []gjson.Result {
	res, err := httper.ZTGet("/network")
	if err != nil {
		logger.Error("get network error", zap.Error(err))
		return []gjson.Result{}
	}
	a := gjson.GetBytes(res, "#.routes.0.target")
	return a.Array()
}

func getZTIP(routes string) (ip, start, end, cidr string) {
	excluded := GetZTIPs()
	cidrs := []string{
		"10.147.11.0/24",
		"10.147.12.0/24",
		"10.147.13.0/24",
		"10.147.14.0/24",
		"10.147.15.0/24",
		"10.147.16.0/24",
		"10.147.17.0/24",
		"10.147.18.0/24",
		"10.147.19.0/24",
		"10.147.20.0/24",
		"10.240.0.0/16",
		"10.241.0.0/16",
		"10.242.0.0/16",
		"10.243.0.0/16",
		"10.244.0.0/16",
		"10.245.0.0/16",
		"10.246.0.0/16",
		"10.247.0.0/16",
		"10.248.0.0/16",
		"10.249.0.0/16",
		"172.21.0.0/16",
		"172.22.0.0/16",
		"172.23.0.0/16",
		"172.24.0.0/16",
		"172.25.0.0/16",
		"172.26.0.0/16",
		"172.27.0.0/16",
		"172.28.0.0/16",
		"172.29.0.0/16",
		"172.30.0.0/16",
	}
	filteredCidrs := make([]string, 0)
	if len(routes) > 0 {
		filteredCidrs = append(filteredCidrs, routes)
	} else {
		for _, cidr := range cidrs {
			isExcluded := false
			for _, excludedIP := range excluded {
				if cidr == excludedIP.Str {
					isExcluded = true
					break
				}
			}
			if !isExcluded {
				filteredCidrs = append(filteredCidrs, cidr)
			}
		}
	}

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	ip = ""
	if len(filteredCidrs) > 0 {
		randomIndex := rnd.Intn(len(filteredCidrs))
		selectedCIDR := filteredCidrs[randomIndex]
		_, ipNet, err := net.ParseCIDR(selectedCIDR)
		if err != nil {
			logger.Error("ParseCIDR error", zap.Error(err))
			return
		}
		cidr = selectedCIDR
		startIP := ipNet.IP
		endIP := make(net.IP, len(startIP))
		copy(endIP, startIP)

		for i := range startIP {
			endIP[i] |= ^ipNet.Mask[i]
		}
		startIP[3] = 1
		start = startIP.String()
		endIP[3] = 254
		end = endIP.String()
		ipt := ipNet
		ipt.IP[3] = 1
		ip = ipt.IP.String()
		return
	} else {
		logger.Error("No available CIDR found")
	}
	return
}
