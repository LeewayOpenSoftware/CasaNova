package ssh

import (
	"bytes"
	json2 "encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

func NewSshClient(user, password string, port string) (*ssh.Client, error) {
	// connet to ssh
	// addr = fmt.Sprintf("%s:%d", host, port)

	config := &ssh.ClientConfig{
		Timeout: time.Second * 5,
		User:    user,
		// Allow host-key verification only for localhost connections (the
		// CasaNova terminal feature connects to the local machine).
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			host, _, _ := net.SplitHostPort(hostname)
			if host == "127.0.0.1" || host == "::1" || host == "localhost" {
				return nil // Local connections are trusted
			}
			return fmt.Errorf("ssh: host key verification failed for %s", hostname)
		},
		// HostKeyCallback: ,
		// HostKeyCallback: hostKeyCallBackFunc(h.Host),
	}
	// if h.Type == "password" {
	config.Auth = []ssh.AuthMethod{ssh.Password(password)}
	//} else {
	//	config.Auth = []ssh.AuthMethod{publicKeyAuthFunc(h.Key)}
	//}
	addr := fmt.Sprintf("%s:%s", "127.0.0.1", port)
	c, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// setup ssh shell session
// set Session and StdinPipe here,
// and the Session.Stdout and Session.Sdterr are also set.
func NewSshConn(cols, rows int, sshClient *ssh.Client) (*SshConn, error) {
	sshSession, err := sshClient.NewSession()
	if err != nil {
		return nil, err
	}

	stdinP, err := sshSession.StdinPipe()
	if err != nil {
		return nil, err
	}
	comboWriter := new(wsBufferWriter)

	sshSession.Stdout = comboWriter
	sshSession.Stderr = comboWriter

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,     // disable echo
		ssh.TTY_OP_ISPEED: 14400, // input speed = 14.4kbaud
		ssh.TTY_OP_OSPEED: 14400, // output speed = 14.4kbaud
	}
	// Request pseudo terminal
	if err := sshSession.RequestPty("xterm", rows, cols, modes); err != nil {
		return nil, err
	}
	// Start remote shell
	if err := sshSession.Shell(); err != nil {
		return nil, err
	}
	return &SshConn{StdinPipe: stdinP, ComboOutput: comboWriter, Session: sshSession}, nil
}

type SshConn struct {
	// calling Write() to write data into ssh server
	StdinPipe io.WriteCloser
	// Write() be called to receive data from ssh server
	ComboOutput *wsBufferWriter
	Session     *ssh.Session
}
type wsBufferWriter struct {
	buffer bytes.Buffer
	mu     sync.Mutex
}

func (w *wsBufferWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.Write(p)
}

func (s *SshConn) Close() {
	if s.Session != nil {
		s.Session.Close()
	}
}

const (
	wsMsgCmd    = "cmd"
	wsMsgResize = "resize"
)

// ReceiveWsMsg  receive websocket msg do some handling then write into ssh.session.stdin
func ReceiveWsMsgUser(wsConn *websocket.Conn, logBuff *bytes.Buffer) string {
	// tells other go routine quit
	username := ""
	for {

		// read websocket msg
		_, wsData, err := wsConn.ReadMessage()
		if err != nil {
			return ""
		}

		msgObj := wsMsg{}
		if err := json2.Unmarshal(wsData, &msgObj); err != nil {
			msgObj.Type = "cmd"
			msgObj.Cmd = string(wsData)
		}
		//if err := json.Unmarshal(wsData, &msgObj); err != nil {
		//	logrus.WithError(err).WithField("wsData", string(wsData)).Error("unmarshal websocket message failed")
		//}
		switch msgObj.Type {
		case wsMsgCmd:
			// handle xterm.js stdin
			// decodeBytes, err := base64.StdEncoding.DecodeString(msgObj.Cmd)
			decodeBytes := []byte(msgObj.Cmd)
			if msgObj.Cmd == "\u007f" {
				if len(username) == 0 {
					continue
				}
				wsConn.WriteMessage(websocket.TextMessage, []byte("\b\x1b[K"))
				username = username[:len(username)-1]
				continue
			}
			if msgObj.Cmd == "\r" {
				return username
			}
			username += msgObj.Cmd

			if err := wsConn.WriteMessage(websocket.TextMessage, decodeBytes); err != nil {
				logrus.WithError(err).Error("ws cmd bytes write to ssh.stdin pipe failed")
			}
			// write input cmd to log buffer
			if _, err := logBuff.Write(decodeBytes); err != nil {
				logrus.WithError(err).Error("write received cmd into log buffer failed")
			}
		}

	}
}

