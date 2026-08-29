package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/constants"
	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	http2 "github.com/LeewayOpenSoftware/CasaNova-Common/utils/http"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/coreos/go-systemd/daemon"

	"github.com/LeewayOpenSoftware/CasaNova-Gateway/common"
	"github.com/LeewayOpenSoftware/CasaNova-Gateway/pkg/tlsmanager"
	"github.com/LeewayOpenSoftware/CasaNova-Gateway/route"
	"github.com/LeewayOpenSoftware/CasaNova-Gateway/service"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const localhost = "127.0.0.1"

var (
	commit = "private build"
	date   = "private build"

	_state *service.State

	serverMu    sync.Mutex
	_gateway    *http.Server
	_gatewayTLS *http.Server
	_tlsManager *tlsmanager.Manager

	_managementServiceReady = make(chan struct{}, 1)
	_gatewayServiceReady    = make(chan struct{}, 1)

	ErrCheckURLNotOK = errors.New("check url did not return 200 OK")

	//go:embed build/sysroot/etc/casaos/gateway.ini.sample
	_confSample string
)

func init() {
	versionFlag := flag.Bool("v", false, "version")
	wwwPathFlag := flag.String("w", filepath.Join(constants.DefaultDataPath, "www"), "www path")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("v%s\n", common.Version)
		os.Exit(0)
	}

	println("git commit:", commit)
	println("build date:", date)

	_state = service.NewState()

	// create default config file if not exist
	ConfigFilePath := filepath.Join(constants.DefaultConfigPath, common.GatewayName+"."+common.GatewayConfigType)
	if _, err := os.Stat(ConfigFilePath); os.IsNotExist(err) {
		fmt.Println("config file not exist, create it")
		tmpPath := ConfigFilePath + ".tmp"
		file, err := os.Create(tmpPath)
		if err != nil {
			panic(err)
		}
		if _, err = file.WriteString(_confSample); err != nil {
			file.Close()
			os.Remove(tmpPath)
			panic(err)
		}
		if err = file.Sync(); err != nil {
			file.Close()
			os.Remove(tmpPath)
			panic(err)
		}
		file.Close()
		if err = os.Rename(tmpPath, ConfigFilePath); err != nil {
			os.Remove(tmpPath)
			panic(err)
		}
	}

	config, err := common.LoadConfig()
	if err != nil {
		panic(err)
	}

	logger.LogInit(
		config.GetString(common.ConfigKeyLogPath),
		config.GetString(common.ConfigKeyLogSaveName),
		config.GetString(common.ConfigKeyLogFileExt),
	)

	runtimePath := config.GetString(common.ConfigKeyRuntimePath)
	if err := _state.SetRuntimePath(runtimePath); err != nil {
		logger.Error("Failed to set runtime path", zap.Any("error", err), zap.Any(common.ConfigKeyRuntimePath, runtimePath))
		panic(err)
	}

	gatewayPort := config.GetString(common.ConfigKeyGatewayPort)
	if err := _state.SetGatewayPort(gatewayPort); err != nil {
		logger.Error("Failed to set gateway port", zap.Any("error", err), zap.Any(common.ConfigKeyGatewayPort, gatewayPort))
		panic(err)
	}

	if err := _state.SetWWWPath(*wwwPathFlag); err != nil {
		logger.Error("Failed to set www path", zap.Any("error", err), zap.String("wwwpath", *wwwPathFlag))
		panic(err)
	}

	if err := checkPrequisites(_state); err != nil {
		logger.Error("Failed to check prequisites", zap.Any("error", err))
		panic(err)
	}

	_state.OnGatewayPortChange(func(port string) error {
		config.Set(common.ConfigKeyGatewayPort, port)
		return config.WriteConfig()
	})
}

