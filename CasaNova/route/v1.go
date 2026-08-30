package route

import (
	"crypto/ecdsa"
	"net/http"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova/common"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/config"
	v1 "github.com/LeewayOpenSoftware/CasaNova/route/v1"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

// maintenanceRoutesEnabled gates the maintenance module while its review
// findings are open. It is a constant, not a runtime setting: a feature that
// deletes files as root should not be switchable by anything an attacker can
// reach, and a build either contains the reviewed version or does not.
const maintenanceRoutesEnabled = false

var v1PreLoginRoutes = map[string]bool{
	"/v1/recover/:type":        true,
	"/v1/recover/:type/:state": true,
}

func InitV1Router() http.Handler {
	e := echo.New()

	e.Use((echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.DELETE},
		AllowHeaders: []string{echo.HeaderAuthorization, echo.HeaderContentLength, echo.HeaderXCSRFToken, echo.HeaderContentType, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders, echo.HeaderAccessControlAllowMethods, echo.HeaderConnection, echo.HeaderOrigin, echo.HeaderXRequestedWith, "Range"},
		// ExposeHeaders MUST include Content-Range and Accept-Ranges so browsers
		// can see partial-content responses. Without these, audio/video playback
		// falls back to downloading the entire file before playing.
		ExposeHeaders: []string{
			echo.HeaderContentLength,
			echo.HeaderAccessControlAllowOrigin,
			echo.HeaderAccessControlAllowHeaders,
			"Content-Range",
			"Accept-Ranges",
		},
		MaxAge: 172800,
		// AllowCredentials must be false when AllowOrigins is "*". Per the
		// CORS spec, browsers reject the combination, but Echo will happily
		// emit it; setting it true here is meaningless for cookies and gives
		// a false sense of security to anyone reading the config.
		AllowCredentials: false,
	})))
	// Gzip compresses responses, but MUST be skipped for file download/streaming
	// endpoints — it corrupts HTTP Range responses (206 Partial Content) and
	// breaks audio/video playback. Also pointless for already-compressed media.
	//
	// /public/share was missing from this list. Share links are the feature for
	// handing someone a video or a photo album, and every one of them was being
	// deflated on the way out: the compression is wasted on already-compressed
	// media, and worse, it breaks exactly the Range responses this comment says
	// it breaks — so seeking in a shared video did not work, and a byte-range
	// resume came back wrong. The list was a set of exact paths, so a route that
	// was not one of the five spellings silently opted in.
	e.Use(echo_middleware.GzipWithConfig(echo_middleware.GzipConfig{
		Skipper: func(c echo.Context) bool {
			p := c.Request().URL.Path
			return p == "/v1/file" || p == "/v1/image" || p == "/v1/batch" ||
				strings.HasPrefix(p, "/v3/file") || p == "/v1/sys/backup/download" ||
				strings.HasPrefix(p, "/public/share")
		},
	}))
	e.Use(echo_middleware.Recover())
	// Path-only logger: WebSocket auth carries the JWT in ?token=, and
	// the default Logger format includes ${uri} which would publish live
	// access tokens to journald and any log shipper.
	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{
		Format: `{"time":"${time_rfc3339}","method":"${method}","path":"${path}","status":${status},"latency_human":"${latency_human}"}` + "\n",
	}))

	e.GET("/v1/sys/version/check", v1.GetSystemCheckVersion)
	e.GET("/v1/sys/version/current", func(ctx echo.Context) error {
		return ctx.String(200, common.FullVersion())
	})
	// No "/ping" here: the gateway answers /ping itself before route dispatch
	// (CasaNova-Gateway/route/gateway_route.go), and this service's own
	// listener is on an ephemeral port, so a duplicate here could never be
	// reached by anything.
	//
	// ZimaOS-client compat probes on v1 paths (unauthenticated, pre-login).
	registerZimaCompatV1(e)
	// Public share links — deliberately OUTSIDE the JWT group: the whole
	// point is an unauthenticated URL. Security model documented in
	// file_share_links.go (crypto-random ID + optional bcrypt password
	// + optional expiry + creation-time path pinning).
	e.GET("/public/share/:id", v1.GetPublicShare)
	e.POST("/public/share/:id", v1.PostPublicShare)
	// v1PreLoginRoutes: the v1 route patterns that must answer before a session can
	// exist. Everything else under /v1 authenticates.
	//
	// Only the OAuth callback qualifies. The identity provider redirects the user's
	// BROWSER here, so the request carries no bearer token and no cookie this API
	// reads — and there is no rbac.Permission that admits the real caller while
	// excluding an attacker, because they are the same anonymous browser. What
	// separates them is the secret they carry: a 256-bit single-use state token,
	// stored only as a SHA-256 hash, bound to the provider and to the principal who
	// started the flow, which the handler consumes BEFORE reading the code and
	// before any outbound call. The half that mints it, PostCloudProviderConnect,
	// is gated on PermStorageWrite. That is a capability check, not an absent one.
	//
	// The other pre-login v1 paths do NOT belong here: /v1/sys/version/{check,current},
	// /public/share/:id and /v1/doc/features are registered on the echo instance
	// itself, outside this group, so this middleware never runs for them.

	// strictJWT returns a JWT-validation middleware identical to the one
	// mounted on v1Group, except that it does NOT honour the loopback
	// skipper. Use it on routes whose privilege-elevation potential makes
	// them unsafe to expose to any unauthenticated local caller — in
	// particular the SSH terminal endpoints, which would otherwise let a
	// less-privileged local process launder OS credentials through the
	// CasaNova daemon. The token is read from Authorization header first
	// then ?token= query (matching the WebSocket pattern).
	v1Group := e.Group("/v1")
	//	e.Any("/v1/test", v1.CheckNetwork)
	v1Group.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		Skipper: skipAuthFor(v1PreLoginRoutes),
		ParseTokenFunc: func(token string, c echo.Context) (interface{}, error) {
			valid, claims, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) { return external.GetPublicKey(config.CommonInfo.RuntimePath) })
			if err != nil || !valid {
				return nil, echo.ErrUnauthorized
			}

			// The principal's tokens may have been revoked. Until the shared
			// revocation authority existed this was checked only inside the
			// user service, and only in memory, so a deleted user's token kept
			// working against this service for its full lifetime and survived
			// restarts. Fails closed if the authority cannot be consulted.
			if jwt.RevokedForClaims(claims) {
				return nil, echo.ErrUnauthorized
			}

			c.Request().Header.Set("user_id", strconv.Itoa(claims.ID))
			// Stash RBAC role in echo.Context so RequirePermission middlewares
			// downstream don't have to re-parse the token.
			commonmw.SetRoleInContext(c, claims.Role, claims.ID)

			return claims, nil
		},
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{wsTokenExtractor},
	}))
	{
		// OAuth callback for cloud storage. Still outside any auth gate by
		// necessity — it is a provider redirect landing in a browser that
		// cannot carry a bearer token. Authorization comes from the
		// single-use state token minted at initiation
		// (POST /v1/cloud/:provider/connect); see route/v1/recover.go and
		// pkg/oauthflow. Both shapes are registered because the redirect is
		// assembled by a third-party relay we do not control: the token is
		// minted into the path, and the query forms are accepted in case the
		// relay rewrites it. Neither shape is reachable without a live token.
		v1Group.GET("/recover/:type", v1.GetRecoverStorage)
		v1Group.GET("/recover/:type/:state", v1.GetRecoverStorage)

		// Destructive storage maintenance: the external formatter and
		// free-space sanitization. Its own prefix (/v1/maintenance, published
		// in the shared gateway manifest) rather than a corner of /v1/sys,
		// because it is the one subsystem in this binary that can destroy a
		// user's data outright and it should be obvious in the route table.
		//
		// Reads are gated on PermStorageRead inside each handler — NOT
		// PermSystemRead, which guest holds. The gateway forwards every LAN
		// and internet request over 127.0.0.1, so the v1Group JWT skipper
		// (loopback && no Authorization header) lets a tokenless request from
		// anywhere through as guest; the device inventory it would otherwise
		// return is a map of which disks are disposable and which hold the
		// system.
		//
		// Every mutating route additionally carries strictJWT(), which drops
		// that loopback skipper entirely, so a tokenless caller is refused by
		// the middleware before a handler is reached at all. The handler's own
		// requireStorageMutationCaller then re-checks the permission bit AND that a
		// real principal exists. Three independent barriers, because the
		// failure mode is a formatted disk.
		storageMaintGroup := v1Group.Group("/maintenance/storage")
		{
			storageMaintGroup.GET("/devices", v1.GetStorageMaintenanceDevices)
			storageMaintGroup.GET("/capabilities", v1.GetStorageMaintenanceCapabilities)
			storageMaintGroup.GET("/health", v1.GetStorageMaintenanceHealth)
			storageMaintGroup.GET("/jobs", v1.GetStorageMaintenanceJobs)

			storageMaintGroup.POST("/format/prepare", v1.PostStorageMaintenanceFormatPrepare, strictJWT())
			storageMaintGroup.POST("/format/:id/execute", v1.PostStorageMaintenanceFormatExecute, strictJWT())
			storageMaintGroup.POST("/sanitize/prepare", v1.PostStorageMaintenanceSanitizePrepare, strictJWT())
			storageMaintGroup.POST("/sanitize/:id/execute", v1.PostStorageMaintenanceSanitizeExecute, strictJWT())
			storageMaintGroup.POST("/jobs/:id/cancel", v1.PostStorageMaintenanceJobCancel, strictJWT())
		}

		v1SysGroup := v1Group.Group("/sys")
		v1SysGroup.Use()
		{
			v1SysGroup.GET("/debug", v1.GetSystemConfigDebug) // debug — now behind JWT
			v1SysGroup.GET("/diagnosis", v1.GetDiagnosis)     // self-diagnosis engine — now behind JWT

			v1SysGroup.GET("/version", v1.GetSystemCheckVersion) // version/check

			v1SysGroup.POST("/update", v1.SystemUpdate) // legacy endpoint

			// Self-update engine — matches frontend api/updates.ts contract
			v1UpdateGroup := v1SysGroup.Group("/update")
			{
				v1UpdateGroup.GET("/status", v1.GetUpdateStatus)
				v1UpdateGroup.GET("/check", v1.GetUpdateCheck)
				v1UpdateGroup.POST("/start", v1.PostUpdateStart)
				v1UpdateGroup.POST("/cancel", v1.PostUpdateCancel)
				v1UpdateGroup.GET("/changelog", v1.GetUpdateChangelog)
				v1UpdateGroup.POST("/channel", v1.PostUpdateChannel)
				v1UpdateGroup.GET("/scan-disks", v1.GetScanDisks)
				v1UpdateGroup.POST("/from-disk", v1.PostUpdateFromDisk)
				v1UpdateGroup.POST("/upload-package", v1.PostUpdateUploadPackage)
				v1UpdateGroup.POST("/rollback", v1.PostUpdateRollback)
				v1UpdateGroup.POST("/clear-cache", v1.PostUpdateClearCache)
				v1UpdateGroup.POST("/repair", v1.PostUpdateRepair, strictJWT())
			}

			v1SysGroup.GET("/hardware", v1.GetSystemHardwareInfo) // hardware/info

			// Frame-consent probe for the Apps section: the browser cannot
			// read a cross-origin response's X-Frame-Options, so the box
			// answers on its behalf. Loopback-only by construction.
			v1SysGroup.POST("/embed-probe", v1.PostEmbedProbe)

			// Public HTTPS origins the box already publishes (tunnel ingress
			// + NPM proxy hosts), read from their own sources — no
			// credentials, no duplicate registry to keep in sync.
			v1SysGroup.GET("/remote-hosts", v1.GetRemoteHosts)

			// Host administration — OS-level knobs (Docker memory cap,
			// apt updates). CPU governor + swappiness live in
			// casanova-driver (the only service without
			// ProtectKernelTunables, so it can write /proc/sys & /sys).
			hostGroup := v1SysGroup.Group("/host")
			hostGroup.GET("/memory", v1.GetHostMemory)
			hostGroup.PUT("/memory", v1.PutHostMemory)
			hostGroup.GET("/apt/updates", v1.GetAptUpdates)
			hostGroup.POST("/apt/check", v1.PostAptCheck)
			hostGroup.GET("/apt/upgrade/status", v1.GetAptUpgradeStatus)
			// apt upgrade is the most dangerous action here (dpkg can
			// restart docker/kernel) → strictJWT on top of the handler's
			// PermSystemConfig, same as the SSH console.
			hostGroup.POST("/apt/upgrade", v1.PostAptUpgrade, strictJWT())
			// P2/P3: unattended-upgrades, curated services, UPS, processes.
			hostGroup.GET("/apt/auto", v1.GetAptAuto)
			hostGroup.PUT("/apt/auto", v1.PutAptAuto)
			hostGroup.POST("/apt/security-now", v1.PostAptSecurityNow, strictJWT())
			hostGroup.GET("/services", v1.GetHostServices)
			hostGroup.POST("/services/:key/:action", v1.PostHostServiceAction, strictJWT())
			hostGroup.GET("/ups", v1.GetHostUPS)
			hostGroup.GET("/ups/config", v1.GetUPSConfig)
			hostGroup.PUT("/ups/config", v1.PutUPSConfig)
			// UPS-aware power orchestrator (consumes the Forza /api/power
			// contract; opt-in). AutoShutdown can power off → PermSystemConfig.
			hostGroup.GET("/power", v1.GetHostPower)
			hostGroup.GET("/power/config", v1.GetHostPowerConfig)
			hostGroup.PUT("/power/config", v1.PutHostPowerConfig)
			hostGroup.GET("/processes", v1.GetHostProcesses)

			// Maintenance — reclaim disk space. Every handler in
			// route/v1/maintenance.go re-checks a real login plus
			// PermSystemConfig in its first two lines, including the
			// read-only ones: the scan enumerates paths and sizes of
			// the user's data area, which is reconnaissance, and the
			// gateway makes every LAN caller look loopback. POST
			// /clean deletes files as root, so it also gets strictJWT
			// on top — same treatment as the SSH console and the apt
			// upgrade, and for the same reason: a tokenless loopback
			// caller must never reach it. These hang off /v1/sys, a
			// prefix main.go already publishes to the gateway (the
			// /v1/trash lesson: handlers nobody routes are 404s).
			//
			// Held back from this build: the module's adversarial review
			// left open findings that all point at the same area — the
			// scratch category walks /DATA, /mnt and /media without the
			// untouchable set (every container mount + every app compose
			// volume) that design rule 1 requires, its walk cannot be
			// cancelled, and the rotated-log category pre-ticks wtmp.1 and
			// btmp.1, which are the login history. The handlers and the
			// engine ship dormant so this security release is not delayed;
			// the routes go live once those are closed.
			if maintenanceRoutesEnabled {
				maintenanceGroup := v1SysGroup.Group("/maintenance")
				maintenanceGroup.POST("/scan", v1.PostMaintenanceScan)
				maintenanceGroup.GET("/scan", v1.GetMaintenanceScan)
				maintenanceGroup.POST("/clean", v1.PostMaintenanceClean, strictJWT())
				maintenanceGroup.GET("/clean", v1.GetMaintenanceClean)
				maintenanceGroup.GET("/manifest", v1.GetMaintenanceManifests)
				maintenanceGroup.GET("/manifest/:id", v1.GetMaintenanceManifest)
			}

			// Migration assistant (CasaOS → CasaNova). Read-only
			// inspection report + a touch-file acknowledgement endpoint.
			v1SysGroup.GET("/migration/report", v1.GetMigrationReport)
			v1SysGroup.POST("/migration/acknowledge", v1.PostMigrationAcknowledge)

			// Remote-access connector inventory (Tailscale,
			// Cloudflared, ZeroTier). Read-only detection — install
			// is delegated to the upstream installers / app catalog.
			v1SysGroup.GET("/remote-access/status", v1.GetRemoteAccessStatus)

			// Theme pack manager (user-uploaded UI themes).
			v1SysGroup.GET("/themes", v1.GetThemePacks)
			v1SysGroup.POST("/themes", v1.PostThemePack)
			v1SysGroup.DELETE("/themes/:id", v1.DeleteThemePack)
			v1SysGroup.GET("/themes/:id/tokens.css", v1.ServeThemeTokens)
			v1SysGroup.GET("/themes/:id/assets/*", v1.ServeThemeAsset)

			// SSH endpoints require a valid USER token — not a peer
			// credential, and not a route exemption.
			//
			// This comment used to justify strictJWT() against the loopback
			// skipper ("service-to-service traffic on this host is
			// unauthenticated by design, cap-the-host trust"). That premise is
			// gone: the skipper no longer trusts loopback, because the gateway
			// proxies the LAN and the tunnel over it. strictJWT() still belongs
			// here for a narrower and still-valid reason — the group skipper now
			// admits a peer service holding the internal secret, and a peer
			// service must not be able to open an SSH session. A local process
			// that could read the root-only secret could otherwise POST OS
			// credentials for a different account to /v1/sys/ssh-login and get a
			// terminal as that user through the CasaNova process. strictJWT()
			// rebuilds the same JWT validation with NO skipper at all, so the
			// WebUI (which always sends a token) keeps working and everything
			// else gets 401.
			v1SysGroup.GET("/wsssh", v1.WsSsh, strictJWT())
			v1SysGroup.POST("/ssh-login", v1.PostSshLogin, strictJWT())
			// v1SysGroup.GET("/config", v1.GetSystemConfig) //delete
			// v1SysGroup.POST("/config", v1.PostSetSystemConfig)
			v1SysGroup.GET("/logs", v1.GetCasaOSErrorLogs) // error/logs
			// v1SysGroup.GET("/widget/config", v1.GetWidgetConfig)//delete
			// v1SysGroup.POST("/widget/config", v1.PostSetWidgetConfig)//delete

			// System journal logs (journalctl)
			v1SysGroup.GET("/logs/journal", v1.GetSystemLogs)
			v1SysGroup.GET("/logs/units", v1.GetLogUnits)

			// Virtualisation capability probe
			v1SysGroup.GET("/virt/capabilities", v1.GetVirtCapabilities)

			v1SysGroup.POST("/stop", v1.PostKillCasaOS)

			v1SysGroup.GET("/utilization", v1.GetSystemUtilization)
			// v1SysGroup.GET("/cpu", v1.GetSystemCupInfo)
			// v1SysGroup.GET("/mem", v1.GetSystemMemInfo)
			// v1SysGroup.GET("/disk", v1.GetSystemDiskInfo)
			// v1SysGroup.GET("/network", v1.GetSystemNetInfo)

			// /server-info was removed — the handlers were placeholders that
			// would have panicked. Frontend doesn't call this endpoint.
			// v1SysGroup.GET("/port", v1.GetCasaOSPort)
			// v1SysGroup.PUT("/port", v1.PutCasaOSPort)
			v1SysGroup.GET("/proxy", v1.GetSystemProxy)

			// System identity — hostname & timezone. Editable via hostnamectl
			// / timedatectl (both shell out under PermSystemConfig).
			v1SysGroup.GET("/hostname", v1.GetSystemHostname)
			v1SysGroup.PUT("/hostname", v1.PutSystemHostname)
			v1SysGroup.GET("/timezone", v1.GetSystemTimezone)
			v1SysGroup.PUT("/timezone", v1.PutSystemTimezone)

			// TLS configuration trio (enabled/domain/email). Persisted to
			// /etc/casanova/tls-config.json; consumed by the cert
			// issue/renew flow.
			v1SysGroup.GET("/certs/config", v1.GetTLSConfig)
			v1SysGroup.PUT("/certs/config", v1.PutTLSConfig)

			v1SysGroup.PUT("/state/:state", v1.PutSystemState)
			v1SysGroup.POST("/troubleshoot/:action", v1.PostTroubleshoot)
			v1SysGroup.GET("/entry", v1.GetSystemEntry)

			// Audit events (security / config / app lifecycle)
			v1SysGroup.GET("/audit", v1.GetAuditEvents)

			// User-facing notification center
			v1SysGroup.GET("/notifications", v1.GetNotifications)
			v1SysGroup.POST("/notifications/read", v1.PostMarkNotificationRead)
			v1SysGroup.DELETE("/notifications/:id", v1.DeleteNotification)

			// Antivirus probes
			v1SysGroup.GET("/antivirus/status", v1.GetAntivirusStatus)
			v1SysGroup.POST("/antivirus/scan", v1.PostAntivirusScan)
			v1SysGroup.POST("/antivirus/update-signatures", v1.PostAntivirusUpdateSigs)

			// SMART disk health monitoring
			v1SysGroup.GET("/smart", v1.GetSmartSummary)
			v1SysGroup.POST("/smart/test", v1.PostSmartTest)
			v1SysGroup.GET("/smart/schedule", v1.GetSmartSchedule)
			v1SysGroup.POST("/smart/schedule", v1.PostSmartSchedule)
			v1SysGroup.DELETE("/smart/schedule", v1.DeleteSmartSchedule)
			v1SysGroup.GET("/smart/results", v1.GetSmartResults)

			// System backup & restore
			v1SysGroup.POST("/backup/create", v1.PostBackupCreate)
			v1SysGroup.GET("/backup/list", v1.GetBackupList)
			v1SysGroup.GET("/backup/download", v1.GetBackupDownload)
			v1SysGroup.POST("/backup/restore", v1.PostBackupRestore)
			v1SysGroup.DELETE("/backup", v1.DeleteBackup)

			// Cloud backup (Rclone)
			v1SysGroup.GET("/cloud/status", v1.GetCloudBackupStatus)
			v1SysGroup.GET("/cloud/providers", v1.GetCloudBackupProviders)
			v1SysGroup.POST("/cloud/remotes", v1.PostCloudBackupRemote)
			v1SysGroup.DELETE("/cloud/remotes/:name", v1.DeleteCloudBackupRemote)
			v1SysGroup.POST("/cloud/sync", v1.PostCloudBackupSync)
			v1SysGroup.GET("/cloud/sync/status", v1.GetCloudBackupSyncStatus)
			v1SysGroup.POST("/cloud/config", v1.PostCloudBackupConfig)

			// ZFS — detection + pool inventory (Tier 1) and safe ops
			// (Tier 2: scrub start/stop, import, export). Snapshot
			// management was already wired separately below.
			v1SysGroup.GET("/zfs/status", v1.GetZFSStatus)
			v1SysGroup.GET("/zfs/pools", v1.GetZFSPools)
			v1SysGroup.POST("/zfs/scrub", v1.PostZFSScrub)
			v1SysGroup.POST("/zfs/import", v1.PostZFSImport)
			v1SysGroup.POST("/zfs/export", v1.PostZFSExport)
			// Tier 3 — destructive lifecycle. Gated by "advanced
			// developer mode" in the UI; the backend's only guard is
			// PermStorageWrite + handler-level safety checks (system
			// disk, name confirm, topology min-devices).
			v1SysGroup.POST("/zfs/pools", v1.PostZFSCreatePool)
			v1SysGroup.DELETE("/zfs/pools", v1.DeleteZFSPool)
			v1SysGroup.POST("/zfs/replace", v1.PostZFSReplace)
			v1SysGroup.GET("/zfs/snapshots", v1.GetZFSSnapshots)
			v1SysGroup.POST("/zfs/snapshots", v1.PostZFSSnapshot)
			v1SysGroup.DELETE("/zfs/snapshots", v1.DeleteZFSSnapshot)
			v1SysGroup.POST("/zfs/snapshots/rollback", v1.PostZFSRollback)
			v1SysGroup.GET("/zfs/datasets", v1.GetZFSDatasets)

			// Two-factor authentication (TOTP)
			v1SysGroup.GET("/2fa/status", v1.GetTOTPStatus)
			v1SysGroup.POST("/2fa/setup", v1.PostTOTPSetup)
			v1SysGroup.POST("/2fa/verify", v1.PostTOTPVerify)
			v1SysGroup.POST("/2fa/validate", v1.PostTOTPValidate)
			v1SysGroup.DELETE("/2fa", v1.DeleteTOTP)

			// TLS certificate management (Let's Encrypt / ACME)
			v1SysGroup.GET("/certs/status", v1.GetCertsStatus)
			v1SysGroup.GET("/certs/check", v1.GetCertsCheck)
			v1SysGroup.POST("/certs/issue", v1.PostCertsIssue)
			v1SysGroup.POST("/certs/renew", v1.PostCertsRenew)
			v1SysGroup.DELETE("/certs", v1.DeleteCerts)

			// Macvlan network management (developer mode)
			v1SysGroup.GET("/network/macvlan", v1.GetMacvlanConfig)
			v1SysGroup.POST("/network/macvlan", v1.PostMacvlanConfig)
			v1SysGroup.DELETE("/network/macvlan", v1.DeleteMacvlanConfig)
			v1SysGroup.PUT("/network/macvlan/app/:id", v1.PutMacvlanApp)

			// Alert threshold rules
			v1SysGroup.GET("/alerts/rules", v1.GetAlertRules)
			v1SysGroup.POST("/alerts/rules", v1.PostAlertRule)
			v1SysGroup.PUT("/alerts/rules/:id", v1.PutAlertRule)
			v1SysGroup.DELETE("/alerts/rules/:id", v1.DeleteAlertRule)
			v1SysGroup.GET("/alerts/history", v1.GetAlertHistory)
			v1SysGroup.POST("/alerts/test/:id", v1.PostAlertTest)

			// Cron job management (scheduled tasks)
			v1SysGroup.GET("/cron", v1.GetCronJobs)
			v1SysGroup.POST("/cron", v1.PostCronJob)
			v1SysGroup.PUT("/cron/:id", v1.PutCronJob)
			v1SysGroup.DELETE("/cron/:id", v1.DeleteCronJob)
			v1SysGroup.POST("/cron/:id/toggle", v1.PostCronToggle)

			// SMB/Samba file sharing management
			v1SysGroup.GET("/smb/status", v1.GetSmbStatus)
			v1SysGroup.POST("/smb/shares", v1.PostSmbShare)
			v1SysGroup.PUT("/smb/shares/:name", v1.PutSmbShare)
			v1SysGroup.DELETE("/smb/shares/:name", v1.DeleteSmbShare)
			v1SysGroup.POST("/smb/toggle", v1.PostSmbToggle)
			v1SysGroup.GET("/smb/browse", v1.GetSmbBrowse)
		}
		// Docker image tag lookup (registry API)
		v1Group.GET("/container/image/tags", v1.GetDockerImageTags)

		// Container discovery — lists ALL Docker containers (not just managed).
		//
		// This service may only serve container paths whose gateway prefix it
		// owns, and app-management owns the generic "/v1/container". The two
		// routes below are the static carve-outs declared in the route
		// manifest (plus "/v1/container/image/tags" above); anything shaped
		// "/v1/container/:id/..." is unroutable here, because the gateway
		// matches literal prefixes and no literal prefix can cover an
		// arbitrary container id. Per-container start/stop/restart, terminal
		// and log streaming therefore live in app-management. Registering
		// them here again would compile, look implemented, and receive
		// nothing — which is exactly what happened to container logs.
		v1ContainerGroup := v1Group.Group("/container")
		{
			v1ContainerGroup.GET("/all", v1.GetAllContainers)
			v1ContainerGroup.GET("/stats", v1.GetContainerResourceStats)
		}

		v1PortGroup := v1Group.Group("/port")
		v1PortGroup.Use()
		{
			v1PortGroup.GET("/", v1.GetPort)              // app/port
			v1PortGroup.GET("/state/:port", v1.PortCheck) // app/check/:port
		}
		v1FileGroup := v1Group.Group("/file")
		v1FileGroup.Use()
		{
			v1FileGroup.GET("", v1.GetDownloadSingleFile) // download/:path
			v1FileGroup.POST("", v1.PostCreateFile)
			v1FileGroup.PUT("", v1.PutFileContent)
			v1FileGroup.PUT("/name", v1.RenamePath)
			// file/rename
			v1FileGroup.GET("/content", v1.GetFilerContent) // file/read
			v1FileGroup.GET("/search", v1.GetFileSearch)    // recursive name search

			// Archive ops + share links (Sprint B file-manager parity)
			v1FileGroup.POST("/compress", v1.PostFileCompress)
			v1FileGroup.POST("/extract", v1.PostFileExtract)
			v1FileGroup.POST("/share-links", v1.PostCreateShareLink)
			v1FileGroup.GET("/share-links", v1.GetShareLinks)
			v1FileGroup.DELETE("/share-links/:id", v1.DeleteShareLink)

			// File uploads need to be handled separately, and will not be modified here
			// v1FileGroup.POST("/upload", v1.PostFileUpload)
			v1FileGroup.POST("/upload", v1.PostFileUpload)
			v1FileGroup.GET("/upload", v1.GetFileUpload)
			// v1FileGroup.GET("/download", v1.UserFileDownloadCommonService)
			v1FileGroup.GET("/ws", v1.ConnectWebSocket)
			v1FileGroup.GET("/peers", v1.GetPeers)
		}
		v1CloudGroup := v1Group.Group("/cloud")
		v1CloudGroup.Use()
		{
			v1CloudGroup.GET("", v1.ListStorages)
			v1CloudGroup.DELETE("", v1.UmountStorage)
			// Initiation half of the OAuth callback state machine, and the
			// callback authority's structured health.
			v1CloudGroup.GET("/oauth-health", v1.GetCloudOAuthHealth)
			v1CloudGroup.POST("/:provider/connect", v1.PostCloudProviderConnect)
		}
		v1DriverGroup := v1Group.Group("/driver")
		v1DriverGroup.Use()
		{
			v1DriverGroup.GET("", v1.ListDriverInfo)
		}

		v1FolderGroup := v1Group.Group("/folder")
		v1FolderGroup.Use()
		{
			v1FolderGroup.PUT("/name", v1.RenamePath)
			v1FolderGroup.GET("", v1.DirPath)   ///file/dirpath
			v1FolderGroup.POST("", v1.MkdirAll) ///file/mkdir
			v1FolderGroup.GET("/size", v1.GetSize)
			v1FolderGroup.GET("/count", v1.GetFileCount)
		}
		v1BatchGroup := v1Group.Group("/batch")
		v1BatchGroup.Use()
		{

			v1BatchGroup.DELETE("", v1.DeleteFile) // file/delete (routes to trash by default)
			v1BatchGroup.DELETE("/:id/task", v1.DeleteOperateFileOrDir)
			v1BatchGroup.POST("/task", v1.PostOperateFileOrDir) // file/operate
			v1BatchGroup.GET("", v1.GetDownloadFile)

			// Parked workers: list is readable, discard is admin-gated in the
			// handler. Without these two, a reservation held by a mount that
			// never comes back is invisible and permanent.
			v1BatchGroup.GET("/wedged", v1.GetWedgedFileOps)
			v1BatchGroup.DELETE("/wedged/:id", v1.DeleteWedgeReservation)
		}
		// Recycle bin endpoints — XDG-compliant per-filesystem trash.
		// Deleted files are moved here (near-instant rename) instead of being
		// permanently removed. Auto-purged after 30 days via background janitor.
		v1TrashGroup := v1Group.Group("/trash")
		{
			v1TrashGroup.GET("", v1.GetTrashList)              // list items in trash
			v1TrashGroup.POST("/restore", v1.PostTrashRestore) // restore items
			v1TrashGroup.DELETE("", v1.DeleteTrashItem)        // permanently delete (empty all if no ids)
		}
		v1ImageGroup := v1Group.Group("/image")
		v1ImageGroup.Use()
		{
			v1ImageGroup.GET("", v1.GetFileImage)
		}
		v1SambaGroup := v1Group.Group("/samba")
		v1SambaGroup.Use()
		{
			v1ConnectionsGroup := v1SambaGroup.Group("/connections")
			v1ConnectionsGroup.Use()
			{
				v1ConnectionsGroup.GET("", v1.GetSambaConnectionsList)
				v1ConnectionsGroup.POST("", v1.PostSambaConnectionsCreate)
				v1ConnectionsGroup.DELETE("/:id", v1.DeleteSambaConnections)
			}
			v1SharesGroup := v1SambaGroup.Group("/shares")
			v1SharesGroup.Use()
			{
				v1SharesGroup.GET("", v1.GetSambaSharesList)
				v1SharesGroup.POST("", v1.PostSambaSharesCreate)
				v1SharesGroup.DELETE("/:id", v1.DeleteSambaShares)
				v1SharesGroup.GET("/status", v1.GetSambaStatus)
			}
		}
		v1NotifyGroup := v1Group.Group("/notify")
		v1NotifyGroup.Use()
		{
			v1NotifyGroup.POST("/:path", v1.PostNotifyMessage)
			// merge to system
			v1NotifyGroup.POST("/system_status", v1.PostSystemStatusNotify)
		}

		v1OtherGroup := v1Group.Group("/other")
		v1OtherGroup.Use()
		{
			v1OtherGroup.GET("/search", v1.GetSearchResult)
		}
		v1ZerotierGroup := v1Group.Group("/zt")
		v1ZerotierGroup.Use()
		{
			v1ZerotierGroup.Any("/*url", v1.ZerotierProxy)
		}
	}

	return e
}