func ReceiveWsMsgPassword(wsConn *websocket.Conn, logBuff *bytes.Buffer) string {
	// tells other go routine quit
	password := ""
	for {

		// read websocket msg
		_, wsData, err := wsConn.ReadMessage()
		if err != nil {
			logrus.WithError(err).Error("reading webSocket message failed")
			return ""
		}

		msgObj := wsMsg{}
		if err := json2.Unmarshal(wsData, &msgObj); err != nil {
			msgObj.Type = "cmd"
			msgObj.Cmd = string(wsData)
		}
		//if err := json.Unmarshal(wsData, &msgObj); err != nil {
		//	logrus.WithError(err).WithField("wsData", string(wsData)).Error("unmarshal websocket message failed")
		//}
		switch msgObj.Type {
		case wsMsgCmd:
			// handle xterm.js stdin
			// decodeBytes, err := base64.StdEncoding.DecodeString(msgObj.Cmd)
			if msgObj.Cmd == "\r" {
				return password
			}

			if msgObj.Cmd == "\u007f" {
				if len(password) == 0 {
					continue
				}
				password = password[:len(password)-1]
				continue
			}
			password += msgObj.Cmd
		}

	}
}

// ReceiveWsMsg  receive websocket msg do some handling then write into ssh.session.stdin
func (ssConn *SshConn) ReceiveWsMsg(wsConn *websocket.Conn, logBuff *bytes.Buffer, exitCh chan bool) {
	// tells other go routine quit
	defer setQuit(exitCh)
	for {
		select {
		case <-exitCh:
			return
		default:
			// read websocket msg
			_, wsData, err := wsConn.ReadMessage()
			if err != nil {
				logrus.WithError(err).Error("reading webSocket message failed")
				return
			}
			//unmashal bytes into struct
			//msgObj := wsMsg{
			//	Type: "cmd",
			//	Cmd:  "",
			//	Rows: 50,
			//	Cols: 180,
			//}
			msgObj := wsMsg{}
			if err := json2.Unmarshal(wsData, &msgObj); err != nil {
				msgObj.Type = "cmd"
				msgObj.Cmd = string(wsData)
			}
			//if err := json.Unmarshal(wsData, &msgObj); err != nil {
			//	logrus.WithError(err).WithField("wsData", string(wsData)).Error("unmarshal websocket message failed")
			//}
			switch msgObj.Type {

			case wsMsgResize:
				// handle xterm.js size change
				if msgObj.Cols > 0 && msgObj.Rows > 0 {
					if err := ssConn.Session.WindowChange(msgObj.Rows, msgObj.Cols); err != nil {
						logrus.WithError(err).Error("ssh pty change windows size failed")
					}
				}
			case wsMsgCmd:
				// handle xterm.js stdin
				// decodeBytes, err := base64.StdEncoding.DecodeString(msgObj.Cmd)
				decodeBytes := []byte(msgObj.Cmd)
				if err != nil {
					logrus.WithError(err).Error("websock cmd string base64 decoding failed")
				}
				if _, err := ssConn.StdinPipe.Write(decodeBytes); err != nil {
					logrus.WithError(err).Error("ws cmd bytes write to ssh.stdin pipe failed")
				}
				// write input cmd to log buffer
				if _, err := logBuff.Write(decodeBytes); err != nil {
					logrus.WithError(err).Error("write received cmd into log buffer failed")
				}
			}
		}
	}
}

func (ssConn *SshConn) SendComboOutput(wsConn *websocket.Conn, exitCh chan bool) {
	// tells other go routine quit
	// defer setQuit(exitCh)

	// every 120ms write combine output bytes into websocket response
	tick := time.NewTicker(time.Millisecond * time.Duration(120))
	// for range time.Tick(120 * time.Millisecond){}
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			// write combine output bytes into websocket response
			if err := flushComboOutput(ssConn.ComboOutput, wsConn); err != nil {
				logrus.WithError(err).Error("ssh sending combo output to webSocket failed")
				return
			}
		case <-exitCh:
			return
		}
	}
}

func flushComboOutput(w *wsBufferWriter, wsConn *websocket.Conn) error {
	if w.buffer.Len() != 0 {
		err := wsConn.WriteMessage(websocket.TextMessage, w.buffer.Bytes())
		if err != nil {
			return err
		}
		w.buffer.Reset()
	}
	return nil
}