func main() {
	// Establish the shared revocation authority before serving. It also
	// provisions itself on first consultation, so a service that never reaches
	// this line still enforces — but doing it here means a failure is logged
	// at boot instead of surfacing later as unexplained 401s.
	//
	// A failure is deliberately NOT fatal: this service must still come up so
	// its diagnostics endpoint can explain why every token is being refused.
	// It fails closed either way.
	if err := tokenrevocation.Ensure(); err != nil {
		logger.Error("token revocation authority is not in force; every token will be refused",
			zap.String("path", tokenrevocation.Path()),
			zap.String("invariant", tokenrevocation.CurrentHealth().Invariant),
			zap.Error(err))
	}

	pidFilename, err := writePidFile(_state.GetRuntimePath())
	if err != nil {
		logger.Error("Failed to write pid file to runtime path", zap.Any("error", err), zap.Any("runtimePath", _state.GetRuntimePath()))
		panic(err)
	}

	defer cleanupFiles(
		_state.GetRuntimePath(),
		pidFilename, external.ManagementURLFilename, external.StaticURLFilename,
	)

	defer func() {
		serverMu.Lock()
		gw := _gateway
		serverMu.Unlock()
		if gw != nil {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer shutdownCancel()
			if err := gw.Shutdown(shutdownCtx); err != nil {
				logger.Error("Failed to stop gateway", zap.Any("error", err))
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	kill := make(chan os.Signal, 1)
	signal.Notify(kill, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-kill
		cancel()
	}()

	go func() {
		select {
		case <-_managementServiceReady:
		case <-ctx.Done():
			return
		}
		select {
		case <-_gatewayServiceReady:
		case <-ctx.Done():
			return
		}

		if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
			logger.Error("Failed to notify systemd that gateway is ready", zap.Any("error", err))
		} else if supported {
			logger.Info("Notified systemd that gateway is ready")
		} else {
			logger.Info("This process is not running as a systemd service.")
		}
	}()

	app := fx.New(
		fx.Provide(func() *service.State { return _state }),
		fx.Provide(service.NewManagementService),
		fx.Provide(route.NewManagementRoute),
		fx.Provide(route.NewGatewayRoute),
		fx.Provide(route.NewStaticRoute),
		fx.Invoke(run),
	)

	if err := app.Start(ctx); err != nil {
		if err != context.Canceled {
			logger.Error("Failed to start gateway", zap.Any("error", err))
		}
		return
	}

	// Wave 35-R: block here until SIGTERM/SIGINT cancels ctx. Without this
	// wait, main() returns the moment fx finishes its OnStart hooks — the
	// deferred shutdown immediately calls _gateway.Shutdown(), the HTTP
	// server logs "A gateway is stopped", and the process exits with
	// status=0 within milliseconds of "Notified systemd that gateway is
	// ready". systemd then dutifully restarts us five times in a row,
	// trips StartLimitBurst, and the gateway is parked in the failed
	// state. Every downstream Wave-35-P unit (message-bus, user-service,
	// app-management, …) is `PartOf=casanova-gateway.service`, so the
	// whole stack collapses behind us.
	//
	// Block on ctx.Done() (closed by the SIGTERM/SIGINT handler above),
	// then run app.Stop with its own timeout context — using ctx for
	// Stop is wrong because ctx is already cancelled by the time we get
	// here, so every OnStop hook would receive a dead context and skip
	// its work.
	<-ctx.Done()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()
	if err := app.Stop(stopCtx); err != nil {
		logger.Error("Failed to stop gateway cleanly", zap.Error(err))
	}
}

func run(
	lifecycle fx.Lifecycle,
	management *service.Management,
	managementRoute *route.ManagementRoute,
	gatewayRoute *route.GatewayRoute,
	staticRoute *route.StaticRoute,
) {
	// Gateway wiring for the management endpoints
	// instance. Both routes are fx-constructed independently, so this
	// linker step has to happen after both exist.
	managementRoute.WithGateway(gatewayRoute)

	// management server
	lifecycle.Append(
		fx.Hook{
			OnStart: func(context.Context) error {
				listener, err := net.Listen("tcp", net.JoinHostPort(localhost, "0"))
				if err != nil {
					return err
				}

				managementServer := &http.Server{
					Handler:           managementRoute.GetRoute(),
					ReadHeaderTimeout: 5 * time.Second,
					ReadTimeout:       30 * time.Second,
					WriteTimeout:      30 * time.Second,
					IdleTimeout:       60 * time.Second,
					// Cap request headers at 64 KiB. Go's default of 1 MiB
					// is enough for an attacker to cheaply pin a goroutine
					// plus buffer per connection — exactly the attack class
					// ReadHeaderTimeout was added to mitigate but which only
					// a size cap actually bounds memory for.
					MaxHeaderBytes: 64 << 10,
				}

				urlFilePath, err := writeAddressFile(_state.GetRuntimePath(), external.ManagementURLFilename, "http://"+listener.Addr().String())
				if err != nil {
					return err
				}

				go func() {
					logger.Info("Management service is listening...",
						zap.Any("address", listener.Addr().String()),
						zap.Any("filepath", urlFilePath),
					)
					err := managementServer.Serve(listener)
					if err != nil && err != http.ErrServerClosed {
						logger.Error("management server error", zap.Any("error", err))
					}
				}()

				// Wave 42 (#8): do NOT publish the loopback management server as
				// a public gateway route. Previously `/v1/gateway/port` was
				// CreateRoute'd here, so the public :85 gateway proxied it to
				// the management server over loopback — and the management JWT
				// skipper trusts loopback, so an unauthenticated WAN/LAN caller
				// could `PUT /v1/gateway/port {"port":"9999"}` and rebind the
				// gateway off :85 (remote DoS + config change), while GET leaked
				// the listen port. The port API is NOT meant to be browser/WAN
				// reachable. Internal callers (main casanova's ChangePort/GetPort,
				// service route registration) reach the management server
				// DIRECTLY over its management.url loopback address, so they are
				// unaffected by removing this public bridge.

				_managementServiceReady <- struct{}{}

				return nil
			},
		},
	)

	// gateway service
	lifecycle.Append(
		fx.Hook{
			OnStart: func(ctx context.Context) error {
				gatewayMux := gatewayRoute.GetRoute()

				if _state.GetGatewayPort() == "" {
					// check if a port is available starting from port 80/8080
					portsToCheck := []int{}
					for i := 80; i < 90; i++ {
						portsToCheck = append(portsToCheck, i)
					}

					for i := 8080; i < 8090; i++ {
						portsToCheck = append(portsToCheck, i)
					}

					port := ""
					for _, p := range portsToCheck {
						port = fmt.Sprintf("%d", p)
						logger.Info("Checking if port is available...", zap.Any("port", port))
						if listener, err := net.Listen("tcp", net.JoinHostPort("", port)); err == nil {
							if err = listener.Close(); err != nil {
								logger.Error("Failed to close listener", zap.Any("error", err), zap.Any("port", port))
								continue
							}
							break
						}
					}

					if port == "" {
						return errors.New("No port available for gateway to use")
					}

					if err := _state.SetGatewayPort(port); err != nil {
						return err
					}
				}

				_state.OnGatewayPortChange(func(port string) error {
					return reloadGateway(port, gatewayMux)
				})

				if err := reloadGateway(_state.GetGatewayPort(), gatewayMux); err != nil {
					return err
				}

				// Wave 4c: TLS gateway. Loaded from tls.json if present,
				// otherwise stays off until the operator enables it via
				// the management API. The reload listener means future
				// SetTLS calls take effect without a service restart.
				if err := _state.LoadTLSFromDisk(); err != nil {
					logger.Error("failed to load tls settings", zap.Error(err))
				}
				_state.OnTLSChange(func(s service.TLSSettings) error {
					return reloadTLSGateway(s, gatewayMux)
				})
				if err := reloadTLSGateway(_state.GetTLS(), gatewayMux); err != nil {
					logger.Error("failed to start tls gateway", zap.Error(err))
					// Non-fatal: HTTP gateway is still up.
				}

				_gatewayServiceReady <- struct{}{}

				return nil
			},
		})

	// static web
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			listener, err := net.Listen("tcp", net.JoinHostPort(localhost, "0"))
			if err != nil {
				return err
			}

			staticServer := &http.Server{
				Handler:           staticRoute.GetRoute(),
				ReadHeaderTimeout: 5 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      60 * time.Second,
				IdleTimeout:       60 * time.Second,
				MaxHeaderBytes:    64 << 10,
			}

			target := "http://" + listener.Addr().String()

			urlFilePath, err := writeAddressFile(_state.GetRuntimePath(), external.StaticURLFilename, target)
			if err != nil {
				return err
			}

			if err := management.CreateRoute(&model.Route{
				Path:   "/",
				Target: target,
			}); err != nil {
				return err
			}

			logger.Info(
				"Static web service is listening...",
				zap.Any("address", listener.Addr().String()),
				zap.Any("filepath", urlFilePath),
			)
			go func() {
				if err := staticServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("static server error", zap.Any("error", err), zap.Any("address", listener.Addr().String()))
				}
			}()
			return nil
		},
	})
}

