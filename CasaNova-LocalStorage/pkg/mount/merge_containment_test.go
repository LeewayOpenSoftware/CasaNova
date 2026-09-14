package mount

import "testing"

// InitMerge/SetMerge MUEVEN el directorio que reciben. Fuera de las raices
// permitidas eso se lleva por delante el sistema anfitrion.
func TestValidateMergePointRefusesHostDirectories(t *testing.T) {
	for _, mp := range []string{"/etc", "/root", "/home", "/", "/usr/lib", "/var/lib/casaos", "relativo", "/DATA/../etc"} {
		if err := ValidateMergePoint(mp); err == nil {
			t.Fatalf("ValidateMergePoint(%q) = nil: InitMerge habria movido ese directorio", mp)
		}
	}
}

// GUARDA DE REGRESION, y es la que importa: /DATA es el punto de merge real
// (common.DefaultMountPoint). ValidatePoint lo RECHAZA por ser una raiz, asi
// que "arreglarlo" reusando ValidatePoint aqui rompe la inicializacion del
// almacenamiento entera. Si alguien colapsa las dos funciones, este test cae.
func TestValidateMergePointAcceptsTheRootsThemselves(t *testing.T) {
	for _, mp := range []string{"/DATA", "/DATA/pool", "/media", "/mnt", "/mnt/pool"} {
		if err := ValidateMergePoint(mp); err != nil {
			t.Fatalf("ValidateMergePoint(%q) = %v: rompe el punto de merge legitimo", mp, err)
		}
	}
	// Y la asimetria queda fijada explicitamente, para que nadie la borre por
	// "duplicacion": el validador de MONTAJE si rechaza la raiz.
	if ValidatePoint("/DATA") == nil {
		t.Fatal("ValidatePoint acepta /DATA: la razon de que ValidateMergePoint exista ha desaparecido, revisar las dos")
	}
}
