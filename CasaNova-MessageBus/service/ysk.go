package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/common"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/pkg/ysk"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/repository"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/utils"
	"go.uber.org/zap"
)

type YSKService struct {
	repository       *repository.Repository
	ws               *EventServiceWS
	eventTypeService *EventTypeService

	// B11: Start() is no longer called only once from main() — the
	// event dispatcher's restart hook calls it again to re-subscribe
	// after a dispatcher restart. startMutex serializes those callers
	// so cancelFunc is not read and written concurrently (which would
	// leak the previous goroutine, i.e. an orphan consumer holding a
	// dead channel forever).
	startMutex sync.Mutex
	cancelFunc context.CancelFunc
}

const YSKOnboardingFinishedKey = "ysk_onboarding_finished"

func NewYSKService(
	repository *repository.Repository,
	ws *EventServiceWS,
	ets *EventTypeService,
) *YSKService {
	return &YSKService{
		repository:       repository,
		ws:               ws,
		eventTypeService: ets,
	}
}

func (s *YSKService) YskCardList(ctx context.Context) ([]ysk.YSKCard, error) {
	cardList, err := (*s.repository).GetYSKCardList()
	if err != nil {
		return []ysk.YSKCard{}, err
	}
	return cardList, nil
}

func (s *YSKService) UpsertYSKCard(ctx context.Context, yskCard ysk.YSKCard) error {
	// don't store short notice cards
	if yskCard.CardType == ysk.CardTypeShortNote {
		return nil
	}
	err := (*s.repository).UpsertYSKCard(yskCard)
	return err
}

func (s *YSKService) DeleteYSKCard(ctx context.Context, id string) error {
	return (*s.repository).DeleteYSKCard(id)
}

func (s *YSKService) Start(init bool) {
	s.startMutex.Lock()
	defer s.startMutex.Unlock()

	// Cancel any previous goroutine from a prior Start() call to prevent leaks.
	if s.cancelFunc != nil {
		s.cancelFunc()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelFunc = cancel

	if init {
		// only run once
		settings, err := (*s.repository).GetSettings(YSKOnboardingFinishedKey)

		if settings == nil && err != nil && err.Error() == "record not found" {
			s.UpsertYSKCard(context.Background(), utils.ZimaOSDataStationNotice)
			s.UpsertYSKCard(context.Background(), utils.ZimaOSFileManagementNotice)
			s.UpsertYSKCard(context.Background(), utils.ZimaOSRemoteAccessNotice)
			(*s.repository).UpsertSettings(model.Settings{
				Key:   YSKOnboardingFinishedKey,
				Value: "true",
			})
		}
	}
	// register event
	s.eventTypeService.RegisterEventType(model.EventType{
		SourceID: common.SERVICENAME,
		Name:     common.EventTypeYSKCardUpsert.Name,
	})

	s.eventTypeService.RegisterEventType(model.EventType{
		SourceID: common.SERVICENAME,
		Name:     common.EventTypeYSKCardDelete.Name,
	})

	// the event is frontend event.
	// in casaos, it register by frontend. register by who call it.
	// but in zimaos ui gen 2. the frontend lose register event type.
	// so we had to register it here.
	// but i think is not a good idea. it should register by who call it.
	s.eventTypeService.RegisterEventType(model.EventType{
		SourceID: "casaos-ui",
		Name:     "casaos-ui:app:mircoapp_communicate",
	})

	channel, err := s.ws.Subscribe(common.SERVICENAME, []string{
		common.EventTypeYSKCardUpsert.Name, common.EventTypeYSKCardDelete.Name,
	})
	if err != nil {
		logger.Error("failed to subscribe to event", zap.Error(err))
		return
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				logger.Info("YSK service goroutine stopped")
				return
			case event, ok := <-channel:
				if !ok {
					logger.Info("YSK event channel closed")
					return
				}
				switch event.Name {
				case common.EventTypeYSKCardUpsert.Name:
					if event.Properties == nil {
						logger.Error("event properties are nil", zap.String("eventName", event.Name))
						continue
					}
					body, exists := event.Properties[common.PropertyTypeCardBody.Name]
					if !exists {
						logger.Error("card body property not found", zap.String("eventName", event.Name))
						continue
					}
					var card ysk.YSKCard
					err := json.Unmarshal([]byte(body), &card)
					if err != nil {
						logger.Error("failed to umarshal ysk card", zap.Error(err))
						continue
					}
					// Wave 35-X: inherit goroutine ctx so shutdown
					// cancels in-flight sqlite ops; bound to 5s so a
					// hung db driver cannot freeze the event loop.
					upCtx, upCancel := context.WithTimeout(ctx, 5*time.Second)
					err = s.UpsertYSKCard(upCtx, card)
					upCancel()
					if err != nil {
						logger.Error("failed to upsert ysk card", zap.Error(err))
					}
				case common.EventTypeYSKCardDelete.Name:
					if event.Properties == nil {
						logger.Error("event properties are nil", zap.String("eventName", event.Name))
						continue
					}
					delCtx, delCancel := context.WithTimeout(ctx, 5*time.Second)
					err = s.DeleteYSKCard(delCtx, event.Properties[common.PropertyTypeCardID.Name])
					delCancel()
					if err != nil {
						logger.Error("failed to delete ysk card", zap.Error(err))
					}
				default:
					// The bus's own keepalive rides every internal
					// subscription — routine, not "unknown". Logging it
					// spammed one info line per 5s forever.
					if event.Name == common.MessageBusHeartbeatName {
						continue
					}
					logger.Info("unknown YSK event type", zap.String("name", event.Name))
				}
			}
		}
	}()
}