// ReceiveWsMsg  receive websocket msg do some handling then write into ssh.session.stdin
func (ssConn *SshConn) Login(wsConn *websocket.Conn, logBuff *bytes.Buffer, exitCh chan bool) {
	// tells other go routine quit
	defer setQuit(exitCh)
	for {
		select {
		case <-exitCh:
			return
		default:
			// read websocket msg
			_, wsData, err := wsConn.ReadMessage()
			if err != nil {
				logrus.WithError(err).Error("reading webSocket message failed")
				return
			}
			//unmashal bytes into struct
			//msgObj := wsMsg{
			//	Type: "cmd",
			//	Cmd:  "",
			//	Rows: 50,
			//	Cols: 180,
			//}
			msgObj := wsMsg{}
			if err := json2.Unmarshal(wsData, &msgObj); err != nil {
				msgObj.Type = "cmd"
				msgObj.Cmd = string(wsData)
			}
			//if err := json.Unmarshal(wsData, &msgObj); err != nil {
			//	logrus.WithError(err).WithField("wsData", string(wsData)).Error("unmarshal websocket message failed")
			//}
			switch msgObj.Type {

			case wsMsgResize:
				// handle xterm.js size change
				if msgObj.Cols > 0 && msgObj.Rows > 0 {
					if err := ssConn.Session.WindowChange(msgObj.Rows, msgObj.Cols); err != nil {
						logrus.WithError(err).Error("ssh pty change windows size failed")
					}
				}
			case wsMsgCmd:
				// handle xterm.js stdin
				// decodeBytes, err := base64.StdEncoding.DecodeString(msgObj.Cmd)
				decodeBytes := []byte(msgObj.Cmd)
				if err != nil {
					logrus.WithError(err).Error("websock cmd string base64 decoding failed")
				}
				if _, err := ssConn.StdinPipe.Write(decodeBytes); err != nil {
					logrus.WithError(err).Error("ws cmd bytes write to ssh.stdin pipe failed")
				}
				// write input cmd to log buffer
				if _, err := logBuff.Write(decodeBytes); err != nil {
					logrus.WithError(err).Error("write received cmd into log buffer failed")
				}
			}
		}
	}
}

func (ssConn *SshConn) SessionWait(quitChan chan bool) {
	if err := ssConn.Session.Wait(); err != nil {
		logrus.WithError(err).Error("ssh session wait failed")
		setQuit(quitChan)
	}
}

func setQuit(ch chan bool) {
	ch <- true
}

type wsMsg struct {
	Type string `json:"type"`
	Cmd  string `json:"cmd"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// (see function name)
func WsWriterCopy(reader io.Reader, writer *websocket.Conn) {
	buf := make([]byte, 32768) // 32KB buffer for better throughput
	for {
		nr, err := reader.Read(buf)
		if nr > 0 {
			if writeErr := writer.WriteMessage(websocket.BinaryMessage, buf[0:nr]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// WsWriterCopyLocked is the concurrency-safe variant of WsWriterCopy.
// Gorilla's *websocket.Conn forbids concurrent writers; callers that
// also run a ping-ticker goroutine on the same conn MUST share a mutex
// between the two writers via this helper, otherwise the runtime
// crashes the entire process with "concurrent write to websocket
// connection". The mu argument is acquired around every write.
func WsWriterCopyLocked(reader io.Reader, writer *websocket.Conn, mu *sync.Mutex) {
	buf := make([]byte, 32768)
	for {
		nr, err := reader.Read(buf)
		if nr > 0 {
			mu.Lock()
			writeErr := writer.WriteMessage(websocket.BinaryMessage, buf[0:nr])
			mu.Unlock()
			if writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// (see function name)
func WsReaderCopy(reader *websocket.Conn, writer io.Writer) {
	for {
		messageType, p, err := reader.ReadMessage()
		if err != nil {
			return
		}
		if messageType == websocket.TextMessage {
			msgObj := wsMsg{}
			if err = json2.Unmarshal(p, &msgObj); err != nil {
				// Not valid JSON - treat as raw text input
				writer.Write(p)
			} else if msgObj.Type == wsMsgResize {
				// Send resize command to the container shell
				if msgObj.Rows > 0 && msgObj.Cols > 0 {
					resizeCmd := fmt.Sprintf("stty rows %d cols %d\n", msgObj.Rows, msgObj.Cols)
					writer.Write([]byte(resizeCmd))
				}
			}
		} else if messageType == websocket.BinaryMessage {
			writer.Write(p)
		}
	}
}
