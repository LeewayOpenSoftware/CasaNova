//go:generate bash -c "mkdir -p codegen && go run github.com/deepmap/oapi-codegen/v2/cmd/oapi-codegen@v2.1.0 -generate types,server,spec -package codegen api/job_management/openapi.yaml > codegen/job_management_api.go"

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
	"path/filepath"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	util_http "github.com/LeewayOpenSoftware/CasaNova-Common/utils/http"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/config"
	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/route"
	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/service"
	"github.com/coreos/go-systemd/daemon"

	"go.uber.org/zap"
)

var (
	commit = "private build"
	date   = "private build"

	//go:embed api/index.html
	_docHTML string

	//go:embed api/job_management/openapi.yaml
	_docYAML string

	//go:embed build/sysroot/etc/casaos/job-management.conf.sample
	_confSample string
)

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

	// watchdogCancel is hoisted up here so the route-registration
	// watchdog launched inside the inner block can be stopped from the
	// SIGTERM handler at the end of main. Defaults to a no-op so the
	// graceful-shutdown branch is safe even if the watchdog block was
	// never entered (e.g. version-flag exit).
	var watchdogCancel context.CancelFunc = func() {}
	defer func() { watchdogCancel() }()

	// parse arguments and intialize
	{
		configFlag := flag.String("c", "", "config file path")
		versionFlag := flag.Bool("v", false, "version")

		flag.Parse()

		if *versionFlag {
			fmt.Printf("version: %s\n", common.Version)
			fmt.Printf("git commit: %s\n", commit)
			fmt.Printf("build date: %s\n", date)

			os.Exit(0)
		}

		fmt.Printf("git commit: %s\n", commit)
		fmt.Printf("build date: %s\n", date)

		config.InitSetup(*configFlag, _confSample)

		logger.LogInit(config.AppInfo.LogPath, config.AppInfo.LogSaveName, config.AppInfo.LogFileExt)

		service.Initialize(config.CommonInfo.RuntimePath)

		// Wave 4e: start the user-defined scheduled-tasks manager.
		// Loads persisted tasks from disk and begins firing cron entries.
		// Non-fatal if it fails — the rest of JobManagement still serves
		// the generated API surface.
		if sched := service.Scheduler(); sched != nil {
			sched.Start()
			defer sched.Stop()
		}
	}

	// setup listener
	listener, _err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
	if _err != nil { // use _err to avoid shadowing the err variables below.
		panic(_err)
	}
	defer listener.Close()

	// write address to file
	{
		urlFilePath := filepath.Join(config.CommonInfo.RuntimePath, "job-management.url")
		if err := file.CreateFileAndWriteContent(urlFilePath, "http://"+listener.Addr().String()); err != nil {
			logger.Error("error when creating address file", zap.Error(err),
				zap.Any("address", listener.Addr().String()),
				zap.Any("filepath", urlFilePath),
			)
		}
	}

	// initialize routers and register at gateway
	{
		apiPaths := []string{
			route.APIPath,
			route.DocPath,
		}

		target := "http://" + listener.Addr().String()
		if err := registerGatewayRoutes(context.Background(), apiPaths, target, service.MyService.Gateway().CreateRoute); err != nil {
			panic(err)
		}
		// v0.1: route self-heal watchdog. Cancellable so the goroutine
		// exits on SIGTERM instead of leaking past server shutdown.
		var watchdogCtx context.Context
		watchdogCtx, watchdogCancel = context.WithCancel(context.Background())
		external.StartRouteRegistrationWatchdog(
			watchdogCtx, apiPaths, target, service.MyService.Gateway().CreateRoute,
		)
	}

	router := route.GetRouter()
	docRouter := route.GetDocRouter(_docHTML, _docYAML)

	mux := &util_http.HandlerMultiplexer{
		HandlerMap: map[string]http.Handler{
			"v2":  router,
			"doc": docRouter,
		},
	}

	// notify systemd that we are ready
	{
		if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
			logger.Error("Failed to notify systemd that the service is ready", zap.Any("error", err))
		} else if supported {
			logger.Info("Notified systemd that the service is ready")
		} else {
			logger.Info("This process is not running as a systemd service.")
		}

		logger.Info("Module management service is listening...", zap.Any("address", listener.Addr().String()))
	}

	s := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // fix G112: Potential slowloris attack (see https://github.com/securego/gosec)
	}

	// Graceful shutdown on SIGINT/SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		logger.Info("shutting down Job Management server...")
		watchdogCancel() // stop POST-ing routes before draining requests
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown error", zap.Error(err))
		}
	}()

	if err := s.Serve(listener); err != nil && err != http.ErrServerClosed {
		logger.Error("Job management service stopped with error", zap.Error(err))
	}
	logger.Info("Job management service stopped")
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
