//go:generate bash -c "mkdir -p codegen && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,server,spec -package codegen api/local_storage/openapi.yaml > codegen/local_storage_api.go"
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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	util_http "github.com/LeewayOpenSoftware/CasaNova-Common/utils/http"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/codegen/message_bus"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/common"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/cache"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/sqlite"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/utils/merge"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/route"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service"
	"github.com/coreos/go-systemd/daemon"
	"github.com/robfig/cron/v3"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

const localhost = "127.0.0.1"

var (
	commit = "private build"
	date   = "private build"

	//go:embed api/index.html
	_docHTML string

	//go:embed api/local_storage/openapi.yaml
	_docYAML string

	//go:embed build/sysroot/etc/casaos/local-storage.conf.sample
	_confSample string
)

func init() {

	configFlag := flag.String("c", "", "config address")
	dbFlag := flag.String("db", "", "db path")

	versionFlag := flag.Bool("v", false, "version")
	initFlag := flag.Bool("init", false, "init local-storage config")

	flag.Parse()

	if *versionFlag {
		fmt.Printf("v%s\n", common.Version)
		os.Exit(0)
	}

	println("git commit:", commit)
	println("build date:", date)

	config.InitSetup(*configFlag, _confSample)

	logger.LogInit(config.AppInfo.LogPath, config.AppInfo.LogSaveName, config.AppInfo.LogFileExt)

	if len(*dbFlag) == 0 {
		*dbFlag = config.AppInfo.DBPath
	}

	sqliteDB := sqlite.GetGlobalDB(*dbFlag)

	service.MyService = service.NewService(sqliteDB)
	service.Cache = cache.Init()
	if initFlag != nil && *initFlag {
		// This branch is the entire body of the casanova-local-storage-first
		// oneshot, which has no start timeout and which docker is ordered
		// behind, so whatever happens here gates the storage stack. Individual
		// probes inside the sweep are bounded, but keep a hard cap on the sweep
		// as a whole: a box that comes up with some disks remounted and a loud
		// log line is recoverable, a boot that never finishes is not.
		finished, sweepErr := service.RunDiskSweepWithDeadline(
			service.MyService.Disk().CheckSerialDiskMount,
			service.DefaultSerialDiskMountDeadline,
		)
		switch {
		case !finished:
			logger.Error("serial disk remount did not finish in time - continuing boot with whatever mounted so far",
				zap.Duration("deadline", service.DefaultSerialDiskMountDeadline))
		case sweepErr != nil:
			logger.Error("serial disk remount failed - some or all data volumes are NOT mounted",
				zap.Error(sweepErr))
		}

		// Assemble the mergerfs pool HERE, in the oneshot, rather than in the
		// daemon. docker.service is ordered behind this unit, so this is the
		// only place where the pool is guaranteed to exist before the first
		// container starts. Assembling it from the daemon instead left a window
		// in which containers bind-mounted /DATA while it was still a bare
		// directory on the rootfs: they kept writing to the shadowed rootfs path
		// even after mergerfs mounted over it, filling the system disk with data
		// the user believes is on the pool.
		//
		// The wait inside CheckMergeMount is what this unit's TimeoutStartSec
		// bounds: if the disks never show up, systemd kills the unit, docker
		// starts anyway (ordering-only) and the pool stays fenced rather than
		// assembled wrong. Bounded degradation beats an unbootable box.
		mergeErr := runInitMergeAssembly()

		// Exiting non-zero is the only signal a oneshot has. It is safe here:
		// casanova-local-storage-first only declares Before=docker.service (pure
		// ordering, no Requires), so a failed run does not keep docker or the
		// target from starting - it just shows up in `systemctl --failed` and in
		// the journal instead of pretending the storage stack came up clean.
		// EX_TEMPFAIL(75) rather than 1, to say "transient, retry after fixing
		// the disk" rather than "misconfigured".
		if !finished || sweepErr != nil || mergeErr != nil {
			os.Exit(75)
		}

		os.Exit(0)
		return
	}
	if strings.ToLower(config.ServerInfo.EnableMergerFS) == "true" {
		if !merge.IsMergerFSInstalled() {
			config.ServerInfo.EnableMergerFS = "false"
			logger.Info("mergerfs is disabled")
		}
	}

	if strings.ToLower(config.ServerInfo.EnableMergerFS) == "true" {
		if !service.MyService.Disk().EnsureDefaultMergePoint() {
			config.ServerInfo.EnableMergerFS = "false"
			logger.Info("mergerfs is disabled")
		}
	}

	if strings.ToLower(config.ServerInfo.EnableMergerFS) == "true" {
		// Fallback only. The authoritative assembly now runs in the -init
		// oneshot, which docker is ordered behind; this call exists for boxes
		// where that unit is not enabled, so their pool still comes up. It is
		// safe to run twice: CheckMergeMount reports a pool the oneshot already
		// built as "merge already exists" and merely refreshes it. It stays in
		// the background so a slow disk cannot delay the API, and it fences
		// itself the same way rather than assembling a pool with disks missing.
		go func() {
			if err := service.MyService.LocalStorage().CheckMergeMount(); err != nil {
				logger.Error("background merge assembly did not complete - the pool may be fenced or incomplete",
					zap.Error(err))
			}
		}()
	}

	checkToken2_11()
	go ensureDefaultDirectories()
	//service.MyService.Disk().EnsureDefaultMergePoint()

	// service.MountLists = make(map[string]*mountlib.MountPoint)
	// configfile.Install()
	// service.MyService.Storage().CheckAndMountAll()

}

