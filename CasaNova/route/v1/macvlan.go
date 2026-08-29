package v1

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"time"
)

// Validation patterns for macvlan user input.
var (
	dockerNetworkNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	networkInterfaceRe  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
)

// ─── Macvlan types ─────────────────────────────────────────────────────────

type MacvlanConfig struct {
	NetworkName     string `json:"networkName"`
	ParentInterface string `json:"parentInterface"`
	Subnet          string `json:"subnet"`
	Gateway         string `json:"gateway"`
	IPRangeStart    string `json:"ipRangeStart"`
	IPRangeEnd      string `json:"ipRangeEnd"`
	AuxAddress      string `json:"auxAddress"`
}

type AppIPAssignment struct {
	AppID     string `json:"appId"`
	AppName   string `json:"appName"`
	IconURL   string `json:"iconUrl"`
	Enabled   bool   `json:"enabled"`
	IPAddress string `json:"ipAddress"`
}

type MacvlanState struct {
	Enabled     bool              `json:"enabled"`
	Config      MacvlanConfig     `json:"config"`
	Created     bool              `json:"created"`
	Assignments []AppIPAssignment `json:"assignments"`
}

// ─── Persistence ───────────────────────────────────────────────────────────

var (
	macvlanMu       sync.RWMutex
	macvlanState    MacvlanState
	macvlanConfPath = "/var/lib/casanova/macvlan.json"
)

func init() {
	macvlanState = MacvlanState{
		Config: MacvlanConfig{
			NetworkName:     "casanova-macvlan",
			ParentInterface: "eth0",
			Subnet:          "192.168.1.0/24",
			Gateway:         "192.168.1.1",
			IPRangeStart:    "192.168.1.200",
			IPRangeEnd:      "192.168.1.250",
		},
		Assignments: []AppIPAssignment{},
	}
	loadMacvlanState()
}

func loadMacvlanState() {
	data, err := os.ReadFile(macvlanConfPath)
	if err != nil {
		return
	}
	var s MacvlanState
	if err := json.Unmarshal(data, &s); err == nil {
		macvlanState = s
	}
}

