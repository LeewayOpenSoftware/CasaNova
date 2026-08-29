package ysk_test

import (
	"context"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/pkg/ysk"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/repository"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/service"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/utils"
	"gotest.tools/assert"
)

var ws *service.EventServiceWS

func setup(t *testing.T) (*service.EventServiceWS, *service.YSKService, func()) {
	repository, err := repository.NewDatabaseRepositoryInMemory()
	assert.NilError(t, err)
	s := service.NewServices(&repository)
	wsService := s.EventServiceWS
	yskService := s.YSKService

	ctx := context.Background()
	go s.Start(&ctx)
	return wsService, yskService, func() {
		repository.Close()
	}
}
func mockPublish(ctx context.Context, sourceID string, eventName string, body map[string]string) {
	if ws != nil {
		ws.Publish(model.Event{
			SourceID:   sourceID,
			Name:       eventName,
			Properties: body,
		})
	}
}

func TestUpdateProgress(t *testing.T) {
	logger.LogInitConsoleOnly()

	wsService, yskService, cleanup := setup(t)
	defer cleanup()
	ws = wsService

	yskService.Start(false)
	// The service has no readiness signal to wait on, so this one sleep stays.
	// It is short and it is not what the test asserts against — every
	// assertion below polls, so a slow start costs time rather than a failure.
	time.Sleep(200 * time.Millisecond)

	err := ysk.NewYSKCard(context.Background(), utils.ApplicationInstallProgress.WithTaskContent(
		"jellyfin logo",
		"Installing LinuxServer/Jellyfin",
	).WithProgress(
		"Installing LinuxServer/Jellyfin", 25,
	), mockPublish)
	assert.NilError(t, err)

	err = ysk.NewYSKCard(context.Background(), utils.ApplicationInstallProgress.WithProgress(
		"Installing LinuxServer/Jellyfin", 50,
	), mockPublish)
	assert.NilError(t, err)

	// Poll instead of sleeping: see waitForCardCount.
	waitForCardCount(t, yskService, 1)

	assert.NilError(t, err)
	err = ysk.DeleteCard(context.Background(), utils.ApplicationInstallProgress.Id, mockPublish)
	assert.NilError(t, err)

	// Poll instead of sleeping: see waitForCardCount.
	waitForCardCount(t, yskService, 0)
}

func TestLongAndShortNoticeInsert(t *testing.T) {
	logger.LogInitConsoleOnly()

	wsService, yskService, cleanup := setup(t)
	defer cleanup()
	ws = wsService

	yskService.Start(false)
	// The service has no readiness signal to wait on, so this one sleep stays.
	// It is short and it is not what the test asserts against — every
	// assertion below polls, so a slow start costs time rather than a failure.
	time.Sleep(200 * time.Millisecond)

	err := ysk.NewYSKCard(context.Background(), utils.ZimaOSDataStationNotice, mockPublish)
	assert.NilError(t, err)
	err = ysk.NewYSKCard(context.Background(), utils.ApplicationUpdateNotice, mockPublish)
	assert.NilError(t, err)

	// Poll instead of sleeping: see waitForCardCount.
	waitForCardCount(t, yskService, 1)
}

// waitForCardCount polls until the service reports `want` cards, or fails.
//
// WHY THIS REPLACED time.Sleep(1 * time.Second).
//
// These tests slept a second and then asserted. A second is plenty on an idle
// machine and not always enough under the release gate, which runs
// `go test ./...` across ten modules back to back: G1 failed three times in
// five runs, twice in this package and once in
// CasaNova-Common/pkg/tokenrevocation — the two packages whose tests assert on
// the wall clock. Every one of them passes in isolation, so the failure read as
// a mystery instead of what it was.
//
// A gate that cries wolf gets ignored, and this repository has already paid for
// an untrusted gate. Polling is deterministic in the direction that matters: it
// returns the instant the condition holds — so the common path is FASTER than
// the sleep it replaces — and it only fails when the condition never holds.
//
// The bound is deliberately far longer than the old sleep. It is not a
// performance budget; it is the point past which "still not there" means a
// real defect rather than a busy machine.
func waitForCardCount(t *testing.T, svc *service.YSKService, want int) {
	t.Helper()

	const bound = 15 * time.Second
	deadline := time.Now().Add(bound)
	var last int
	for {
		cards, err := svc.YskCardList(context.Background())
		assert.NilError(t, err)
		last = len(cards)
		if last == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %v the service reports %d cards, want %d — this is a "+
				"real failure, not a slow machine: the bound is %v, far beyond the "+
				"one second this used to sleep", bound, last, want, bound)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
