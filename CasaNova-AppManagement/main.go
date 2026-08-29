//go:generate bash -c "mkdir -p codegen && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,server,spec -package codegen api/app_management/openapi.yaml > codegen/app_management_api.go"
//go:generate bash -c "mkdir -p codegen/message_bus && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,client -package message_bus https://raw.githubusercontent.com/IceWhaleTech/CasaOS-MessageBus/main/api/message_bus/openapi.yaml > codegen/message_bus/api.go"

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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/route"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/gatewayroutes"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/coreos/go-systemd/daemon"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	util_http "github.com/LeewayOpenSoftware/CasaNova-Common/utils/http"
)

var (
	commit = "private build"
	date   = "private build"

	//go:embed api/index.html
	_docHTML string

	//go:embed api/index_v1.html
	_docHTMLV1 string

	//go:embed api/app_management/openapi.yaml
	_docYAML string

	//go:embed api/app_management/openapi_v1.yaml
	_docYAMLV1 string

	//go:embed build/sysroot/etc/casaos/app-management.conf.sample
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// parse arguments and intialize
	{
		configFlag := flag.String("c", "", "config file path")
		versionFlag := flag.Bool("v", false, "version")
		removeRuntimeIfNoNvidiaGPUFlag := flag.Bool("removeRuntimeIfNoNvidiaGPU", false, "remove runtime with nvidia gpu")

		flag.Parse()

		if *versionFlag {
			fmt.Printf("v%s\n", common.AppManagementVersion)
			os.Exit(0)
		}

		println("git commit:", commit)
		println("build date:", date)

		config.InitSetup(*configFlag, _confSample)
		config.InitGlobal(*configFlag)

		logger.LogInit(config.AppInfo.LogPath, config.AppInfo.LogSaveName, config.AppInfo.LogFileExt)

		service.MyService = service.NewService(config.CommonInfo.RuntimePath)

		config.RemoveRuntimeIfNoNvidiaGPUFlag = *removeRuntimeIfNoNvidiaGPUFlag

		// Wave 2c: start background services that we built but weren't wired in.
		// StartStatusSync reconciles Docker state with our cache every 30s,
		// catching containers that crashed or were stopped externally.
		// Wave 28: was passing context.Background — meant the goroutine
		// kept running past graceful shutdown until the process was killed.
		// Use the cancellable main ctx so cancel() in main's defer drains
		// these workers as well.
		service.StartStatusSync(ctx)

		// Wave 2e: container auto-update (opt-in via casanova-auto-update label).
		// Checks daily for new images, recreates containers if updates found.
		service.StartAutoUpdate(ctx)

		// One-shot orphan sweep: heal any app whose container detached from its
		// compose project (offline / "not found") — e.g. after an interrupted
		// update or a failed recreate. Delayed so docker + status-sync settle.
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(20 * time.Second):
				service.RepairOrphanedApps(ctx)
			}
		}()
	}

	// setup cron
	{
		crontab := cron.New(cron.WithSeconds())

		// schedule async v2job to get v2 appstore list
		// Wave 28: previously a bare `go func()` with no shutdown coupling.
		// UpdateCatalog can be a long HTTP-bound call (mirrors, retries),
		// so during a fast shutdown the process would hang in defers
		// waiting for it, or — worse — exit before logs were flushed.
		// Wrap with ctx.Done() so a cancel() in main's defer aborts the
		// in-flight call and the goroutine returns promptly.
		go func() {
			// Wave 35-X: recover() on the inner goroutine — a panic
			// inside UpdateCatalog (malformed catalog JSON, registry
			// disk error nil-deref) used to silently kill the startup
			// catalog refresh with no error in the logs.
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() {
					if r := recover(); r != nil {
						logger.Error("startup catalog update panicked",
							zap.Any("panic", r))
					}
				}()
				if err := service.MyService.AppStoreManagement().UpdateCatalog(); err != nil {
					logger.Error("error when updating AppStore catalog at startup", zap.Error(err))
				}
			}()
			select {
			case <-done:
			case <-ctx.Done():
				logger.Info("aborting startup AppStore catalog update — shutdown requested")
			}
		}()

		// Wave 35-X: overlap protection. UpdateCatalog runs the
		// rename-dance (workdir → backup → ready → workdir) for every
		// remote store. If a previous 10m tick is still running when
		// the next fires (slow registries, large catalogs), two
		// concurrent runs race on the same filesystem state and the
		// orphan-recovery logic from Wave 35-X can be tricked into
		// restoring a half-written backup. Guard with atomic flag.
		var catalogCronInFlight atomic.Bool
		if _, err := crontab.AddFunc("@every 10m", func() {
			if !catalogCronInFlight.CompareAndSwap(false, true) {
				logger.Info("previous catalog update still running — skipping this tick")
				return
			}
			defer catalogCronInFlight.Store(false)
			// Wave 35-X: recover() so a single bad catalog run does
			// not kill the cron entry for the daemon lifetime.
			defer func() {
				if r := recover(); r != nil {
					logger.Error("scheduled catalog update panicked",
						zap.Any("panic", r))
				}
			}()
			if err := service.MyService.AppStoreManagement().UpdateCatalog(); err != nil {
				logger.Error("error when updating AppStore catalog", zap.Error(err))
			}
		}); err != nil {
			logger.Error("failed to register catalog update cron", zap.Error(err))
		}

		crontab.Start()
		defer crontab.Stop()

		// Wave 4a: start the Restic-backed backup scheduler. Loads every
		// persisted schedule from disk and registers a cron entry for
		// each enabled one. Non-fatal if it can't start — the rest of
		// AppManagement keeps running without scheduled backups.
		if sched := service.BackupScheduler(); sched != nil {
			sched.Start()
			defer sched.Stop()
		}

	}

	// register at message bus — with retry: a single attempt lost the
	// registration forever whenever this service booted before the bus
	// (same boot-order landmine the other services already guard against).
	go func() {
		for i := 0; i < 10; i++ {
			regCtx, regCancel := context.WithTimeout(ctx, 5*time.Second)
			response, err := service.MyService.MessageBus().RegisterEventTypesWithResponse(regCtx, common.EventTypes)
			regCancel()
			if err == nil && response != nil && response.StatusCode() == http.StatusOK {
				return
			}
			if err != nil {
				logger.Error("register event types failed; retrying", zap.Error(err), zap.Int("attempt", i+1))
			} else if response != nil {
				logger.Error("register event types failed; retrying", zap.String("status", response.Status()), zap.Int("attempt", i+1))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
		logger.Error("register event types gave up after 10 attempts - event types will not be discoverable")
	}()

	// setup listener
	listener, err := net.Listen("tcp", net.JoinHostPort(common.Localhost, "0"))
	if err != nil {
		panic(err)
	}

	urlFilePath := filepath.Join(config.CommonInfo.RuntimePath, "app-management.url")
	if err := file.CreateFileAndWriteContent(urlFilePath, "http://"+listener.Addr().String()); err != nil {
		logger.Error("error when creating address file", zap.Error(err),
			zap.Any("address", listener.Addr().String()),
			zap.Any("filepath", urlFilePath),
		)
	}

	// initialize routers and register at gateway
	{
		// Gateway prefixes come from the shared ownership manifest. The
		// "/v1/container" claim in there is a generic prefix: because the
		// gateway routes on literal prefixes only, owning it means this
		// service — and no other — can serve "/v1/container/:id/..." routes.
		// A ":id" route registered in another binary receives nothing, which
		// is how container log streaming stayed dead behind a 404 while a
		// hardened handler for it sat unused in the main service.
		//
		// The doc/v2 base paths stay local: they are derived from this
		// service's own OpenAPI document.
		apiPaths := append(gatewayroutes.Prefixes(gatewayroutes.OwnerAppManagement),
			route.V1DocPath,
			route.V2APIPath,
			route.V2DocPath,
		)

		target := "http://" + listener.Addr().String()
		if err := registerGatewayRoutes(ctx, apiPaths, target); err != nil {
			panic(err)
		}
		// v0.1: re-register routes every 30s so a gateway crash + tmpfs
		// loss self-heals without needing to restart this service.
		external.StartRouteRegistrationWatchdog(
			ctx, apiPaths, target, service.MyService.Gateway().CreateRoute,
		)
	}

	v1Router := route.InitV1Router()
	v2Router := route.InitV2Router()
	v1DocRouter := route.InitV1DocRouter(_docHTMLV1, _docYAMLV1)
	v2DocRouter := route.InitV2DocRouter(_docHTML, _docYAML)

	mux := &util_http.HandlerMultiplexer{
		HandlerMap: map[string]http.Handler{
			"v1":    v1Router,
			"v2":    v2Router,
			"v1doc": v1DocRouter,
			"doc":   v2DocRouter,
		},
	}

	// notify systemd that we are ready
	{
		if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
			logger.Error("Failed to notify systemd that casaos main service is ready", zap.Any("error", err))
		} else if supported {
			logger.Info("Notified systemd that casaos main service is ready")
		} else {
			logger.Info("This process is not running as a systemd service.")
		}

		logger.Info("App management service is listening...", zap.Any("address", listener.Addr().String()))
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
		logger.Info("shutting down App Management server...")
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := s.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown error", zap.Error(err))
		}
	}()

	err = s.Serve(listener) // not using http.serve() to fix G114: Use of net/http serve function that has no support for setting timeouts (see https://github.com/securego/gosec)
	if err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}

func registerGatewayRoutes(ctx context.Context, paths []string, target string) error {
	for _, apiPath := range paths {
		route := &model.Route{Path: apiPath, Target: target}
		var lastErr error
		for attempt := 1; attempt <= 30; attempt++ {
			if err := service.MyService.Gateway().CreateRoute(route); err != nil {
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