func saveMacvlanState() error {
	data, err := json.MarshalIndent(macvlanState, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll("/var/lib/casanova", 0755)
	// Wave 35-X: atomic write — corrupt macvlan config blocks docker
	// network attach for every macvlan-using container at next boot.
	return commonfile.WriteFileAtomic(macvlanConfPath, data, 0644)
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// dockerNetworkExists checks if a Docker network exists by name.
func dockerNetworkExists(name string) bool {
	if !dockerNetworkNameRe.MatchString(name) {
		logger.Error("invalid network name", zap.String("name", name))
		return false
	}
	out, err := execOutBounded(10*time.Second, "docker", "network", "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == name
}

// listDockerContainers returns running container IDs and names.
func listDockerContainers() []AppIPAssignment {
	out, err := execOutBounded(10*time.Second, "docker", "ps", "--format", "{{.ID}}|{{.Names}}|{{.Image}}")
	if err != nil {
		return nil
	}
	var apps []AppIPAssignment
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 2 {
			continue
		}
		apps = append(apps, AppIPAssignment{
			AppID:   parts[0][:12],
			AppName: parts[1],
			IconURL: "",
		})
	}
	return apps
}

// getContainerNetworks returns which networks a container is connected to.
func getContainerNetworks(containerID string) []string {
	out, err := execOutBounded(10*time.Second, "docker", "inspect", "--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", containerID)
	if err != nil {
		return nil
	}
	return strings.Fields(strings.TrimSpace(string(out)))
}

// getContainerIP returns the container's IP on a specific network.
func getContainerIP(containerID, networkName string) string {
	// Validate networkName to prevent Go template injection in --format.
	if !dockerNetworkNameRe.MatchString(networkName) {
		return ""
	}
	out, err := execOutBounded(10*time.Second, "docker", "inspect", "--format",
		fmt.Sprintf("{{(index .NetworkSettings.Networks \"%s\").IPAddress}}", networkName),
		containerID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// validateIP checks if a string is a valid IPv4 address.
func validateIP(ip string) bool {
	return net.ParseIP(ip) != nil
}

// getAvailableInterfaces lists network interfaces on the host.
func getAvailableInterfaces() []map[string]string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var result []map[string]string
	for _, iface := range ifaces {
		// Skip loopback, docker bridges, veth, etc.
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := iface.Name
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") ||
			strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "lo") {
			continue
		}
		label := name
		if strings.HasPrefix(name, "en") || strings.HasPrefix(name, "eth") {
			label = name + " — Ethernet"
		} else if strings.HasPrefix(name, "wl") {
			label = name + " — WiFi"
		}
		result = append(result, map[string]string{
			"value": name,
			"label": label,
		})
	}
	return result
}

// isPrivateSubnet checks whether the given network falls within a private IP range.
func isPrivateSubnet(subnet *net.IPNet) bool {
	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
	}
	for _, cidr := range privateRanges {
		_, privateNet, _ := net.ParseCIDR(cidr)
		if privateNet.Contains(subnet.IP) {
			return true
		}
	}
	return false
}

// ─── Handlers ──────────────────────────────────────────────────────────────

// GET /v1/sys/network/macvlan — Get macvlan configuration and per-app assignments.
func GetMacvlanConfig(ctx echo.Context) error {
	// Gated: network topology: interfaces, subnets and address ranges.
	if err := requirePerm(ctx, rbac.PermSystemNetwork); err != nil {
		return err
	}

	macvlanMu.RLock()
	state := macvlanState
	macvlanMu.RUnlock()

	// Check if the Docker network actually exists
	state.Created = dockerNetworkExists(state.Config.NetworkName)

	// Merge current running containers with saved assignments
	containers := listDockerContainers()
	if containers != nil {
		// Build a map of saved assignments
		saved := map[string]AppIPAssignment{}
		for _, a := range state.Assignments {
			saved[a.AppName] = a
		}
		// Merge: keep saved state, add new containers
		merged := make([]AppIPAssignment, 0, len(containers))
		for _, c := range containers {
			if s, ok := saved[c.AppName]; ok {
				// Check if actually connected and get current IP
				if state.Created {
					currentIP := getContainerIP(c.AppID, state.Config.NetworkName)
					if currentIP != "" {
						s.IPAddress = currentIP
						s.Enabled = true
					}
				}
				s.AppID = c.AppID // Update container ID (may change after restart)
				merged = append(merged, s)
			} else {
				merged = append(merged, c)
			}
		}
		state.Assignments = merged
	}

	// Add available interfaces
	ifaces := getAvailableInterfaces()

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"enabled":     state.Enabled,
			"config":      state.Config,
			"created":     state.Created,
			"assignments": state.Assignments,
			"interfaces":  ifaces,
		},
	})
}

