package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	util_http "github.com/LeewayOpenSoftware/CasaNova-Common/utils/http"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/common"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/route"
	v2route "github.com/LeewayOpenSoftware/CasaNova-Driver/route/v2"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/service"
	"github.com/coreos/go-systemd/v22/daemon"
	"go.uber.org/zap"
)

const localhost = "127.0.0.1"

var (
	commit = "private build"
	date   = "private build"

	//go:embed build/sysroot/etc/casanova/driver.conf.sample
	_confSample string
)

func init() {
	configFlag := flag.String("c", "", "config file path")
	versionFlag := flag.Bool("v", false, "version")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("v%s\n", common.FullVersion())
		os.Exit(0)
	}

	println("git commit:", commit)
	println("build date:", date)

	config.InitSetup(*configFlag, _confSample)
	logger.LogInit(config.AppInfo.LogPath, config.AppInfo.LogSaveName, config.AppInfo.LogFileExt)

	service.MyService = service.NewService()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listener, err := net.Listen("tcp", net.JoinHostPort(localhost, "0"))
	if err != nil {
		panic(err)
	}

	// Internal unix-socket listener: lets sibling root services (the main
	// service's power orchestrator) apply tuning levers server-side without a
	// user JWT. Trust comes from the LISTENER, not the peer address — the
	// socket server's ConnContext stamps every accepted connection, and the
	// rbac gate honours only that stamp (never a loopback RemoteAddr, which
	// the gateway makes meaningless). The 0700 parent dir gates access to
	// root BEFORE the socket even exists (no bind→chmod TOCTOU window).
	// /run/casanova is this unit's RuntimeDirectory (NOT /tmp: PrivateTmp).
	if err := os.MkdirAll(v2route.InternalSocketDir, 0o700); err != nil {
		panic(fmt.Errorf("mkdir %s: %w", v2route.InternalSocketDir, err))
	}
	// MkdirAll keeps an existing dir's mode — force it in case a previous
	// build/tool created it looser.
	if err := os.Chmod(v2route.InternalSocketDir, 0o700); err != nil {
		panic(fmt.Errorf("chmod %s: %w", v2route.InternalSocketDir, err))
	}
	os.Remove(v2route.InternalSocketPath)
	socketListener, err := net.Listen("unix", v2route.InternalSocketPath)
	if err != nil {
		panic(err)
	}
	if err := os.Chmod(v2route.InternalSocketPath, 0o600); err != nil {
		_ = socketListener.Close()
		panic(fmt.Errorf("chmod %s: %w", v2route.InternalSocketPath, err))
	}

	// Register our route prefixes with the gateway (management.url -> :85).
	//
	// "/v1/driver" is deliberately NOT here. The main CasaNova service owns
	// that prefix — it is the original CasaOS v1 endpoint that lists cloud
	// storage providers, and it has clients. This service registering the same
	// prefix did not "also serve" it: the gateway stores one target per prefix,
	// and both services re-register every 30s, so the route alternated between
	// the two backends on a 30s cycle and half of every caller's requests hit
	// the wrong service. Hardware driver endpoints are reachable under
	// /v2/driver, which is what the UI has always used.
	apiPaths := []string{
		route.V2APIPath,
		route.V2DocPath,
	}
	target := "http://" + listener.Addr().String()
	if err := registerGatewayRoutes(ctx, apiPaths, target, service.MyService.Gateway().CreateRoute); err != nil {
		panic(err)
	}
	// Route self-heal watchdog: re-registers if the gateway restarts.
	external.StartRouteRegistrationWatchdog(ctx, apiPaths, target, service.MyService.Gateway().CreateRoute)

	// Re-apply persisted host tuning (governor, swappiness, IO scheduler,
	// spindown, sysctl, zram) — sysfs resets to kernel defaults every boot.
	// Runs in a goroutine so a wedged tool (e.g. `hdparm` on a flaky disk,
	// whose kernel retries can block far past the service start timeout) can
	// NEVER delay SdNotify(ready) and crash-loop the driver. Each command is
	// already bounded by runCmd; recover() is a last-resort backstop so a
	// panic in the reapply chain can't take the process down.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in host tuning reapply", zap.Any("panic", r))
			}
		}()
		if notes := service.MyService.Tuning().Reapply(); len(notes) > 0 {
			logger.Info("host tuning reapply had warnings", zap.Strings("notes", notes))
		}
	}()

	// NOTE: message-bus event registration (hotplug/driver-state events) is a
	// ROADMAP item — it needs the generated message-bus client (oapi-codegen
	// against the private message-bus OpenAPI). See README.

	mux := &util_http.HandlerMultiplexer{
		HandlerMap: map[string]http.Handler{
			"v2":  route.InitV2Router(),
			"doc": route.InitDocRouter(),
		},
	}

	if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		logger.Error("failed to notify systemd of readiness", zap.Error(err))
	} else if supported {
		logger.Info("notified systemd that driver service is ready")
	} else {
		logger.Info("not running as a systemd service")
	}

	logger.Info("driver service is listening...", zap.String("address", listener.Addr().String()))

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Separate http.Server for the unix socket: ConnContext must exist ONLY
	// here — sharing one server across both listeners would stamp TCP
	// connections as internal too and erase the trust boundary.
	socketServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext:       v2route.InternalSocketConnContext,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		logger.Info("shutting down driver server...")
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown error", zap.Error(err))
		}
		if err := socketServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("socket server shutdown error", zap.Error(err))
		}
	}()

	httpErrChan := make(chan error, 1)
	socketErrChan := make(chan error, 1)
	go func() { httpErrChan <- server.Serve(listener) }()
	go func() { socketErrChan <- socketServer.Serve(socketListener) }()

	select {
	case err := <-httpErrChan:
		if err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	case err := <-socketErrChan:
		if err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}
}

// registerGatewayRoutes registers each API prefix, retrying while the gateway
// comes up (mirrors CasaNova-LocalStorage).
func registerGatewayRoutes(ctx context.Context, paths []string, target string, createRoute func(*model.Route) error) error {
	for _, apiPath := range paths {
		r := &model.Route{Path: apiPath, Target: target}
		var lastErr error
		for attempt := 1; attempt <= 30; attempt++ {
			if err := createRoute(r); err != nil {
				lastErr = err
				logger.Error("gateway route registration failed; retrying",
					zap.Error(err), zap.String("path", apiPath), zap.Int("attempt", attempt))
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Second):
				}
				continue
			}
			lastErr = nil
			break
		}
		if lastErr != nil {
			return fmt.Errorf("register gateway route %s: %w", apiPath, lastErr)
		}
	}
	return nil
}
