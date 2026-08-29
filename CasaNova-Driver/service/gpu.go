package service

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/model"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/pkg/pci"
)

// gpuVendorNames maps PCI vendor IDs to the display-vendor label used in the UI.
var gpuVendorNames = map[string]string{
	"10de": "NVIDIA",
	"1002": "AMD",
	"1022": "AMD",
	"8086": "Intel",
	"1a03": "ASPEED",
	"102b": "Matrox",
}

// GPUService detects display-class GPUs, their bound drivers, and (for NVIDIA)
// enriches from nvidia-smi. It also exposes the GPU driver-branch selector,
// mirroring the oracle's nvidia-driver-select flow.
type GPUService interface {
	List() ([]model.GPU, error)
	Select(load bool) (string, error)
	ActiveNvidiaBranch() string
}

type gpuService struct {
	ids *pci.DB
}

func NewGPUService(ids *pci.DB) GPUService { return &gpuService{ids: ids} }

func (g *gpuService) List() ([]model.GPU, error) {
	dev := NewDeviceService(g.ids)
	pciDevs, err := dev.ListPCI()
	if err != nil {
		return nil, err
	}

	// nvidia-smi enrichment keyed by index order (best-effort, non-fatal).
	var nvInfos []external.NvidiaGPUInfo
	if infos, err := external.NvidiaGPUInfoList(); err == nil {
		nvInfos = infos
	}
	branch := g.ActiveNvidiaBranch()

	gpus := make([]model.GPU, 0)
	nvSeen := 0
	for _, p := range pciDevs {
		// Display controllers: class high byte 03 (VGA/3D/Display).
		if !strings.HasPrefix(p.Class, "03") {
			continue
		}
		gpu := model.GPU{
			Slot:     p.Slot,
			Class:    p.Class,
			VendorID: p.VendorID,
			Vendor:   gpuVendorNames[p.VendorID],
			DeviceID: p.DeviceID,
			Product:  p.DeviceName,
			Driver:   p.Driver,
		}
		if gpu.Vendor == "" {
			gpu.Vendor = p.VendorName
		}
		if p.VendorID == "10de" {
			gpu.Branch = branch
			if nvSeen < len(nvInfos) {
				info := nvInfos[nvSeen]
				gpu.DriverVersion = info.DriverVersion
				gpu.UUID = info.UUID
				gpu.Serial = info.GPUSerial
				if gpu.Product == "" {
					gpu.Product = info.Name
				}
				nvSeen++
			}
		}
		gpus = append(gpus, gpu)
	}
	return gpus, nil
}

// ActiveNvidiaBranch reads NVIDIA_BRANCH from the driver-select env file written
// by the GPU selector (generic path mirroring /etc/nvidia/driver.env).
func (g *gpuService) ActiveNvidiaBranch() string {
	for _, path := range []string{"/etc/nvidia/driver.env", "/etc/nvidia/driver.conf"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if v, ok := strings.CutPrefix(line, "NVIDIA_BRANCH="); ok {
				f.Close()
				return strings.Trim(v, `"`)
			}
		}
		f.Close()
	}
	return ""
}

// Select invokes the configured GPU driver-branch selector. When load is false
// the selector only writes userspace config (equivalent to --skip-modules).
func (g *gpuService) Select(load bool) (string, error) {
	bin := config.ServerInfo.GPUSelectBin
	if bin == "" {
		return "", fmt.Errorf("no GPU driver-select binary configured")
	}
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("GPU driver-select binary not found: %s", bin)
	}
	args := []string{}
	if load {
		args = append(args, "--load-modules")
	} else {
		args = append(args, "--skip-modules")
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("gpu driver-select: %w", err)
	}
	return string(out), nil
}
