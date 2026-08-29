//go:generate bash -c "mkdir -p codegen && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,server,spec -package codegen api/casaos/openapi.yaml > codegen/casaos_api.go"
//go:generate bash -c "mkdir -p codegen/message_bus && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,client -package message_bus https://raw.githubusercontent.com/IceWhaleTech/CasaOS-MessageBus/main/api/message_bus/openapi.yaml > codegen/message_bus/api.go"
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/tokenrevocation"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/gatewayroutes"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/command"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/constants"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"

	util_http "github.com/LeewayOpenSoftware/CasaNova-Common/utils/http"

	"github.com/LeewayOpenSoftware/CasaNova/common"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/cache"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/sqlite"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova/route"
	v1 "github.com/LeewayOpenSoftware/CasaNova/route/v1"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/coreos/go-systemd/daemon"
	"go.uber.org/zap"

	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

const LOCALHOST = "127.0.0.1"

var sqliteDB *gorm.DB

var (
	commit = "private build"
	date   = "private build"

	//go:embed api/index.html
	_docHTML string

	//go:embed api/casaos/openapi.yaml
	_docYAML string

	//go:embed build/sysroot/etc/casaos/casaos.conf.sample
	_confSample string

	configFlag  = flag.String("c", "", "config address")
	dbFlag      = flag.String("db", "", "db path")
	versionFlag = flag.Bool("v", false, "version")
)

func init() {
	flag.Parse()
	if *versionFlag {
		fmt.Println(common.FullVersion())
		return
	}

	println("git commit:", commit)
	println("build date:", date)

	config.InitSetup(*configFlag, _confSample)

	logger.LogInit(config.AppInfo.LogPath, config.AppInfo.LogSaveName, config.AppInfo.LogFileExt)
	if len(*dbFlag) == 0 {
		*dbFlag = config.AppInfo.DBPath + "/db"
	}

	sqliteDB = sqlite.GetDb(*dbFlag)
	// gredis.GetRedisConn(config.RedisInfo),

	service.MyService = service.NewService(sqliteDB, config.CommonInfo.RuntimePath)

	// Wire audit log + notification backends onto the same SQLite handle.
	// Both packages live in CasaNova-Common and are shared with other services.
	service.InitAuditAndNotifications(sqliteDB)

	service.Cache = cache.Init()

	service.GetCPUThermalZone()

	route.InitFunction()

	//service.MyService.System().GenreateSystemEntry()
	///
	//service.MountLists = make(map[string]*mountlib.MountPoint)
	//configfile.Install()
}

// @title casaOS API
// @version 1.0.0
// @contact.name lauren.pan
// @contact.url https://www.zimaboard.com
// @contact.email dev@lucentopen.org
// @description casaOS v1versionapi
// @host 192.168.2.217:8089
// @securityDefinitions.apikey ApiKeyAuth
// @in header
// @name Authorization
// @BasePath /v1
// storageMaintenanceStartBudget bounds boot-time storage recovery. It must stay
// comfortably under casanova.service's TimeoutStartSec; see the test named in
// the comment at its use site.
const storageMaintenanceStartBudget = 45 * time.Second