func strictJWT() echo.MiddlewareFunc {
	return echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		ParseTokenFunc: func(token string, c echo.Context) (interface{}, error) {
			valid, claims, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) {
				return external.GetPublicKey(config.CommonInfo.RuntimePath)
			})
			if err != nil || !valid {
				return nil, echo.ErrUnauthorized
			}
			// The principal's tokens may have been revoked. Until the shared
			// revocation authority existed this was checked only inside the
			// user service, and only in memory, so a deleted user's token kept
			// working against this service for its full lifetime and survived
			// restarts. Fails closed if the authority cannot be consulted.
			if jwt.RevokedForClaims(claims) {
				return nil, echo.ErrUnauthorized
			}

			c.Request().Header.Set("user_id", strconv.Itoa(claims.ID))
			commonmw.SetRoleInContext(c, claims.Role, claims.ID)
			return claims, nil
		},
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{wsTokenExtractor},
	})
}

// wsTokenExtractor reads the bearer token from the Authorization header,
// falling back to the query string for WebSocket handshakes (browsers cannot
// set headers there).
//
// ONE function, shared by the v1Group middleware and strictJWT. They were
// separate inline literals and drifted: strictJWT learned to read ?jwt= while
// the group extractor still read only ?token=, so /v1/sys/wsssh — which sends
// ?jwt=<JWT>&token=<opaque ssh session id> — had its hex session id handed to
// jwt.Validate and 401'd at the group before strictJWT ever ran. The
// integrated terminal died at exactly that point, with the route-level fix
// sitting unreachable behind a group-level refusal. Sharing one function is
// what stops them drifting again; ws_token_extractor_test.go pins it.
func wsTokenExtractor(ctx echo.Context) ([]string, error) {
	auth := ctx.Request().Header.Get(echo.HeaderAuthorization)
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return []string{auth[7:]}, nil
	}
	if len(auth) > 0 {
		return []string{auth}, nil
	}
	if jwtParam := ctx.QueryParam("jwt"); jwtParam != "" {
		return []string{jwtParam}, nil
	}
	return []string{ctx.QueryParam("token")}, nil
}
