package service

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/repository"
)

// urlSafeID matches the identifiers services actually use: alphanumeric with
// dash/underscore/dot AND the colon namespace separator every event/action
// name in the stack is built on ("casaos:system:utilization",
// "local-storage:disk:added", …). Colon is a legal pchar in URL path
// segments (RFC 3986), so /event/{source}/{name} routing is unaffected.
// The original class omitted ':' — every real registration 400ed, which
// stayed invisible for months because the dead unix socket 401ed first.
var urlSafeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)

type EventTypeService struct {
	repository *repository.Repository
}

var (
	ErrEventSourceIDNotFound = errors.New("event source id not found")
	ErrEventNameNotFound     = errors.New("event name not found")
)

func (s *EventTypeService) GetEventTypes() ([]model.EventType, error) {
	return (*s.repository).GetEventTypes()
}

func (s *EventTypeService) RegisterEventType(eventType model.EventType) (*model.EventType, error) {
	if !urlSafeID.MatchString(eventType.SourceID) {
		return nil, fmt.Errorf("sourceID contains invalid characters (must be alphanumeric/dash/underscore)")
	}
	if !urlSafeID.MatchString(eventType.Name) {
		return nil, fmt.Errorf("name contains invalid characters (must be alphanumeric/dash/underscore/dot)")
	}

	return (*s.repository).RegisterEventType(eventType)
}

func (s *EventTypeService) GetEventTypesBySourceID(sourceID string) ([]model.EventType, error) {
	return (*s.repository).GetEventTypesBySourceID(sourceID)
}

func (s *EventTypeService) GetEventType(sourceID string, name string) (*model.EventType, error) {
	return (*s.repository).GetEventType(sourceID, name)
}

func NewEventTypeService(repository *repository.Repository) *EventTypeService {
	return &EventTypeService{
		repository: repository,
	}
}
