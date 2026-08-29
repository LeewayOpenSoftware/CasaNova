package service

import (
	"github.com/LeewayOpenSoftware/CasaNova-UserService/model"
	"gorm.io/gorm"
)

type EventService interface {
	CreateEvemt(m model.EventModel) model.EventModel
	GetEvents() (list []model.EventModel)
	GetEventByUUID(uuid string) (m model.EventModel)
	DeleteEvent(uuid string)
	DeleteEventBySerial(serial string)
}

type eventService struct {
	db *gorm.DB
}

func (e *eventService) CreateEvemt(m model.EventModel) model.EventModel {
	e.db.Create(&m)
	return m
}
func (e *eventService) GetEvents() (list []model.EventModel) {
	e.db.Find(&list)
	return
}
func (e *eventService) GetEventByUUID(uuid string) (m model.EventModel) {
	e.db.Where("uuid = ?", uuid).First(&m)
	return
}
func (e *eventService) DeleteEvent(uuid string) {
	e.db.Where("uuid = ?", uuid).Delete(&model.EventModel{})
}

// DeleteEventBySerial removes every local-storage event whose JSON
// "properties" field contains the given disk serial.
//
// Wave 28: previously this loaded the entire events table into memory,
// JSON-decoded each row in Go, then issued one DELETE per match. With
// thousands of rows that's an O(N) read + O(M) delete amplification —
// noticeable hitch when a disk is unplugged. Replaced with a single
// scoped DELETE that uses SQLite's json_extract() to filter on the
// serial field directly. SQLite 3.38+ ships json_extract in core, and
// Ubuntu 24.04 (the supported base) ships 3.45+, so no extension load
// is needed.
func (e *eventService) DeleteEventBySerial(serial string) {
	e.db.Where(
		"source_id = ? AND json_extract(properties, '$.serial') = ?",
		"local-storage", serial,
	).Delete(&model.EventModel{})
}
func NewEventService(db *gorm.DB) EventService {
	return &eventService{db: db}
}
