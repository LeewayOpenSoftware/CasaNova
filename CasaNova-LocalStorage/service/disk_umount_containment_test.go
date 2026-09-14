package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/model"
)

// NOTA DE SEGURIDAD DEL TEST: no se desmonta ni se formatea NADA real. El caso
// rojo se demuestra con un directorio propio bajo t.TempDir() -- que esta FUERA
// de /mnt, /media y /DATA exactamente igual que /boot/efi -- y se comprueba que
// SIGUE EXISTIENDO despues de la llamada. Que el directorio sobreviva es la
// prueba de que la negativa ocurrio ANTES del umount y del os.RemoveAll.
func TestUmountPointAndRemoveDirRefusesAMountPointOutsideTheAllowedRoots(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "boot-efi-stand-in")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(victim, "grubx64.efi")
	if err := os.WriteFile(canary, []byte("no me borres"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &diskService{}
	err := d.UmountPointAndRemoveDir(model.LSBLKModel{Path: "/dev/sda1", MountPoint: victim})
	if err == nil {
		t.Fatal("acepto desmontar y borrar un punto de montaje fuera de las raices permitidas")
	}
	if !errors.Is(err, ErrMountPointNotOurs) {
		t.Fatalf("fallo por otra razon, no por la contencion: %v", err)
	}
	if _, statErr := os.Stat(canary); statErr != nil {
		t.Fatalf("el fichero fue BORRADO pese al error: la negativa llego tarde (%v)", statErr)
	}
}

// La misma negativa tiene que cubrir a los HIJOS, que es por donde entraba el
// desastre real: se formatea /dev/sda y la victima es la particion /boot/efi.
func TestUmountPointAndRemoveDirRefusesAChildOutsideTheAllowedRoots(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "child-boot-efi")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	d := &diskService{}
	err := d.UmountPointAndRemoveDir(model.LSBLKModel{
		Path: "/dev/sda",
		Children: []model.LSBLKModel{
			{Path: "/dev/sda1", MountPoint: victim},
		},
	})
	if !errors.Is(err, ErrMountPointNotOurs) {
		t.Fatalf("un hijo fuera de las raices permitidas no fue rechazado: %v", err)
	}
	if _, statErr := os.Stat(victim); statErr != nil {
		t.Fatalf("el directorio del hijo fue borrado pese al error: %v", statErr)
	}
}

// Contraprueba: un punto de montaje que CasaNova SI pudo crear (/media/...)
// pasa la contencion. No se desmonta nada porque la ruta no existe; lo que se
// afirma es que el fallo, si lo hay, YA NO es ErrMountPointNotOurs.
func TestUmountPointAndRemoveDirDoesNotBlockAMountPointCasaNovaCouldOwn(t *testing.T) {
	d := &diskService{}
	err := d.UmountPointAndRemoveDir(model.LSBLKModel{
		Path:       "/dev/sdz9",
		MountPoint: "/media/Storage_stanley_containment_probe_does_not_exist",
	})
	if errors.Is(err, ErrMountPointNotOurs) {
		t.Fatal("la contencion rechaza una ruta legitima de CasaNova: rompe al que monta en /media")
	}
}
