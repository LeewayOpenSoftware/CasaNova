package service

import (
	"context"
	"encoding/json"
	json2 "encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova/common"
	model2 "github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/model/notify"
	"github.com/LeewayOpenSoftware/CasaNova/service/model"
	"github.com/LeewayOpenSoftware/CasaNova/types"
	"go.uber.org/zap"
	"golang.org/x/sync/syncmap"

	socketio "github.com/googollee/go-socket.io"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

type NotifyServer interface {
	GetLog(id string) model.AppNotify
	AddLog(log model.AppNotify)
	UpdateLog(log model.AppNotify)
	UpdateLogByCustomID(log model.AppNotify)
	DelLog(id string)
	GetList(c int) (list []model.AppNotify)
	MarkRead(id string, state int)
	//	SendText(m model.AppNotify)
	//	SendUninstallAppBySocket(app notifyCommon.Application)

	SendFileOperateNotify(nowSend bool)
	//SendInstallAppBySocket(app notifyCommon.Application)
	SendNotify(name string, message map[string]interface{})
	SettingSystemTempData(message map[string]interface{})
	GetSystemTempMap() *syncmap.Map
}

// TempEntry wraps a value stored in SystemTempMap with a timestamp so stale
// entries can be evicted, preventing unbounded growth of the sync.Map.
type TempEntry struct {
	Value    interface{}
	StoredAt time.Time
}

type notifyServer struct {
	db            *gorm.DB
	SystemTempMap syncmap.Map //[string]interface{}
}

func (i *notifyServer) SettingSystemTempData(message map[string]interface{}) {
	// Evict entries older than 1 hour before storing new ones.
	now := time.Now()
	i.SystemTempMap.Range(func(key, value interface{}) bool {
		if entry, ok := value.(TempEntry); ok {
			if now.Sub(entry.StoredAt) > time.Hour {
				i.SystemTempMap.Delete(key)
			}
		}
		return true
	})

	for k, v := range message {
		i.SystemTempMap.Store(k, TempEntry{Value: v, StoredAt: now})
	}
}

func (i *notifyServer) SendNotify(name string, message map[string]interface{}) {
	msg := make(map[string]string)
	for k, v := range message {
		bt, _ := json.Marshal(v)
		msg[k] = string(bt)
	}
	// Wave 35-X: bound publish so a wedged bus cannot pin the
	// periodical utilization broadcaster (called every 2-5s from
	// periodical.go SendAllHardwareStatusBySocket).
	pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pubCancel()
	response, err := MyService.MessageBus().PublishEventWithResponse(pubCtx, common.SERVICENAME, name, msg)
	if err != nil {
		logger.Error("failed to publish event to message bus", zap.Error(err), zap.Any("event", msg))
		return
	}
	if response != nil && response.StatusCode() != http.StatusOK {
		logger.Error("failed to publish event to message bus", zap.String("status", response.Status()), zap.Any("response", response))
	}
	// SocketServer.BroadcastToRoom("/", "public", path, message)
}

// Send periodic broadcast messages
// sendFileNotifyMu serializes SendFileOperateNotify so its two entry points
// (the background poll loop and the per-cancel call) cannot both pop the
// front of OpStrArr concurrently (TOCTOU double-pop dropping an in-flight
// copy from progress and leaking its FileQueue entry).
var sendFileNotifyMu sync.Mutex

// fileOperateSnapshot builds the payload of one file-operation broadcast and
// settles the queue while doing it: finished operations are retired, stalled
// ones are cancelled for real, and operations whose worker is still unwinding
// a cancel are reported as CANCELLING instead of being retired early.
//
// This body used to be duplicated verbatim in both branches of
// SendFileOperateNotify. The cancellation rules below are the difference
// between "the operation stopped" and "the dashboard says it stopped", so they
// cannot be allowed to drift between the immediate and the periodic path.
func fileOperateSnapshot() []notify.File {
	OpMu.Lock()
	ids := make([]string, len(OpStrArr))
	copy(ids, OpStrArr)
	OpMu.Unlock()

	list := []notify.File{}
	for _, v := range ids {
		tempItem, ok := FileQueue.Load(v)
		if !ok {
			continue
		}
		temp := tempItem.(model2.FileOperate)

		task := notify.File{}
		task.Id = v
		task.ProcessedSize = temp.ProcessedSize
		task.TotalSize = temp.TotalSize
		task.To = temp.To
		task.Type = temp.Type

		// The worker is parked inside the kernel — typically a hard-mounted
		// NFS/SMB share whose server left the network. Report the fact, with the
		// path and mount that stopped answering, instead of an operation that
		// says CANCELLING for ever and a progress bar that never moves. The
		// entry deliberately stays on screen: the operation really has not
		// finished, and it is the only place the operator can see WHY.
		//
		// Checked before the cancel and completion rules so a parked worker can
		// never be reported as CANCELLING (about to stop, which it is not) or as
		// FINISHED (its byte count is frozen, not complete).
		if wedged, ok := OpWedged(v); ok {
			task.Status = "WEDGED"
			task.ProcessingPath = wedged.Holder
			if wedged.Mount != "" {
				task.ProcessingPath = wedged.Holder + " (on " + wedged.Mount + ")"
			}
			list = append(list, task)
			continue
		}

		// A cancel is already in flight and its worker has not unwound yet.
		// Re-issuing it would block this loop for a whole unwind window on
		// every tick, and retiring the entry would hand the destination to the
		// next operation while the previous one is still writing into it.
		if OpCancelPending(v) {
			task.Status = "CANCELLING"
			list = append(list, task)
			continue
		}

		// An operation that has not moved a byte in a long while gets its paths
		// probed (asynchronously, and only ever once at a time). Nothing is
		// cancelled and no verdict is reached here: if the filesystem answers,
		// the operation is just slow and keeps its slot. See considerWedge.
		if !temp.Finished && OpStalledFor(v, temp.ProcessedSize) >= OpStallProbeAfter {
			considerWedge(v)
		}

		// Staleness: no byte moved in 5 minutes. Deleting the tracking (what
		// this did before) left the worker copying into a filling disk with
		// the operation invisible to the operator, so cancel the operation's
		// context and only report ERROR once it has actually stopped.
		//
		// Note this path is a backstop today: CheckFileStatus refreshes
		// LastUpdate on every 500ms tick, so it only becomes reachable if that
		// poller dies. It is kept correct for exactly that case.
		if !temp.Finished && !temp.LastUpdate.IsZero() && time.Since(temp.LastUpdate) > 5*time.Minute {
			logger.Error("file operation stalled, cancelling", zap.String("id", v), zap.String("type", temp.Type), zap.String("to", temp.To))
			if !CancelOp(v, CancelUnwindWait) {
				task.Status = "CANCELLING"
				list = append(list, task)
				continue
			}
			task.Finished = true
			task.Status = "ERROR"
			list = append(list, task)
			continue
		}

		if task.ProcessedSize == 0 {
			task.Status = "STARTING"
		} else {
			task.Status = "PROCESSING"
		}

		if temp.Finished || temp.ProcessedSize >= temp.TotalSize {
			task.Finished = true
			task.Status = "FINISHED"
			ReapOp(v)
			go ExecOpFile()
			list = append(list, task)
			continue
		}

		for _, item := range temp.Item {
			if item.Size != item.ProcessedSize {
				task.ProcessingPath = item.From
				break
			}
		}

		list = append(list, task)
	}

	// Keep the queue moving. Every other pump is hung off some operation
	// finishing (finishOp, the FINISHED branch above) or off an enqueue that
	// found the queue empty, so a queue whose head can never be claimed — a
	// parked worker's entry stays in it by design — had nothing left to start
	// anything, not even operations queued after the wedge. This tick is the one
	// thing still running in that state, so it is where the pump belongs.
	//
	// Starting is claim-guarded and reservation-guarded (beginOp), so pumping a
	// busy or a blocked queue is a no-op; the idle check only avoids the useless
	// goroutine in the common case.
	if len(ids) > 0 && OpQueueIdle() {
		go ExecOpFile()
	}
	return list
}

func (i *notifyServer) SendFileOperateNotify(nowSend bool) {
	sendFileNotifyMu.Lock()
	defer sendFileNotifyMu.Unlock()
	if nowSend {
		queueLen := 0
		FileQueue.Range(func(k, v interface{}) bool {
			queueLen++
			return true
		})

		model := notify.NotifyModel{}
		listMsg := make(map[string]interface{})
		if queueLen == 0 {
			model.Data = []string{}
			listMsg["file_operate"] = model
			msg := make(map[string]string)
			for k, v := range listMsg {
				bt, _ := json.Marshal(v)
				msg[k] = string(bt)
			}
			// Wave 35-X: bound publish (fires per file-op tick).
			pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
			response, err := MyService.MessageBus().PublishEventWithResponse(pubCtx, common.SERVICENAME, "casaos:file:operate", msg)
			pubCancel()
			if err != nil {
				logger.Error("failed to publish event to message bus", zap.Error(err), zap.Any("event", msg))
			}
			if response != nil && response.StatusCode() != http.StatusOK {
				logger.Error("failed to publish event to message bus", zap.String("status", response.Status()), zap.Any("response", response))
			}
			return
		}

		model.State = "NORMAL"
		model.Data = fileOperateSnapshot()

		listMsg["file_operate"] = model
		msg := make(map[string]string)
		for k, v := range listMsg {
			bt, _ := json.Marshal(v)
			msg[k] = string(bt)
		}
		// Wave 35-X: bound publish.
		pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		response, err := MyService.MessageBus().PublishEventWithResponse(pubCtx, common.SERVICENAME, "casaos:file:operate", msg)
		pubCancel()
		if err != nil {
			logger.Error("failed to publish event to message bus", zap.Error(err), zap.Any("event", msg))
		}
		if response != nil && response.StatusCode() != http.StatusOK {
			logger.Error("failed to publish event to message bus", zap.String("status", response.Status()), zap.Any("response", response))
		}

	} else {
		for {

			queueLen := 0
			FileQueue.Range(func(k, v interface{}) bool {
				queueLen++
				return true
			})
			if queueLen == 0 {
				return
			}
			listMsg := make(map[string]interface{})
			model := notify.NotifyModel{}
			model.State = "NORMAL"
			model.Data = fileOperateSnapshot()

			listMsg["file_operate"] = model
			msg := make(map[string]string)
			for k, v := range listMsg {
				bt, _ := json.Marshal(v)
				msg[k] = string(bt)
			}
			// Wave 35-X: bound publish (fires per file-op tick).
			pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
			response, err := MyService.MessageBus().PublishEventWithResponse(pubCtx, common.SERVICENAME, "casaos:file:operate", msg)
			pubCancel()
			if err != nil {
				logger.Error("failed to publish event to message bus", zap.Error(err), zap.Any("event", msg))
			}
			if response != nil && response.StatusCode() != http.StatusOK {
				logger.Error("failed to publish event to message bus", zap.String("status", response.Status()), zap.Any("response", response))
			}
			time.Sleep(time.Second * 3)
		}
	}
}

// func (i *notifyServer) SendInstallAppBySocket(app notifyCommon.Application) {
// 	SocketServer.BroadcastToRoom("/", "public", "app_install", app)
// }

// func (i *notifyServer) SendUninstallAppBySocket(app notifyCommon.Application) {
// 	SocketServer.BroadcastToRoom("/", "public", "app_uninstall", app)
// }

func (i *notifyServer) SSR() {
	server := socketio.NewServer(nil)
	fmt.Println(server)
}

// Pointer receiver — value receiver copies syncmap.Map (noCopy guard).
func (i *notifyServer) GetList(c int) (list []model.AppNotify) {
	i.db.Where("class = ?", c).Where(i.db.Where("state = ?", types.NOTIFY_DYNAMICE).Or("state = ?", types.NOTIFY_UNREAD)).Find(&list)
	return
}

func (i *notifyServer) AddLog(log model.AppNotify) {
	i.db.Create(&log)
}

func (i *notifyServer) UpdateLog(log model.AppNotify) {
	i.db.Save(&log)
}

func (i *notifyServer) UpdateLogByCustomID(log model.AppNotify) {
	if len(log.CustomId) == 0 {
		return
	}
	i.db.Model(&model.AppNotify{}).Select("*").Where("custom_id = ? ", log.CustomId).Updates(log)
}

func (i *notifyServer) GetLog(id string) model.AppNotify {
	var log model.AppNotify
	i.db.Where("custom_id = ? ", id).First(&log)
	return log
}

func (i *notifyServer) MarkRead(id string, state int) {
	// Reject empty or sentinel values that would degenerate into a
	// full-table update. Mass-mark-all is now a separate handler that
	// must be called explicitly and audited.
	if id == "" || id == "0" {
		return
	}
	i.db.Model(&model.AppNotify{}).Where("id = ?", id).Update("state", state)
}

func (i *notifyServer) DelLog(id string) {
	var log model.AppNotify
	i.db.Where("custom_id = ?", id).Delete(&log)
}

func SendMeg() {
	// for {
	// 	mt, message, err := ws.ReadMessage()
	// 	if err != nil {
	// 		break
	// 	}
	// 	notify := model.NotifyMssage{}
	// 	json2.Unmarshal(message, &notify)
	// 	if notify.Type == "read" {
	// 		service.MyService.Notify().MarkRead(notify.Data, types.NOTIFY_READ)
	// 	}
	// 	if notify.Type == "app" {
	//		go func(ws *websocket.Conn) {

	for {
		list := MyService.Notify().GetList(types.NOTIFY_APP)
		json, _ := json2.Marshal(list)

		if len(list) > 0 {
			var temp []*websocket.Conn
			WebSocketMu.Lock()
			for _, v := range WebSocketConns {

				err := v.WriteMessage(1, json)
				if err == nil {
					temp = append(temp, v)
				}
			}
			WebSocketConns = temp
			WebSocketMu.Unlock()
			for _, v := range list {
				MyService.Notify().MarkRead(v.Id, types.NOTIFY_READ)
			}
		}

		WebSocketMu.Lock()
		if len(WebSocketConns) == 0 {
			SocketRun.Store(false)
			WebSocketMu.Unlock()
			return
		}
		WebSocketMu.Unlock()
		time.Sleep(time.Second * 2)
	}
	// 	}(ws)
	// }
	//	}
}

// func (i notifyServer) SendText(m model.AppNotify) {
// 	list := []model.AppNotify{}
// 	list = append(list, m)
// 	json, _ := json2.Marshal(list)
// 	var temp []*websocket.Conn
// 	for _, v := range WebSocketConns {

// 		err := v.WriteMessage(1, json)
// 		if err == nil {
// 			temp = append(temp, v)
// 		}
// 	}
// 	WebSocketConns = temp

// 	if len(WebSocketConns) == 0 {
// 		SocketRun = false
// 	}

// }
// Return a pointer so we don't copy the syncmap.Map's internal lock
// (vet's noCopy guard). All known callers were already ranging the map;
// switching to *syncmap.Map keeps the existing call sites compatible.
func (i *notifyServer) GetSystemTempMap() *syncmap.Map {
	return &i.SystemTempMap
}

func NewNotifyService(db *gorm.DB) NotifyServer {
	return &notifyServer{db: db, SystemTempMap: syncmap.Map{}}
}