func main() {
	// `-v` PRINTS A VERSION AND DOES NOTHING ELSE.
	//
	// init() already printed it and returned, but returning from init() only
	// skips the rest of init() — main() still runs. The version check used to
	// live 90 lines down, AFTER the storage authority was provisioned and
	// after InstallStorageMaintenanceEngine ran capability discovery, formatter
	// recovery and sanitize recovery. So `casanova -v` performed destructive
	// storage maintenance against the state of the LIVE service.
	//
	// That is not hypothetical: casaos-to-casanova.sh calls /usr/bin/casanova -v
	// FOUR times per upgrade, one of them after the services are already
	// running, as root. Every upgrade this box has taken ran a second,
	// concurrent recovery pass against the running instance's own state.
	//
	// A flag that asks a question must not answer it by changing the system.
	if *versionFlag {
		return
	}

	// Establish the shared revocation authority before serving. It also
	// provisions itself on first consultation, so a service that never reaches
	// this line still enforces — but doing it here means a failure is logged
	// at boot instead of surfacing later as unexplained 401s.
	//
	// A failure is deliberately NOT fatal: this service must still come up so
	// its diagnostics endpoint can explain why every token is being refused.
	// It fails closed either way.
	// Establish the storage authority before serving. It answers "may CasaNova,
	// and may this principal, perform this operation here" — the question the
	// inherited deny-list could not ask.
	//
	// The ceiling is DERIVED, never hardcoded: an install whose disk sits at
	// /storage or whose share lives under /home/data works today, and a fixed
	// {/DATA,/mnt,/media} would start refusing it with no diagnosis. Each
	// source below is asked at runtime; a source that ERRORS marks the ceiling
	// degraded (narrower, and said out loud) rather than guessed at.
	//
	// A failure here is deliberately NOT fatal: the service must still come up
	// so its diagnostics can explain why storage is being refused. It fails
	// closed either way.
	if err := storageauth.Ensure(storageauth.Deps{
		DataRoots: utils.DataRoots,
		Mounts: func() ([]string, error) {
			if service.MyService == nil || service.MyService.Storage() == nil {
				return nil, errors.New("storage service not initialised")
			}
			list, err := service.MyService.Storage().GetStorages()
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(list.MountPoints))
			for _, mp := range list.MountPoints {
				if mp.MountPoint != "" {
					out = append(out, mp.MountPoint)
				}
			}
			return out, nil
		},
		AppData: func() (string, error) {
			if config.AppInfo.DBPath == "" {
				return "", errors.New("application data path is not configured")
			}
			return config.AppInfo.DBPath, nil
		},
		NonAdminPrincipals: func() ([]storageauth.Principal, error) {
			// Used only by the one-time migration, to report how many accounts
			// are waiting for an administrator to assign storage. The user
			// service owns the account list; until that call exists here,
			// reporting zero would be a lie, so the migration is told it does
			// not know and refuses to write a store it cannot describe.
			return nil, errors.New("account enumeration is not wired in this service")
		},
	}); err != nil {
		logger.Error("storage authority is not in force; file operations will be refused",
			zap.String("invariant", storageauth.CurrentHealth().Invariant),
			zap.String("cause", storageauth.CurrentHealth().Detail),
			zap.Error(err))
	}

	// Bring up destructive storage maintenance. Until this runs the formatter
	// and the sanitizer exist but nothing can reach them, and the recovery
	// guarantees the API documents are false: an interrupted format stays
	// undiagnosed and sanitization artifacts from a crash are never cleaned up.
	//
	// Not fatal. A service that cannot offer storage maintenance must still
	// start, because its own diagnostics are how an operator learns why; the
	// endpoints answer 503 until it succeeds, which is the fail-closed side.
	// A DEADLINE, not just a context.
	//
	// Startup recovery walks filesystems named by records read off disk, and a
	// root on a remote mount that has gone away blocks in the kernel. Honouring
	// a context that never expires bounds nothing: the bound is this deadline.
	// The deadline must be SHORTER THAN systemd's start timeout, or it is
	// decorative. The first version of this used two minutes; casanova.service
	// carries TimeoutStartUSec=1min 30s, so systemd would have killed the unit
	// thirty seconds before the deadline could fire — the hang this was written
	// to bound would have become a crash-loop instead, which is strictly worse
	// than the hang. A bound that cannot be reached is not a bound.
	//
	// storageMaintenanceStartBudget is asserted against the packaged unit file
	// by TestStartupBudgetFitsInsideTheSystemdStartTimeout, so changing either
	// one without the other fails the build rather than shipping quietly.
	smCtx, smCancel := context.WithTimeout(context.Background(), storageMaintenanceStartBudget)
	defer smCancel()
	if err := v1.InstallStorageMaintenanceEngine(smCtx); err != nil {
		logger.Error("storage maintenance is unavailable; format and sanitize endpoints will refuse",
			zap.Error(err))
	}

	if err := tokenrevocation.Ensure(); err != nil {
		logger.Error("token revocation authority is not in force; every token will be refused",
			zap.String("path", tokenrevocation.Path()),
			zap.String("invariant", tokenrevocation.CurrentHealth().Invariant),
			zap.Error(err))
	}

	if *versionFlag {
		return
	}
	v1Router := route.InitV1Router()

	v2Router := route.InitV2Router()
	v2DocRouter := route.InitV2DocRouter(_docHTML, _docYAML)
	v3File := route.InitFile()
	mux := &util_http.HandlerMultiplexer{
		HandlerMap: map[string]http.Handler{
			"v1":  v1Router,
			"v2":  v2Router,
			"v3":  v3File,
			"doc": v2DocRouter,
		},
	}

	crontab := cron.New(cron.WithSeconds())
	// Status broadcast interval - reduced from 5s to 10s to lower CPU/log overhead on low-power devices
	if _, err := crontab.AddFunc("@every 10s", route.SendAllHardwareStatusBySocket); err != nil {
		logger.Error("add crontab error", zap.Error(err))
	}

	crontab.Start()
	defer crontab.Stop()

	listener, err := net.Listen("tcp", net.JoinHostPort(LOCALHOST, "0"))
	if err != nil {
		panic(err)
	}
	// Gateway prefixes come from the shared ownership manifest, not from a
	// list maintained here. Two services independently editing their own list
	// is what produced a container-logs endpoint that 404'd for months (the
	// handler existed, app-management's blanket "/v1/container" claim just
	// swallowed the traffic) and a "/v1/driver" route that flapped between two
	// backends every 30s. The manifest's tests reject both shapes.
	//
	// The v2/v3 base paths stay local: each is derived from this service's own
	// OpenAPI document at init, so the spec — not a hand-copied literal —
	// remains authoritative for them.
	routers := append(gatewayroutes.Prefixes(gatewayroutes.OwnerCasaNova),
		route.V2APIPath,
		route.V2DocPath,
		route.V3FilePath,
	)
	// Wave 35-X: retry gateway route registration. Previously this
	// panicked on the first failure, taking the entire CasaNova
	// process down whenever the gateway happened to be slow to boot
	// or restarted out from under us. The other six services already
	// implement this retry — CasaNova was the only one still on the
	// "panic on first error" path, which explained the cascade where
	// a gateway hiccup at boot took the whole dashboard with it.
	target := "http://" + listener.Addr().String()
	for _, apiPath := range routers {
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
				time.Sleep(2 * time.Second)
				continue
			}
			lastErr = nil
			break
		}
		if lastErr != nil {
			fmt.Println("err", lastErr)
			panic(lastErr)
		}
	}

	// v0.1: route-registration watchdog. Gateway's routes live in
	// /run/casaos/routes.json (tmpfs — disappears on reboot) and in
	// memory. PartOf=casanova-gateway cascades restarts when gateway
	// dies, but in the rare case where gateway restarts faster than
	// systemd can cascade (or one cascading child gets StartLimitBurst'd
	// into "failed" state), routes silently vanish. The watchdog
	// re-POSTs every 30s — gateway's CreateRoute is idempotent so
	// steady-state is a no-op, and a route-loss self-heals in <60s
	// instead of needing a manual systemctl restart.
	external.StartRouteRegistrationWatchdog(
		context.Background(),
		routers,
		target,
		service.MyService.Gateway().CreateRoute,
	)

	// register at message bus — per-attempt timeout (Wave 35-X). A
	// wedged bus must not freeze CasaNova startup forever.
	for i := 0; i < 10; i++ {
		attemptCtx, attemptCancel := context.WithTimeout(context.Background(), 5*time.Second)
		response, err := service.MyService.MessageBus().RegisterEventTypesWithResponse(attemptCtx, common.EventTypes)
		attemptCancel()
		if err != nil {
			logger.Error("error when trying to register one or more event types - some event type will not be discoverable", zap.Error(err))
		}
		if response != nil && response.StatusCode() != http.StatusOK {
			logger.Error("error when trying to register one or more event types - some event type will not be discoverable", zap.String("status", response.Status()), zap.String("body", string(response.Body)))
		}
		if response != nil && response.StatusCode() == http.StatusOK {
			break
		}
		time.Sleep(time.Second)
	}

	go func() {
		time.Sleep(time.Second * 2)
		// v0.3.6
		if config.ServerInfo.HttpPort != "" {
			changePort := model.ChangePortRequest{}
			changePort.Port = config.ServerInfo.HttpPort
			err := service.MyService.Gateway().ChangePort(&changePort)
			if err == nil {
				config.Cfg.Section("server").Key("HttpPort").SetValue("")
				config.Cfg.SaveTo(config.SystemConfigInfo.ConfigPath)
			}
		}
	}()

	urlFilePath := filepath.Join(config.CommonInfo.RuntimePath, "casaos.url")
	if err := file.CreateFileAndWriteContent(urlFilePath, "http://"+listener.Addr().String()); err != nil {
		logger.Error("error when creating address file", zap.Error(err),
			zap.Any("address", listener.Addr().String()),
			zap.Any("filepath", urlFilePath),
		)
	}

	// run any script that needs to be executed
	scriptDirectory := filepath.Join(constants.DefaultConfigPath, "start.d")
	command.ExecuteScripts(scriptDirectory)

	if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		logger.Error("Failed to notify systemd that casaos main service is ready", zap.Any("error", err))
	} else if supported {
		logger.Info("Notified systemd that casaos main service is ready")
	} else {
		logger.Info("This process is not running as a systemd service.")
	}
	// http.HandleFunc("/v1/file/test", func(w http.ResponseWriter, r *http.Request) {

	// 	//http.ServeFile(w, r, r.URL.Path[1:])
	// 	http.ServeFile(w, r, "/DATA/test.img")
	// })
	// go http.ListenAndServe(":8081", nil)

	// Start background goroutines after all initialization is complete.
	// Start the recycle bin janitor — auto-purges items older than 30 days.
	// Use a cancellable context so the janitor stops on shutdown.
	janitorCtx, janitorCancel := context.WithCancel(context.Background())
	defer janitorCancel()
	go service.StartTrashJanitor(janitorCtx)
	// Sweep orphaned .temp upload directories and .casanova-tmp copy stubs
	// left behind by a previous crash.
	go service.SweepOrphanTempDirs([]string{"/DATA", "/mnt", "/media"})
	// First-boot onboarding YSK cards — "set up storage" and "pick a
	// remote-access connector" hints. The publisher re-evaluates every
	// 5 minutes so plugging in a disk or installing Tailscale makes
	// the relevant card disappear without restart.
	v1.StartOnboardingCardsPublisher(janitorCtx)
	// ZimaOS-client LAN connect gate — binds :9527 and answers the mobile
	// client's hard-coded device probe with the flat device document it
	// accepts (route/zimaos_gate.go). Stops with janitorCtx on shutdown.
	go route.StartZimaClientGate(janitorCtx)
	// UDP :9527 discovery responder — answers the client's LAN broadcast probe
	// so a CasaNova box auto-appears in the Windows/Android device list.
	go route.StartZimaDiscoveryResponder(janitorCtx)
	// UPS-aware power orchestrator — polls the Forza power daemon
	// (GET /api/power) and, when enabled, reacts to its `advice` (ordered
	// shutdown on shutdown_now). Opt-in: observes only until configured.
	service.Power().Start(janitorCtx)

	s := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // fix G112: Potential slowloris attack (see https://github.com/securego/gosec)
	}

	// Graceful shutdown on SIGINT/SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		logger.Info("shutting down server...")
		// Cancel the trash janitor goroutine before shutting down the server.
		janitorCancel()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown error", zap.Error(err))
		}
	}()

	logger.Info("CasaOS main service is listening...", zap.Any("address", listener.Addr().String()))
	// defer service.MyService.Storage().UnmountAllStorage()
	err = s.Serve(listener) // not using http.serve() to fix G114: Use of net/http serve function that has no support for setting timeouts (see https://github.com/securego/gosec)
	if err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}
