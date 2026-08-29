package v1

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	sshHelper "github.com/LeewayOpenSoftware/CasaNova-Common/utils/ssh"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils"
	"github.com/labstack/echo/v4"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	modelCommon "github.com/LeewayOpenSoftware/CasaNova-Common/model"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:   1024,
	WriteBufferSize:  1024,
	HandshakeTimeout: 5 * time.Second,
	// CheckOrigin uses gorilla's default: same-origin or no-origin (non-browser).
}

// sshSession holds validated SSH credentials associated with a single-use
// session token. Created by PostSshLogin, consumed by WsSsh. The token
// is the only thing that travels in the WebSocket URL — credentials
// never appear in query strings, server logs, or browser history.
type sshSession struct {
	Username  string
	Password  string
	Port      string
	CreatedAt time.Time
}

var (
	sshSessionsMu sync.Mutex
	sshSessions   = make(map[string]*sshSession)
)

const sshSessionTTL = 60 * time.Second // token valid for 60s after creation
const maxSessions = 100                // cap active session tokens to prevent memory exhaustion

// generateToken returns a 32-byte hex-encoded random token.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// cleanExpiredSessions removes tokens older than sshSessionTTL.
// Called under lock from PostSshLogin to keep the map bounded.
func cleanExpiredSessions() {
	now := time.Now()
	for token, sess := range sshSessions {
		if now.Sub(sess.CreatedAt) > sshSessionTTL {
			delete(sshSessions, token)
		}
	}
}

// sshLoginLimiter throttles SSH credential attempts per client IP. Even gated
// to admins this stays: a stolen admin session should not become an unbounded
// OS-password brute-force engine, and the SSH server itself never sees these
// attempts as remote (they arrive on 127.0.0.1) so fail2ban cannot see them.
var (
	sshLoginAttemptsMu sync.Mutex
	sshLoginAttempts   = map[string][]time.Time{}
)

const (
	sshLoginWindow   = time.Minute
	sshLoginMaxTries = 5
)

func sshLoginAllowed(ip string) bool {
	sshLoginAttemptsMu.Lock()
	defer sshLoginAttemptsMu.Unlock()
	now := time.Now()
	kept := sshLoginAttempts[ip][:0]
	for _, at := range sshLoginAttempts[ip] {
		if now.Sub(at) < sshLoginWindow {
			kept = append(kept, at)
		}
	}
	if len(kept) >= sshLoginMaxTries {
		sshLoginAttempts[ip] = kept
		return false
	}
	sshLoginAttempts[ip] = append(kept, now)
	return true
}

func PostSshLogin(ctx echo.Context) error {
	// Host shell / host credentials: admin only. These were the only
	// privileged v1 handlers with strictJWT() but no permission check, so any
	// authenticated role (guest included) could brute-force OS passwords
	// against 127.0.0.1 — the 200-vs-error reply is a perfect oracle — and on
	// success get an interactive root-capable shell.
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if !sshLoginAllowed(ctx.RealIP()) {
		return ctx.JSON(http.StatusTooManyRequests, modelCommon.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "too many SSH login attempts; wait a minute",
		})
	}
	j := make(map[string]string)
	if err := ctx.Bind(&j); err != nil {
		return ctx.JSON(http.StatusBadRequest, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: err.Error()})
	}
	userName := j["username"]
	password := j["password"]
	port := j["port"]
	if userName == "" || password == "" || port == "" {
		return ctx.JSON(common_err.CLIENT_ERROR, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: common_err.GetMsg(common_err.INVALID_PARAMS), Data: "Username or password or port is empty"})
	}

	// Validate credentials by attempting a real SSH connection.
	testClient, err := sshHelper.NewSshClient(userName, password, port)
	if err != nil {
		logger.Error("connect ssh error", zap.Any("error", err))
		return ctx.JSON(common_err.CLIENT_ERROR, modelCommon.Result{Success: common_err.CLIENT_ERROR, Message: common_err.GetMsg(common_err.CLIENT_ERROR), Data: "Please check if the username and port are correct, and make sure that ssh server is installed."})
	}
	testClient.Close()

	// Generate a single-use session token. Credentials stay server-side.
	token, err := generateToken()
	if err != nil {
		logger.Error("generate ssh session token error", zap.Any("error", err))
		return ctx.JSON(common_err.SERVICE_ERROR, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "Failed to create session"})
	}

	sshSessionsMu.Lock()
	cleanExpiredSessions()
	if len(sshSessions) >= maxSessions {
		sshSessionsMu.Unlock()
		logger.Error("SSH session limit reached", zap.Int("max", maxSessions))
		return ctx.JSON(common_err.SERVICE_ERROR, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: "Too many active sessions, please try again later"})
	}
	sshSessions[token] = &sshSession{
		Username:  userName,
		Password:  password,
		Port:      port,
		CreatedAt: time.Now(),
	}
	sshSessionsMu.Unlock()

	return ctx.JSON(common_err.SUCCESS, modelCommon.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"session_id": token,
			"expires_at": time.Now().Add(sshSessionTTL).Unix(),
		},
	})
}