func checkToken2_11() {
	deviceTree, err := service.MyService.USB().GetDeviceTree()
	if err != nil {
		panic(err)
	}

	if service.MyService.USB().GetSysInfo().KernelArch == "aarch64" && strings.ToLower(config.ServerInfo.USBAutoMount) != "true" && strings.Contains(deviceTree, "Raspberry Pi") {
		service.MyService.USB().UpdateUSBAutoMount("False")
		service.MyService.USB().ExecUSBAutoMountShell("False")
	}
}

func ensureDefaultDirectories() {
	sysType := runtime.GOOS
	var dirArray []string
	if sysType == "linux" {
		dirArray = []string{"/DATA/AppData", "/DATA/Documents", "/DATA/Downloads", "/DATA/Gallery", "/DATA/Media/Movies", "/DATA/Media/TV Shows", "/DATA/Media/Music"}
	}

	if sysType == "windows" {
		dirArray = []string{"C:\\CasaOS\\DATA\\AppData", "C:\\CasaOS\\DATA\\Documents", "C:\\CasaOS\\DATA\\Downloads", "C:\\CasaOS\\DATA\\Gallery", "C:\\CasaOS\\DATA\\Media/Movies", "C:\\CasaOS\\DATA\\Media\\TV Shows", "C:\\CasaOS\\DATA\\Media\\Music"}
	}

	if sysType == "darwin" {
		dirArray = []string{"./CasaOS/DATA/AppData", "./CasaOS/DATA/Documents", "./CasaOS/DATA/Downloads", "./CasaOS/DATA/Gallery", "./CasaOS/DATA/Media/Movies", "./CasaOS/DATA/Media/TV Shows", "./CasaOS/DATA/Media/Music"}
	}

	for _, v := range dirArray {
		if err := file.IsNotExistMkDir(v); err != nil {
			logger.Error("ensureDefaultDirectories", zap.Error(err))
		}
	}
}

