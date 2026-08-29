//go:generate bash -c "mkdir -p codegen && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,server,spec -package codegen api/message_bus/openapi.yaml > codegen/message_bus_api.go"

package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	util_http "github.com/LeewayOpenSoftware/CasaNova-Common/utils/http"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/common"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/config"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/repository"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/route"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/service"
	"github.com/coreos/go-systemd/daemon"
	"go.uber.org/zap"
)

const localhost = "127.0.0.1"

var (
	commit = "private build"
	date   = "private build"

	//go:embed api/index.html
	_docHTML string

	//go:embed api/message_bus/openapi.yaml
	_docYAML string

	//go:embed build/sysroot/etc/casaos/message-bus.conf.sample
	_confSample string
)

// unixSocketPath: shared constant from Common — /run/casanova/internal, NOT
// /tmp. Every unit runs with PrivateTmp=yes, so a /tmp socket only ever
// existed inside this service's private namespace while every consumer dialed
// its OWN empty private /tmp (the "message-bus caído" outage).
const unixSocketPath = external.MessageBusSocketPath

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

	// arguments
	configFlag := flag.String("c", "", "config file path")
	versionFlag := flag.Bool("v", false, "version")

	flag.Parse()

	if *versionFlag {
		fmt.Printf("v%s\n", common.MessageBusVersion)
		os.Exit(0)
	}

	println("git commit:", commit)
	println("build date:", date)

	// initialization
	config.InitSetup(*configFlag, _confSample)

	logger.LogInit(config.AppInfo.LogPath, config.AppInfo.LogSaveName, config.AppInfo.LogFileExt)

	// repository
	if err := file.IsNotExistMkDir(config.CommonInfo.RuntimePath); err != nil {
		panic(err)
	}

	databaseFilePath := filepath.Join(config.CommonInfo.RuntimePath, "message-bus.db")
	persistDatabaseFilePath := filepath.Join(config.AppInfo.DBPath, "db", "message-bus.db")
	repository, err := repository.NewDatabaseRepository(databaseFilePath, persistDatabaseFilePath)
	if err != nil {
		panic(err)
	}
	defer repository.Close()

	// service
	services := service.NewServices(&repository)

	// B11: push dispatcher degradation into systemd's STATUS= line. The
	// unit is Type=notify (see build/sysroot/.../casanova-message-bus.service)
	// so NOTIFY_SOCKET is present and the READY=1 notify below already
	// proves the channel works. This gives the operator a sticky trace
	// in `systemctl status casanova-message-bus` — the process stays
	// active after a dispatcher restart, so without it the only record
	// would be a journald line scrolling away under months of uptime.
	services.SetStatusNotifier(func(status string) {
		if _, err := daemon.SdNotify(false, "STATUS="+status); err != nil {
			logger.Error("failed to report degraded status to systemd", zap.Error(err))
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	services.Start(&ctx)
	go services.YSKService.Start(true)

	// route
	swagger, err := codegen.GetSwagger()
	if err != nil {
		panic(err)
	}

	apiRouter, err := route.NewAPIRouter(swagger, &services)
	if err != nil {
		panic(err)
	}

	docRouter, err := route.NewDocRouter(swagger, _docHTML, _docYAML)
	if err != nil {
		panic(err)
	}

	mux := &util_http.HandlerMultiplexer{
		HandlerMap: map[string]http.Handler{
			"v2":  apiRouter,
			"doc": docRouter,
		},
	}

	// http listener
	listener, err := net.Listen("tcp", net.JoinHostPort(localhost, "0"))
	if err != nil {
		panic(err)
	}

	// Trusted-caller unix socket, inside the 0700 root-only internal dir
	// (same pattern as casanova-driver): the parent dir gates access to root
	// BEFORE the socket exists — no bind→chmod TOCTOU window, and no
	// world-traversable /tmp. The route layer's JWT middleware trusts
	// connections accepted on this listener (ConnContext stamp), so the
	// socket's 0600 + the dir's 0700 ARE the auth boundary for tokenless
	// internal services.
	if err := os.MkdirAll(external.MessageBusSocketDir, 0o700); err != nil {
		panic(fmt.Errorf("mkdir %s: %w", external.MessageBusSocketDir, err))
	}
	// MkdirAll keeps an existing dir's mode — force it (the driver creates
	// the same dir; both enforce identical perms, so order doesn't matter).
	if err := os.Chmod(external.MessageBusSocketDir, 0o700); err != nil {
		panic(fmt.Errorf("chmod %s: %w", external.MessageBusSocketDir, err))
	}
	// remove unix socket file. don't need check whether it exists or not
	os.Remove(unixSocketPath)
	// socket listener
	socketListener, err := net.Listen("unix", unixSocketPath)
	if err != nil {
		panic(err)
	}
	if err := os.Chmod(unixSocketPath, 0o600); err != nil {
		_ = socketListener.Close()
		panic(fmt.Errorf("chmod %s: %w", unixSocketPath, err))
	}

	// register at gateway
	u, err := url.Parse(swagger.Servers[0].URL)
	if err != nil {
		panic(err)
	}

	apiPath := strings.TrimRight(u.Path, "/")
	apiPaths := []string{apiPath, "/doc" + apiPath}

	gatewayManagement, err := external.NewManagementService(config.CommonInfo.RuntimePath)
	if err != nil {
		panic(err)
	}

	target := "http://" + listener.Addr().String()
	if err := registerGatewayRoutes(ctx, apiPaths, target, gatewayManagement.CreateRoute); err != nil {
		panic(err)
	}
	// v0.1: route self-heal watchdog.
	external.StartRouteRegistrationWatchdog(
		ctx, apiPaths, target, gatewayManagement.CreateRoute,
	)

	// write address file
	addressFilePath, err := writeAddressFile(config.CommonInfo.RuntimePath, external.MessageBusAddressFilename, "http://"+listener.Addr().String())
	if err != nil {
		panic(err)
	}

	// notify systemd
	if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		logger.Error("Failed to notify systemd that message bus service is ready", zap.Error(err))
	} else if supported {
		logger.Info("Notified systemd that message bus service is ready")
	} else {
		logger.Info("This process is not running as a systemd service.")
	}

	// start http server
	logger.Info("MessageBus service is listening...", zap.Any("address", listener.Addr().String()), zap.String("filepath", addressFilePath))

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	socketServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// Wave 32: tag every connection accepted on the unix socket as
		// trusted-local. The JWT middleware reads this context value to
		// decide whether to skip auth, replacing the previous (spoofable)
		// `Host == "unix"` heuristic.
		ConnContext: route.UnixSocketConnContext,
	}

	// Graceful shutdown on SIGINT/SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		logger.Info("shutting down MessageBus servers...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown error", zap.Error(err))
		}
		if err := socketServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("socket server shutdown error", zap.Error(err))
		}
	}()

	httpServerErrChan := make(chan error, 1)
	socketServerErrChan := make(chan error, 1)

	go func() {
		err := server.Serve(listener)
		httpServerErrChan <- err
	}()

	go func() {
		err := socketServer.Serve(socketListener)
		socketServerErrChan <- err
	}()

	select {
	case err := <-httpServerErrChan:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("MessageBus service is stopped", zap.Error(err))
			os.Exit(1)
		}
	case err := <-socketServerErrChan:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("MessageBus socket service is stopped", zap.Error(err))
			os.Exit(1)
		}
	}

	// Cancel context to signal service goroutines (Event, Action, SocketIO)
	// to stop, then wait for them to finish cleanup before exiting.
	cancel()
	services.Wait()
	logger.Info("all service goroutines stopped, exiting")
}

func writeAddressFile(runtimePath string, filename string, address string) (string, error) {
	err := os.MkdirAll(runtimePath, 0o755)
	if err != nil {
		return "", err
	}

	// Wave 35-X: atomic write. Other services tail this file to
	// discover the bus endpoint; a non-atomic os.WriteFile leaves a
	// torn (zero-byte) file visible briefly. Worse, a power loss
	// between the truncate and the rewrite leaves a permanently
	// empty file — consumers parse "" as the endpoint and fail
	// forever until manual restart.
	addrPath := filepath.Join(runtimePath, filename)
	return addrPath, file.WriteFileAtomic(addrPath, []byte(address), 0o600)
}

func registerGatewayRoutes(ctx context.Context, paths []string, target string, createRoute func(*model.Route) error) error {
	for _, apiPath := range paths {
		route := &model.Route{Path: apiPath, Target: target}
		var lastErr error
		for attempt := 1; attempt <= 30; attempt++ {
			if err := createRoute(route); err != nil {
				lastErr = err
				logger.Error("gateway route registration failed; retrying",
					zap.Error(err),
					zap.String("path", apiPath),
					zap.Int("attempt", attempt),
				)
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