func WsSsh(ctx echo.Context) error {
	// Host shell / host credentials: admin only. These were the only
	// privileged v1 handlers with strictJWT() but no permission check, so any
	// authenticated role (guest included) could brute-force OS passwords
	// against 127.0.0.1 — the 200-vs-error reply is a perfect oracle — and on
	// success get an interactive root-capable shell.
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	_, e := exec.LookPath("ssh")
	if e != nil {
		return ctx.JSON(common_err.SERVICE_ERROR, modelCommon.Result{Success: common_err.SERVICE_ERROR, Message: common_err.GetMsg(common_err.SERVICE_ERROR), Data: "ssh server not found"})
	}

	// Authenticate via session token — NOT raw credentials.
	token := ctx.QueryParam("token")
	if token == "" {
		return ctx.JSON(http.StatusUnauthorized, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: "Missing session token"})
	}

	// Consume the token (single-use).
	sshSessionsMu.Lock()
	sess, ok := sshSessions[token]
	if ok {
		delete(sshSessions, token)
	}
	sshSessionsMu.Unlock()

	if !ok {
		return ctx.JSON(http.StatusUnauthorized, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: "Invalid or expired session token"})
	}

	// Check TTL
	if time.Since(sess.CreatedAt) > sshSessionTTL {
		return ctx.JSON(http.StatusUnauthorized, modelCommon.Result{Success: common_err.INVALID_PARAMS, Message: "Session token expired"})
	}

	userName := sess.Username
	password := sess.Password
	port := sess.Port

	wsConn, err := upgrader.Upgrade(ctx.Response().Writer, ctx.Request(), nil)
	if err != nil {
		logger.Error("websocket upgrade failed", zap.Error(err))
		return nil // Upgrade already wrote the HTTP error response
	}

	logBuff := new(bytes.Buffer)
	quitChan := make(chan bool, 3)

	cols, _ := strconv.Atoi(utils.DefaultQuery(ctx, "cols", "200"))
	rows, _ := strconv.Atoi(utils.DefaultQuery(ctx, "rows", "32"))
	if cols < 1 || cols > 500 {
		cols = 200
	}
	if rows < 1 || rows > 200 {
		rows = 32
	}

	client, err := sshHelper.NewSshClient(userName, password, port)
	if err != nil || client == nil {
		msg := "SSH connection failed"
		if err != nil {
			msg = err.Error()
		}
		wsConn.WriteMessage(websocket.TextMessage, []byte(msg+"\r\n"))
		wsConn.Close()
		return nil
	}

	ssConn, err := sshHelper.NewSshConn(cols, rows, client)
	if err != nil {
		wsConn.WriteMessage(websocket.TextMessage, []byte("Failed to open SSH session\r\n"))
		wsConn.Close()
		client.Close()
		return nil
	}

	go ssConn.ReceiveWsMsg(wsConn, logBuff, quitChan)
	go ssConn.SendComboOutput(wsConn, quitChan)
	go ssConn.SessionWait(quitChan)

	// Wait for the first goroutine to signal exit.
	<-quitChan

	// Signal remaining goroutines so they don't leak. Non-blocking sends
	// cover the case where a goroutine already exited on its own.
	select {
	case quitChan <- true:
	default:
	}
	select {
	case quitChan <- true:
	default:
	}

	// Close resources; this also unblocks goroutines stuck in I/O.
	ssConn.Close()
	client.Close()
	wsConn.Close()

	return nil
}