// runInitMergeAssembly assembles the mergerfs pool from inside the -init
// oneshot, honouring the same EnableMergerFS gating the daemon applies. It
// returns nil when there is nothing to assemble, so a box without mergerfs
// never fails its storage oneshot over a feature it does not use.
func runInitMergeAssembly() error {
	if strings.ToLower(config.ServerInfo.EnableMergerFS) != "true" {
		return nil
	}
	if !merge.IsMergerFSInstalled() {
		logger.Info("mergerfs is enabled in config but not installed - skipping pool assembly")
		return nil
	}
	if !service.MyService.Disk().EnsureDefaultMergePoint() {
		logger.Error("default merge point could not be ensured - skipping pool assembly")
		return nil
	}
	return service.MyService.LocalStorage().CheckMergeMount()
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

	go monitorUEvent(ctx)

	service.StartSmartMonitor(ctx)

	go sendStorageStats()

	crontab := cron.New(cron.WithSeconds())
	if _, err := crontab.AddFunc("@every 5s", sendStorageStats); err != nil {
		logger.Error("crontab add func error", zap.Error(err))
	}

	crontab.Start()
	defer crontab.Stop()

	listener, err := net.Listen("tcp", net.JoinHostPort(localhost, "0"))
	if err != nil {
		panic(err)
	}

	// register at gateway
	apiPaths := []string{
		"/v1/usb",
		"/v1/disks",
		"/v1/storage",
		// "/v1/cloud",
		// "/v1/recover",
		// "/v1/driver",
		route.V2APIPath,
		route.V2DocPath,
	}
	target := "http://" + listener.Addr().String()
	if err := registerGatewayRoutes(ctx, apiPaths, target, service.MyService.Gateway().CreateRoute); err != nil {
		panic(err)
	}
	// v0.1: route self-heal watchdog.
	external.StartRouteRegistrationWatchdog(
		ctx, apiPaths, target, service.MyService.Gateway().CreateRoute,
	)
	go RegMsg()
	go service.MyService.Disk().InitCheck()
	v1Router := route.InitV1Router()
	v2Router := route.InitV2Router()
	v2DocRouter := route.InitV2DocRouter(_docHTML, _docYAML)

	mux := &util_http.HandlerMultiplexer{
		HandlerMap: map[string]http.Handler{
			"v1":  v1Router,
			"v2":  v2Router,
			"doc": v2DocRouter,
		},
	}

	if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		logger.Error("Failed to notify systemd that local storage service is ready", zap.Error(err))
	} else if supported {
		logger.Info("Notified systemd that local storage service is ready")
	} else {
		logger.Info("This process is not running as a systemd service.")
	}

	logger.Info("LocalStorage service is listening...", zap.Any("address", listener.Addr().String()))

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		logger.Info("shutting down LocalStorage server...")
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown error", zap.Error(err))
		}
	}()

	err = server.Serve(listener)
	if err != nil && err != http.ErrServerClosed {
		panic(err)
	}
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

func RegMsg() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []message_bus.EventType
	events = append(events, message_bus.EventType{Name: common.ServiceName + ":merge_status", SourceID: common.ServiceName, PropertyTypeList: []message_bus.PropertyType{}})
	events = append(events, message_bus.EventType{Name: common.ServiceName + ":storage_status", SourceID: common.ServiceName, PropertyTypeList: []message_bus.PropertyType{}})
	// Wave 35-X: bound each register attempt. context.Background()
	// + 10 sleeps wedged startup forever when the message-bus was
	// hung (the request never returned because no deadline). The
	// service then never reached HTTP serve.
	for i := 0; i < 10; i++ {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 5*time.Second)
		response, err := service.MyService.MessageBus().RegisterEventTypesWithResponse(attemptCtx, events)
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
	// register at message bus
	for devtype, eventTypesByAction := range common.EventTypes {
		response, err := service.MyService.MessageBus().RegisterEventTypesWithResponse(ctx, lo.Values(eventTypesByAction))
		if err != nil {
			logger.Error("error when trying to register one or more event types - some event type will not be discoverable", zap.Error(err), zap.String("devtype", devtype))
		}

		if response != nil && response.StatusCode() != http.StatusOK {
			logger.Error("error when trying to register one or more event types - some event type will not be discoverable", zap.String("status", response.Status()), zap.String("body", string(response.Body)), zap.String("devtype", devtype))
		}
	}

}