// reloadTLSGateway brings up (or reconfigures, or tears down) the HTTPS
// listener based on the current TLS settings in state. Called from two
// places:
//   - Once during startup, after the plain-HTTP gateway is running, so
//     the operator keeps HTTP reachable even if TLS config is incomplete.
//   - On every SetTLS change from the management API, so the listener
//     picks up new certs without a service restart.
//
// Listen address is ":443" by convention. When TLS is disabled the
// current TLS server (if any) is shut down and nothing replaces it.
func reloadTLSGateway(settings service.TLSSettings, gatewayMux *http.ServeMux) error {
	serverMu.Lock()
	defer serverMu.Unlock()

	if !settings.Enabled {
		if _gatewayTLS != nil {
			logger.Info("TLS disabled — stopping HTTPS listener")
			old := _gatewayTLS
			_gatewayTLS = nil
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = old.Shutdown(shutdownCtx)
			shutdownCancel()
		}
		return nil
	}

	// Lazily construct the manager on first enable so the cert directory
	// and self-signed material aren't created until the operator asks.
	if _tlsManager == nil {
		certDir := filepath.Join(_state.GetRuntimePath(), "tls")
		mgr, err := tlsmanager.New(tlsmanager.Config{
			Enabled: true,
			Domains: settings.Domains,
			Email:   settings.Email,
			CertDir: certDir,
		})
		if err != nil {
			return fmt.Errorf("tls manager init: %w", err)
		}
		_tlsManager = mgr
	} else {
		if err := _tlsManager.Update(tlsmanager.Config{
			Enabled: true,
			Domains: settings.Domains,
			Email:   settings.Email,
			CertDir: _tlsManager.Config().CertDir,
		}); err != nil {
			return err
		}
	}

	// BUG 8 fix: shut down old server before opening new listener so the
	// port is released and the new Listen call can succeed.
	if _gatewayTLS != nil {
		logger.Info("Stopping previous TLS gateway before rebind")
		old := _gatewayTLS
		_gatewayTLS = nil
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = old.Shutdown(shutdownCtx)
		shutdownCancel()
		// Brief grace period for the OS to release the port.
		time.Sleep(100 * time.Millisecond)
	}

	// 443 is the expected HTTPS port; privilege to bind is granted by the
	// systemd unit (CAP_NET_BIND_SERVICE) in the CasaOS packaging.
	listener, err := net.Listen("tcp", ":443")
	if err != nil {
		return fmt.Errorf("listen 443: %w", err)
	}

	newServer := &http.Server{
		Handler:           route.WithIdleDeadlines(gatewayMux, route.IdleTransferTimeout),
		ReadHeaderTimeout: 5 * time.Second,
		// NO ABSOLUTE WriteTimeout/ReadTimeout on a listener that carries file
		// transfers. Both were 120s, which is not "the peer went away" — it is
		// "this response has taken too long", and it killed downloads that were
		// streaming perfectly. The client had already received 200 and a
		// Content-Length, so the failure arrived as a TRUNCATED FILE rather than
		// an error. route.WithIdleDeadlines replaces them with a deadline that
		// measures inactivity, so a stalled peer is still cut and a progressing
		// transfer is not. See gatewayMux/idle_deadline.go for the measurement.
		ReadTimeout:    0,
		WriteTimeout:   0,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 64 << 10,
		TLSConfig:      _tlsManager.TLSConfig(),
	}

	go func() {
		if err := newServer.ServeTLS(listener, "", ""); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			logger.Error("TLS gateway serve error", zap.Error(err))
		}
	}()

	logger.Info("TLS gateway listening", zap.String("address", listener.Addr().String()),
		zap.Strings("domains", settings.Domains))

	_gatewayTLS = newServer
	return nil
}