// POST /v1/sys/network/macvlan — Create or update the macvlan network.
func PostMacvlanConfig(ctx echo.Context) error {
	// Network mutation was ungated — tokenless loopback could reach it.
	if err := requirePerm(ctx, rbac.PermSystemNetwork); err != nil {
		return err
	}
	var req struct {
		Enabled bool          `json:"enabled"`
		Config  MacvlanConfig `json:"config"`
	}
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}

	// Validate
	if req.Config.ParentInterface == "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "parentInterface is required",
		})
	}
	if req.Config.Subnet == "" || !strings.Contains(req.Config.Subnet, "/") {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "subnet must be in CIDR notation (e.g., 192.168.1.0/24)",
		})
	}
	if !validateIP(req.Config.Gateway) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "invalid gateway IP",
		})
	}

	macvlanMu.Lock()
	defer macvlanMu.Unlock()

	macvlanState.Enabled = req.Enabled
	macvlanState.Config = req.Config

	if req.Enabled {
		netName := req.Config.NetworkName
		if netName == "" {
			netName = "casanova-macvlan"
			macvlanState.Config.NetworkName = netName
		}

		// Validate all user-supplied values before passing to exec.Command.
		if !dockerNetworkNameRe.MatchString(netName) {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "invalid network name: use alphanumeric, dots, dashes, underscores only",
			})
		}
		if !networkInterfaceRe.MatchString(req.Config.ParentInterface) {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "invalid parent interface name",
			})
		}
		_, ipNet, err := net.ParseCIDR(req.Config.Subnet)
		if err != nil {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "invalid subnet CIDR: " + err.Error(),
			})
		}
		ones, _ := ipNet.Mask.Size()
		if ones < 16 {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "subnet prefix length must be /16 or more specific (reject overly broad subnets)",
			})
		}
		if !isPrivateSubnet(ipNet) {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "subnet must be in a private range (10.0.0.0/8, 172.16.0.0/12, or 192.168.0.0/16)",
			})
		}
		if net.ParseIP(req.Config.Gateway) == nil {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "invalid gateway IP address",
			})
		}
		if req.Config.IPRangeStart != "" && net.ParseIP(req.Config.IPRangeStart) == nil {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "invalid IP range start address",
			})
		}
		if req.Config.AuxAddress != "" && net.ParseIP(req.Config.AuxAddress) == nil {
			return ctx.JSON(http.StatusBadRequest, model.Result{
				Success: common_err.CLIENT_ERROR,
				Message: "invalid auxiliary address",
			})
		}

		// Remove existing network if it exists (to update config)
		if dockerNetworkExists(netName) {
			if out, err := execRunBounded(30*time.Second, "docker", "network", "rm", netName); err != nil {
				logger.Error("failed to remove existing macvlan network", zap.Error(err), zap.String("output", string(out)))
				// Network might be in use — try to proceed without removing
			}
		}

		// Create the macvlan network
		args := []string{
			"network", "create", "-d", "macvlan",
			"--subnet=" + req.Config.Subnet,
			"--gateway=" + req.Config.Gateway,
			"-o", "parent=" + req.Config.ParentInterface,
		}
		if req.Config.IPRangeStart != "" && req.Config.IPRangeEnd != "" {
			// Docker uses CIDR for ip-range, but we'll use the start IP with a /28 or similar
			// For simplicity, use --ip-range with the start address and a /24 scope
			args = append(args, "--ip-range="+req.Config.IPRangeStart+"/28")
		}
		if req.Config.AuxAddress != "" {
			args = append(args, "--aux-address", "host="+req.Config.AuxAddress)
		}
		args = append(args, netName)

		if out, err := execRunBounded(30*time.Second, "docker", args...); err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: fmt.Sprintf("Failed to create macvlan network: %s — %s", err.Error(), strings.TrimSpace(string(out))),
			})
		}

		macvlanState.Created = true
	} else {
		// Disable — remove the network
		netName := macvlanState.Config.NetworkName
		if dockerNetworkExists(netName) {
			// Disconnect all containers first
			for _, a := range macvlanState.Assignments {
				if a.Enabled {
					execRunBounded(30*time.Second, "docker", "network", "disconnect", "-f", netName, a.AppID) //nolint:errcheck — best-effort cleanup
				}
			}
			execRunBounded(30*time.Second, "docker", "network", "rm", netName) //nolint:errcheck — best-effort cleanup
		}
		macvlanState.Created = false
		// Reset all assignments
		for i := range macvlanState.Assignments {
			macvlanState.Assignments[i].Enabled = false
		}
	}

	if err := saveMacvlanState(); err != nil {
		logger.Error("failed to save macvlan state", zap.Error(err))
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    macvlanState,
	})
}

