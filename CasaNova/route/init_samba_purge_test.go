package route

import (
	"errors"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/service"
	model2 "github.com/LeewayOpenSoftware/CasaNova/service/model"
)

// stubConns implements only what InitNetworkMount touches; every other method
// of the interface is inherited nil and would panic loudly if reached, which
// is the behaviour we want from a test double.
type stubConns struct {
	service.ConnectionsService
	list      []model2.ConnectionsDBModel
	deleted   []string
	unmounted []string
	unmountFn func(string) error
	probed    int
}

func (s *stubConns) GetConnectionsList() []model2.ConnectionsDBModel { return s.list }
func (s *stubConns) GetConnectionByID(id string) model2.ConnectionsDBModel {
	s.probed++
	for _, c := range s.list {
		if id == "1" {
			return c
		}
	}
	return model2.ConnectionsDBModel{}
}
func (s *stubConns) DeleteConnection(id string) { s.deleted = append(s.deleted, id) }
func (s *stubConns) UnmountSmaba(mp string) error {
	s.unmounted = append(s.unmounted, mp)
	if s.unmountFn != nil {
		return s.unmountFn(mp)
	}
	return nil
}

type stubStorage struct {
	service.StorageService
}

func (stubStorage) CheckAndMountAll() error { return nil }

type stubRepo struct {
	service.Repository
	conns   service.ConnectionsService
	storage service.StorageService
}

func (r stubRepo) Connections() service.ConnectionsService { return r.conns }
func (r stubRepo) Storage() service.StorageService         { return r.storage }

// Un fallo al PREGUNTAR por los recursos no es una respuesta de "no hay
// recursos". El arranque no puede borrar credenciales por no alcanzar el NAS.
func TestInitNetworkMountKeepsConnectionWhenShareListUnavailable(t *testing.T) {
	prevDelay := initNetworkMountDelay
	initNetworkMountDelay = 0
	t.Cleanup(func() { initNetworkMountDelay = prevDelay })

	conns := &stubConns{list: []model2.ConnectionsDBModel{
		// 127.0.0.1:1 refuses instantly: this is the "NAS not up yet" state,
		// produced without any network of our own.
		{ID: 1, Host: "127.0.0.1", Port: "1", Username: "u", Password: "p"},
	}}
	prev := service.MyService
	service.MyService = stubRepo{conns: conns, storage: stubStorage{}}
	t.Cleanup(func() { service.MyService = prev })

	done := make(chan struct{})
	go func() { defer close(done); InitNetworkMount() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("InitNetworkMount no termino")
	}

	// Anti-vacuidad: sin esto, un bucle que no corre imprime el mismo verde
	// que un bucle que corrio y decidio no borrar.
	if conns.probed == 0 {
		t.Fatal("el bucle no llego a mirar ninguna conexion: el verde seria vacio")
	}
	if len(conns.deleted) != 0 {
		t.Fatalf("la conexion guardada fue BORRADA por no poder consultar el NAS: %v", conns.deleted)
	}
}

// UnmountSmaba devuelve error y el codigo lo descartaba: con un recurso aun
// montado, borrar el directorio padre entra en el CIFS vivo.
func TestUnmountAllStaleReportsAShareThatWouldNotUnmount(t *testing.T) {
	conns := &stubConns{unmountFn: func(mp string) error {
		if mp == "/mnt/nas/busy" {
			return errors.New("device or resource busy")
		}
		return nil
	}}
	if !unmountAllStale(conns, []string{"/mnt/nas/ok", "/mnt/nas/busy"}) {
		t.Fatal("un recurso que NO se desmonto se reporto como desmontado: el padre se habria borrado encima del CIFS vivo")
	}
	if unmountAllStale(conns, []string{"/mnt/nas/ok"}) {
		t.Fatal("falso positivo: todo se desmonto y aun asi se frena la limpieza")
	}
}