func reloadGateway(port string, gatewayMux *http.ServeMux) error {
	listener, err := net.Listen("tcp", net.JoinHostPort("", port))
	if err != nil {
		logger.Error("failed to bind gateway port", zap.String("port", port), zap.Error(err))
		return err
	}

	addr := listener.Addr().String()

	serverMu.Lock()
	if _gateway != nil && _gateway.Addr == addr {
		serverMu.Unlock()
		listener.Close() // BUG 4 fix: close leaked listener on early return
		logger.Info("Port is the same as current running gateway - no change is required")
		return nil
	}
	serverMu.Unlock()

	// start new gateway
	gatewayNew := &http.Server{
		Addr:              addr,
		Handler:           route.WithIdleDeadlines(gatewayMux, route.IdleTransferTimeout),
		ReadHeaderTimeout: 5 * time.Second,
		// NO ABSOLUTE WriteTimeout/ReadTimeout on a listener that carries file
		// transfers. Both were 120s, which is not "the peer went away" — it is
		// "this response has taken too long", and it killed downloads that were
		// streaming perfectly. The client had already received 200 and a
		// Content-Length, so the failure arrived as a TRUNCATED FILE rather than
		// an error. route.WithIdleDeadlines replaces them with a deadline that
		// measures inactivity, so a stalled peer is still cut and a progressing
		// transfer is not. See gatewayMux/idle_deadline.go for the measurement.
		ReadTimeout:    0,
		WriteTimeout:   0,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}

	go func() {
		err := gatewayNew.Serve(listener)
		if err != nil {
			if errors.Is(err, http.ErrServerClosed) {
				logger.Info("A gateway is stopped", zap.Any("address", gatewayNew.Addr))
				return
			}
			logger.Error("Error when serving a gateway", zap.Any("error", err), zap.Any("address", gatewayNew.Addr))
		}
	}()

	// test if gateway is running
	url := "http://" + addr + "/ping"
	if err := checkURLWithRetry(url, 10); err != nil {
		// Wave 27: shut the new server down on health-check failure.
		// Without this the goroutine at line 495 keeps Serve() running
		// on the new listener forever — _gateway still points at the
		// old server, so nothing will ever Shutdown() the new one. The
		// next reloadGateway call would then fail to bind the same
		// port (or worse, two gateways race for traffic).
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if sErr := gatewayNew.Shutdown(shutdownCtx); sErr != nil {
			logger.Error("failed to shut down unhealthy new gateway",
				zap.String("address", addr),
				zap.Error(sErr))
		}
		return err
	}

	logger.Info("New gateway is listening...", zap.Any("address", gatewayNew.Addr))

	// stop old gateway
	serverMu.Lock()
	if _gateway != nil {
		gatewayOld := _gateway
		go func() {
			logger.Info("Stopping previous gateway in 1 seconds...", zap.Any("address", gatewayOld.Addr))
			time.Sleep(time.Second) // so that any request to the old gateway gets a response
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer shutdownCancel()
			if err := gatewayOld.Shutdown(shutdownCtx); err != nil {
				logger.Error("Error when stopping previous gateway", zap.Any("error", err), zap.Any("address", gatewayOld.Addr))
			}
		}()
	}

	_gateway = gatewayNew
	serverMu.Unlock()

	return nil
}

