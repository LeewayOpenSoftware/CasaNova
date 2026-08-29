package service

import (
	"errors"
	"fmt"

	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/repository"
)

type ActionTypeService struct {
	repository *repository.Repository
}

var (
	ErrActionSourceIDNotFound = errors.New("event source id not found")
	ErrActionNameNotFound     = errors.New("event name not found")
)

func (s *ActionTypeService) GetActionTypes() ([]model.ActionType, error) {
	return (*s.repository).GetActionTypes()
}

func (s *ActionTypeService) RegisterActionType(actionType model.ActionType) (*model.ActionType, error) {
	if !urlSafeID.MatchString(actionType.SourceID) {
		return nil, fmt.Errorf("sourceID contains invalid characters")
	}
	if !urlSafeID.MatchString(actionType.Name) {
		return nil, fmt.Errorf("name contains invalid characters")
	}

	return (*s.repository).RegisterActionType(actionType)
}

func (s *ActionTypeService) GetActionTypesBySourceID(sourceID string) ([]model.ActionType, error) {
	return (*s.repository).GetActionTypesBySourceID(sourceID)
}

func (s *ActionTypeService) GetActionType(sourceID string, name string) (*model.ActionType, error) {
	return (*s.repository).GetActionType(sourceID, name)
}

func NewActionTypeService(repository *repository.Repository) *ActionTypeService {
	return &ActionTypeService{
		repository: repository,
	}
}
