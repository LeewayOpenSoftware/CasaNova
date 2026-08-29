package service

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SensorService reads temperature/fan sensors straight from hwmon (and the
// thermal_zone fallback for ARM). 100% read-only — zero writes, zero exec,
// zero client input — so its risk surface is nil. A flaky sensor (EIO/ENODATA)
// is skipped, never fatal.
type SensorService interface {
	Read() (SensorReport, error)
}

const (
	sysfsHwmon   = "/sys/class/hwmon"
	sysfsThermal = "/sys/class/thermal"
)

type TempReading struct {
	Label   string   `json:"label"` // e.g. "Package id 0", "Core 0"
	Celsius float64  `json:"celsius"`
	CritC   *float64 `json:"critC,omitempty"`
}

type FanReading struct {
	Label string `json:"label"`
	RPM   int    `json:"rpm"`
}

type SensorChip struct {
	Name  string        `json:"name"` // hwmon "name" (coretemp, nvme, k10temp…)
	Temps []TempReading `json:"temps"`
	Fans  []FanReading  `json:"fans"`
}

type SensorReport struct {
	Chips     []SensorChip `json:"chips"`
	Supported bool         `json:"supported"`
}

type sensorService struct{}

func NewSensorService() SensorService { return &sensorService{} }

func (s *sensorService) Read() (SensorReport, error) {
	report := SensorReport{Chips: []SensorChip{}}
	entries, err := os.ReadDir(sysfsHwmon)
	if err == nil {
		for _, e := range entries {
			dir := filepath.Join(sysfsHwmon, e.Name())
			chip := readHwmonChip(dir)
			if chip.Name != "" && (len(chip.Temps) > 0 || len(chip.Fans) > 0) {
				report.Chips = append(report.Chips, chip)
			}
		}
	}
	// ARM / thermal-zone fallback when hwmon yielded no temps.
	if len(report.Chips) == 0 {
		if chip := readThermalZones(); len(chip.Temps) > 0 {
			report.Chips = append(report.Chips, chip)
		}
	}
	report.Supported = len(report.Chips) > 0
	return report, nil
}

func readHwmonChip(dir string) SensorChip {
	chip := SensorChip{Name: readSysAttr(dir, "name")}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return chip
	}
	// Collect temp<N>_input and fan<N>_input, pairing with their _label.
	for _, e := range entries {
		n := e.Name()
		switch {
		case strings.HasPrefix(n, "temp") && strings.HasSuffix(n, "_input"):
			idx := strings.TrimSuffix(strings.TrimPrefix(n, "temp"), "_input")
			milli := readIntAttr(dir, n)
			if milli == 0 && readSysAttr(dir, n) == "" {
				continue // sensor returned EIO/absent — skip
			}
			t := TempReading{Celsius: float64(milli) / 1000.0}
			t.Label = readSysAttr(dir, "temp"+idx+"_label")
			if t.Label == "" {
				t.Label = "temp" + idx
			}
			if crit := readIntAttr(dir, "temp"+idx+"_crit"); crit > 0 {
				c := float64(crit) / 1000.0
				t.CritC = &c
			}
			chip.Temps = append(chip.Temps, t)
		case strings.HasPrefix(n, "fan") && strings.HasSuffix(n, "_input"):
			idx := strings.TrimSuffix(strings.TrimPrefix(n, "fan"), "_input")
			rpm := readIntAttr(dir, n)
			f := FanReading{RPM: rpm}
			f.Label = readSysAttr(dir, "fan"+idx+"_label")
			if f.Label == "" {
				f.Label = "fan" + idx
			}
			chip.Fans = append(chip.Fans, f)
		}
	}
	sort.Slice(chip.Temps, func(i, j int) bool { return chip.Temps[i].Label < chip.Temps[j].Label })
	return chip
}

func readThermalZones() SensorChip {
	chip := SensorChip{Name: "thermal"}
	matches, _ := filepath.Glob(filepath.Join(sysfsThermal, "thermal_zone*"))
	for _, z := range matches {
		milli := readIntAttr(z, "temp")
		if milli <= 0 {
			continue
		}
		label := readSysAttr(z, "type")
		if label == "" {
			label = filepath.Base(z)
		}
		chip.Temps = append(chip.Temps, TempReading{Label: label, Celsius: float64(milli) / 1000.0})
	}
	return chip
}

func readIntAttr(dir, name string) int {
	v := readSysAttr(dir, name)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}