func checkURLWithRetry(url string, retry int) error {
	count := retry
	var err error

	for count >= 0 {
		logger.Info("Checking if service at URL is running...", zap.Any("url", url), zap.Any("retry", count))
		if err = checkURL(url); err != nil {
			time.Sleep(time.Second)
			count--
			continue
		}
		break
	}

	return err
}

func checkURL(url string) error {
	response, err := http2.Get(url, 5*time.Second)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return ErrCheckURLNotOK
	}

	return nil
}

func writePidFile(runtimePath string) (string, error) {
	filename := "gateway.pid"
	filepath := filepath.Join(runtimePath, filename)
	return filename, os.WriteFile(filepath, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600)
}

func writeAddressFile(runtimePath string, filename string, address string) (string, error) {
	err := os.MkdirAll(runtimePath, 0o755)
	if err != nil {
		return "", err
	}

	// Wave 35-X: use WriteFileAtomic which fsync's both the file and
	// the parent dir. Previous tmp+rename had no fsync — power loss
	// between write and rename could leave a zero-byte management.url
	// / static.url, breaking discovery for every other service that
	// reads these files on next boot.
	finalPath := filepath.Join(runtimePath, filename)
	return finalPath, commonfile.WriteFileAtomic(finalPath, []byte(address), 0o600)
}

func cleanupFiles(runtimePath string, filenames ...string) {
	for _, filename := range filenames {
		err := os.Remove(filepath.Join(runtimePath, filename))
		if err != nil {
			logger.Error("Failed to cleanup file", zap.Any("error", err), zap.Any("filename", filename))
		}
	}
}

func checkPrequisites(state *service.State) error {
	path := state.GetRuntimePath()

	err := os.MkdirAll(path, 0o755)
	if err != nil {
		return fmt.Errorf("please ensure the owner of this service has write permission to that path %s", path)
	}

	return nil
}
