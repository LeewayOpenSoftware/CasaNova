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
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/common"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/route"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/service"
	"go.uber.org/zap"
)

const localhost = "127.0.0.1"

var (
	commit = "private build"
	date   = "private build"

	//go:embed build/sysroot/etc/casaos/virt-management.conf.sample
	_confSample string
)

func init() {
	configFlag := flag.String("c", "", "config address")
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

	// Register the virtualization API prefix with the gateway.
	apiPaths := []string{route.V2APIPath}
	target := "http://" + listener.Addr().String()
	if err := registerGatewayRoutes(ctx, apiPaths, target, service.MyService.Gateway().CreateRoute); err != nil {
		panic(err)
	}
	// Self-heal: re-register periodically so a gateway restart auto-recovers.
	external.StartRouteRegistrationWatchdog(ctx, apiPaths, target, service.MyService.Gateway().CreateRoute)

	handler := route.InitV2Router()

	if err := sdNotifyReady(); err != nil {
		logger.Error("sd_notify failed", zap.Error(err))
	}

	logger.Info("Virt management service is listening...", zap.String("address", listener.Addr().String()))

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		logger.Info("shutting down Virt management server...")
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown error", zap.Error(err))
		}
	}()

	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}

// registerGatewayRoutes posts each API prefix to the gateway, retrying until it
// succeeds or ctx is cancelled. Mirrors CasaNova-LocalStorage/main.go.
func registerGatewayRoutes(ctx context.Context, paths []string, target string, createRoute func(*model.Route) error) error {
	for _, apiPath := range paths {
		var lastErr error
		for attempt := 1; attempt <= 30; attempt++ {
			if err := createRoute(&model.Route{Path: apiPath, Target: target}); err != nil {
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

// sdNotifyReady sends "READY=1" to systemd via $NOTIFY_SOCKET. Implemented
// inline (rather than pulling in coreos/go-systemd) so the service satisfies
// its Type=notify unit with zero extra dependencies. A no-op when not run under
// systemd (NOTIFY_SOCKET unset).
func sdNotifyReady() error {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		logger.Info("not running under systemd (NOTIFY_SOCKET unset)")
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte("READY=1"))
	return err
}
