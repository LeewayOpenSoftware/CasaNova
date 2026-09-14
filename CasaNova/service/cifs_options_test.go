package service

import (
	"errors"
	"strings"
	"testing"
)

func TestCIFSMountOptionsCarriesTheRequestedPort(t *testing.T) {
	opts, err := cifsMountOptions("u", "p", "10445")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opts, "port=10445") {
		t.Fatalf("el puerto pedido se perdio: %q", opts)
	}
	// 445 y vacio mantienen el comportamiento anterior exacto.
	for _, port := range []string{"", "445"} {
		opts, err := cifsMountOptions("u", "p", port)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(opts, "port=") {
			t.Fatalf("port=%q añadio una opcion que antes no estaba: %q", port, opts)
		}
	}
}

func TestCIFSMountOptionsRefusesACommaItCannotEscape(t *testing.T) {
	if _, err := cifsMountOptions("u", "clave,secundaria", ""); !errors.Is(err, ErrCIFSOptionUnrepresentable) {
		t.Fatalf("una contrasena con coma se mando al kernel sin avisar: err=%v", err)
	}
	if _, err := cifsMountOptions("us,er", "p", ""); !errors.Is(err, ErrCIFSOptionUnrepresentable) {
		t.Fatalf("un usuario con coma se mando al kernel sin avisar: err=%v", err)
	}
	if _, err := cifsMountOptions("u", "p", "1;rw"); err == nil {
		t.Fatal("un puerto no numerico se habria convertido en opciones de montaje extra")
	}
}
