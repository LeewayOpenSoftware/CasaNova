package v1

import (
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

// VirtCapabilities describes the host's readiness for KVM virtualisation.
type VirtCapabilities struct {
	CPUVirt          bool   `json:"cpu_virt"`
	CPUVirtType      string `json:"cpu_virt_type"` // "vmx", "svm", or ""
	KVMAvailable     bool   `json:"kvm_available"`
	LibvirtInstalled bool   `json:"libvirt_installed"`
}

// GET /v1/sys/virt/capabilities — Check virtualisation support on the host.
func GetVirtCapabilities(ctx echo.Context) error {
	// Gated: PermSystemRead is held by guest, so requirePerm alone would allow a tokenless caller.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}

	caps := VirtCapabilities{}

	// Check CPU virtualisation flags in /proc/cpuinfo.
	cpuinfo, err := os.ReadFile("/proc/cpuinfo")
	if err == nil {
		content := string(cpuinfo)
		if strings.Contains(content, " vmx") || strings.Contains(content, " vmx ") {
			caps.CPUVirt = true
			caps.CPUVirtType = "vmx"
		} else if strings.Contains(content, " svm") || strings.Contains(content, " svm ") {
			caps.CPUVirt = true
			caps.CPUVirtType = "svm"
		}
		// Also check in the flags line more precisely.
		for _, line := range strings.Split(content, "\n") {
			if !strings.HasPrefix(line, "flags") {
				continue
			}
			fields := strings.Fields(line)
			for _, f := range fields {
				if f == "vmx" {
					caps.CPUVirt = true
					caps.CPUVirtType = "vmx"
				} else if f == "svm" {
					caps.CPUVirt = true
					caps.CPUVirtType = "svm"
				}
			}
		}
	}

	// Check if /dev/kvm exists.
	if _, err := os.Stat("/dev/kvm"); err == nil {
		caps.KVMAvailable = true
	}

	// Check if libvirtd is installed.
	if _, err := exec.LookPath("libvirtd"); err == nil {
		caps.LibvirtInstalled = true
	} else if _, err := exec.LookPath("virsh"); err == nil {
		caps.LibvirtInstalled = true
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    caps,
	})
}