// PUT /v1/sys/network/macvlan/app/:id — Toggle macvlan for a specific app.
func PutMacvlanApp(ctx echo.Context) error {
	// Network mutation was ungated — tokenless loopback could reach it.
	if err := requirePerm(ctx, rbac.PermSystemNetwork); err != nil {
		return err
	}
	appID := ctx.Param("id")
	var req struct {
		Enabled   bool   `json:"enabled"`
		IPAddress string `json:"ipAddress"`
	}
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}

	macvlanMu.Lock()
	defer macvlanMu.Unlock()

	if !macvlanState.Created {
		return ctx.JSON(http.StatusPreconditionFailed, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "Macvlan network not created yet",
		})
	}

	netName := macvlanState.Config.NetworkName

	// Find the assignment
	found := false
	for i, a := range macvlanState.Assignments {
		if a.AppID == appID || a.AppName == appID {
			found = true
			if req.Enabled {
				if req.IPAddress != "" && !validateIP(req.IPAddress) {
					return ctx.JSON(http.StatusBadRequest, model.Result{
						Success: common_err.INVALID_PARAMS,
						Message: "invalid IP address",
					})
				}
				// Disconnect first if already connected
				execRunBounded(30*time.Second, "docker", "network", "disconnect", netName, a.AppID) //nolint:errcheck — best-effort cleanup

				// Connect with static IP
				connectArgs := []string{"network", "connect"}
				if req.IPAddress != "" {
					connectArgs = append(connectArgs, "--ip", req.IPAddress)
				}
				connectArgs = append(connectArgs, netName, a.AppID)

				if out, err := execRunBounded(30*time.Second, "docker", connectArgs...); err != nil {
					return ctx.JSON(http.StatusInternalServerError, model.Result{
						Success: common_err.SERVICE_ERROR,
						Message: fmt.Sprintf("Failed to connect container: %s — %s", err.Error(), strings.TrimSpace(string(out))),
					})
				}

				macvlanState.Assignments[i].Enabled = true
				macvlanState.Assignments[i].IPAddress = req.IPAddress
			} else {
				// Disconnect from macvlan
				execRunBounded(30*time.Second, "docker", "network", "disconnect", netName, a.AppID) //nolint:errcheck — best-effort cleanup
				macvlanState.Assignments[i].Enabled = false
				macvlanState.Assignments[i].IPAddress = ""
			}
			break
		}
	}

	if !found {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "App not found: " + appID,
		})
	}

	if err := saveMacvlanState(); err != nil {
		logger.Error("failed to save macvlan state", zap.Error(err))
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// DELETE /v1/sys/network/macvlan — Tear down the macvlan network entirely.
func DeleteMacvlanConfig(ctx echo.Context) error {
	// Network mutation was ungated — tokenless loopback could reach it.
	if err := requirePerm(ctx, rbac.PermSystemNetwork); err != nil {
		return err
	}
	macvlanMu.Lock()
	defer macvlanMu.Unlock()

	netName := macvlanState.Config.NetworkName
	if dockerNetworkExists(netName) {
		// Disconnect all containers first
		for _, a := range macvlanState.Assignments {
			if a.Enabled {
				execRunBounded(30*time.Second, "docker", "network", "disconnect", "-f", netName, a.AppID) //nolint:errcheck — best-effort cleanup
			}
		}
		if out, err := execRunBounded(30*time.Second, "docker", "network", "rm", netName); err != nil {
			return ctx.JSON(http.StatusInternalServerError, model.Result{
				Success: common_err.SERVICE_ERROR,
				Message: fmt.Sprintf("Failed to remove network: %s — %s", err.Error(), string(out)),
			})
		}
	}

	macvlanState.Enabled = false
	macvlanState.Created = false
	for i := range macvlanState.Assignments {
		macvlanState.Assignments[i].Enabled = false
		macvlanState.Assignments[i].IPAddress = ""
	}

	if err := saveMacvlanState(); err != nil {
		logger.Error("failed to save macvlan state", zap.Error(err))
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "Macvlan network removed",
	})
}
