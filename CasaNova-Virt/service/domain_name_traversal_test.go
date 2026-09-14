package service

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
)

// NOTA DE SEGURIDAD: ningun test de aqui escribe fuera de su propio t.TempDir().
// El escape se demuestra construyendo la MISMA ruta que construye Define y
// midiendo que cae fuera de diskDir, sin crear ni un fichero.

// El escape es real: la ruta que Define arma con un Name con "../" sale de
// diskDir. Esto no toca el disco; solo mide la ruta.
func TestDomainNameEscapesDiskDirWithoutTheGuard(t *testing.T) {
	diskDir := filepath.Join(t.TempDir(), "disks")
	const hostile = "../escaped/vm"

	// Exactamente la expresion de service/domain.go: filepath.Join(s.diskDir,
	// fmt.Sprintf("%s-%d.qcow2", cfg.Name, i))
	full := filepath.Join(diskDir, fmt.Sprintf("%s-%d.qcow2", hostile, 0))
	rel, err := filepath.Rel(diskDir, full)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("la premisa es falsa: %q no escapa de %q (rel=%q)", hostile, diskDir, rel)
	}
	// Y el seed ISO, que usa la otra expresion: filepath.Join(s.diskDir, name+"-seed.iso")
	seed := filepath.Join(diskDir, hostile+"-seed.iso")
	if relSeed, _ := filepath.Rel(diskDir, seed); !strings.HasPrefix(relSeed, "..") {
		t.Fatalf("el seed ISO no escapa: %q", relSeed)
	}
}

// Define tiene que rechazar ese nombre, y rechazarlo ANTES de marcar libvirt:
// el servicio se construye con conn nil, asi que llegar a s.conn.Conn() entra
// en panico. Si el guard desaparece, este test cae con ese panico recuperado.
func TestDefineRefusesATraversalName(t *testing.T) {
	diskDir := filepath.Join(t.TempDir(), "disks")
	s := NewDomainService(nil, diskDir)

	for _, name := range []string{"../escaped/vm", "../../etc/cron.d/x", "/etc/shadow", "..", ".", "  ", "a/b"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Define(%q) llego a marcar libvirt: el nombre no se valido antes (panic=%v)", name, r)
				}
			}()
			_, err := s.Define(&model.VirtualMachineConfiguration{
				Name:  name,
				Disks: []model.DomainDisk{{SizeGiB: 1}},
			})
			if err == nil {
				t.Fatalf("Define(%q) no fallo: el nombre habria creado directorios fuera de diskDir", name)
			}
			if !strings.Contains(err.Error(), "invalid domain name") {
				t.Fatalf("Define(%q) fallo por otra razon, no por la validacion: %v", name, err)
			}
		}()
	}
}

// Anti-vacuidad: un nombre legitimo tiene que PASAR la validacion. Si este se
// pone rojo, el arreglo rompe la creacion de maquinas.
func TestSafeDiskDirNameAcceptsRealDomainNames(t *testing.T) {
	diskDir := filepath.Join(t.TempDir(), "disks")
	for _, name := range []string{"ubuntu-24.04", "vm1", "mi_maquina", "debian.test"} {
		got, err := safeDiskDirName(diskDir, name, "domain name")
		if err != nil || got != name {
			t.Fatalf("safeDiskDirName(%q) = %q, %v: rechaza un nombre legitimo", name, got, err)
		}
	}
}

// Lo que el guard de DeleteISO ya hacia tiene que seguir haciendolo: esta es la
// prueba de que extraerlo no aflojo nada.
func TestSafeDiskDirNameKeepsWhatDeleteISOAlreadyRefused(t *testing.T) {
	diskDir := filepath.Join(t.TempDir(), "disks")
	for _, name := range []string{"", ".", "..", "../x.iso", "/etc/passwd", "a/b.iso", "  ..  "} {
		if _, err := safeDiskDirName(diskDir, name, "ISO name"); err == nil {
			t.Fatalf("safeDiskDirName acepto %q", name)
		}
	}
}
