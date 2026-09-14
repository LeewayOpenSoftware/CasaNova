//go:generate bash -c "mkdir -p codegen/user_service && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,server,spec -package codegen api/user-service/openapi.yaml > codegen/user_service/user_service_api.go"
//go:generate bash -c "mkdir -p codegen/message_bus && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -package message_bus https://raw.githubusercontent.com/IceWhaleTech/CasaOS-MessageBus/main/api/message_bus/openapi.yaml > codegen/message_bus/api.go"
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
	"strings"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	util_http "github.com/LeewayOpenSoftware/CasaNova-Common/utils/http"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/codegen/message_bus"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/common"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/sessionsecurity"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/sqlite"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/utils/encryption"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/utils/random"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/route"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	"github.com/coreos/go-systemd/daemon"
	"go.uber.org/zap"
)

const localhost = "127.0.0.1"

var (
	commit = "private build"
	date   = "private build"

	//go:embed api/index.html
	_docHTML string

	//go:embed api/user-service/openapi.yaml
	_docYAML string

	//go:embed build/sysroot/etc/casaos/user-service.conf.sample
	_confSample string
)

func init() {
	configFlag := flag.String("c", "", "config address")
	dbFlag := flag.String("db", "", "db path")
	resetUserFlag := flag.Bool("ru", false, "reset user")
	userFlag := flag.String("user", "", "user name")
	versionFlag := flag.Bool("v", false, "version")

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

	sqliteDB := sqlite.GetDb(*dbFlag)
	service.MyService = service.NewService(sqliteDB, config.CommonInfo.RuntimePath)

	if *resetUserFlag {
		if userFlag == nil || len(*userFlag) == 0 {
			fmt.Println("user is empty")
			return
		}

		userData := service.MyService.User().GetUserAllInfoByName(*userFlag)

		if userData.Id == 0 {
			fmt.Println("user not exist")
			return
		}

		password := random.RandomString(12, true) // 12 chars with mixed case for stronger reset passwords
		hashedPwd, err := encryption.HashPassword(password)
		if err != nil {
			fmt.Println("Error hashing password:", err)
			return
		}
		userData.Password = hashedPwd
		if err := sessionsecurity.ResetPassword(userData.Id, func() {
			service.MyService.User().UpdateUserPassword(userData)
		}); err != nil {
			fmt.Println("ERROR: password changed but existing sessions could not be revoked:", err)
			return
		}
		fmt.Println("User password has been reset successfully.")
		fmt.Println("Username: " + userData.Username)

		// Wave 28: never write the plaintext reset password to stdout
		// when stdout is not a real terminal. Under systemd / sudo /
		// piping the password ends up in journald or a parent log,
		// where it stays indefinitely. When stdout is a TTY (admin
		// running the command directly) we still print so the
		// existing UX works; otherwise we drop a 0600 file and print
		// only its path.
		if isTerminal(os.Stdout) {
			fmt.Println("New password: " + password)
		} else {
			pwPath := filepath.Join(config.CommonInfo.RuntimePath, "reset-password.txt")
			if err := os.WriteFile(pwPath, []byte(password+"\n"), 0o600); err != nil {
				fmt.Println("ERROR: failed to write reset password file:", err)
				fmt.Println("Re-run from an interactive terminal to retrieve the new password.")
				return
			}
			fmt.Println("New password written to: " + pwPath)
			fmt.Println("Read it once, then delete the file.")
		}
		fmt.Println("IMPORTANT: Change this password immediately after login.")
	}
}

// isTerminal reports whether f is attached to a character device,
// which is the standard cheap proxy for "is this an interactive
// TTY". Avoids pulling in golang.org/x/term just for this one check.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return (st.Mode() & os.ModeCharDevice) != 0
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

	v1Router := route.InitRouter()
	v2Router := route.InitV2Router()
	v2DocRouter := route.InitV2DocRouter(_docHTML, _docYAML)

	_, publicKey := service.MyService.User().GetKeyPair()

	jswkJSON, err := jwt.GenerateJwksJSON(publicKey)
	if err != nil {
		panic(err)
	}

	mux := &util_http.HandlerMultiplexer{
		HandlerMap: map[string]http.Handler{
			"v1":                                    v1Router,
			"v2":                                    v2Router,
			"doc":                                   v2DocRouter,
			strings.SplitN(jwt.JWKSPath, "/", 2)[0]: jwt.JWKSHandler(jswkJSON),
		},
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(localhost, "0"))
	if err != nil {
		panic(err)
	}

	apiPaths := []string{
		"/v1/users",
		route.V2APIPath,
		route.V2DocPath,
		"/" + jwt.JWKSPath,
	}
	target := "http://" + listener.Addr().String()
	if err := registerGatewayRoutes(context.Background(), apiPaths, target, service.MyService.Gateway().CreateRoute); err != nil {
		panic(err)
	}
	// v0.1: gateway route self-heal — see CasaNova/main.go for rationale.
	// Cancellable context so the watchdog goroutine exits on SIGTERM
	// instead of leaking past server shutdown.
	watchdogCtx, watchdogCancel := context.WithCancel(context.Background())
	defer watchdogCancel()
	external.StartRouteRegistrationWatchdog(
		watchdogCtx, apiPaths, target, service.MyService.Gateway().CreateRoute,
	)

	// write address file
	addressFilePath, err := writeAddressFile(config.CommonInfo.RuntimePath, external.UserServiceAddressFilename, "http://"+listener.Addr().String())
	if err != nil {
		panic(err)
	}

	if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		logger.Error("Failed to notify systemd that user service is ready", zap.Any("error", err))
	} else if supported {
		logger.Info("Notified systemd that user service is ready")
	} else {
		logger.Info("This process is not running as a systemd service.")
	}
	go route.EventListen()
	logger.Info("User service is listening...", zap.Any("address", listener.Addr().String()), zap.String("filepath", addressFilePath))

	var events []message_bus.EventType
	events = append(events, message_bus.EventType{Name: "zimaos:user:save_config", SourceID: common.SERVICENAME, PropertyTypeList: []message_bus.PropertyType{}})
	// register at message bus
	// Wave 35-X: per-attempt timeout. Background() + sleep meant a
	// wedged bus pinned UserService startup forever.
	for i := 0; i < 10; i++ {
		attemptCtx, attemptCancel := context.WithTimeout(context.Background(), 5*time.Second)
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

	s := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // fix G112: Potential slowloris attack (see https://github.com/securego/gosec)
	}

	// Graceful shutdown on SIGINT/SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		logger.Info("shutting down User Service server...")
		// Stop the route-watchdog goroutine before the HTTP server so
		// it doesn't keep POST-ing routes during shutdown.
		watchdogCancel()
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

func writeAddressFile(runtimePath string, filename string, address string) (string, error) {
	err := os.MkdirAll(runtimePath, 0o755)
	if err != nil {
		return "", err
	}

	finalPath := filepath.Join(runtimePath, filename)
	tmpPath := finalPath + ".tmp"

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write([]byte(address)); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	return finalPath, nil
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
