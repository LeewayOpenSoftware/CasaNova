package common_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"gotest.tools/v3/assert"
)

// THE FAILURE THIS GUARDS IS A DAEMON CRASH, NOT A WRONG FIELD.
//
// Event properties live in a map kept in the context, and that map is shared on
// purpose: a handler writes into it so that everything published later on the
// same context carries what it wrote. PublishEventWrapper RANGES that map, and
// nearly every publish is launched as `go PublishEventWrapper(...)`. A range
// concurrent with a write is not a torn read in Go — the runtime aborts the
// process with "concurrent map read and map write", which recover() cannot
// catch. One event racing one image pull takes the whole daemon down.
//
// This test drives the exact production pair:
//
//	reader  service/service.go   for k, v := range PropertiesSnapshot(ctx)
//	writer  service/image.go     SetProperty(ctx, ImageUpdated, ...)  (in a defer)
//
// Run under -race it is the reproduction: against the previous API, where the
// getter handed out the live map and handlers assigned into it, the detector
// reports mapassign_faststr against mapIterNext on the same address. It must
// stay in the -race suite to keep meaning anything.
func TestSnapshotAndWriteAreSafeConcurrently(t *testing.T) {
	const goroutines = 8
	const iterations = 500

	ctx := common.WithProperties(context.Background(), map[string]string{
		common.PropertyTypeAppName.Name: "some-app",
	})

	var wg sync.WaitGroup

	// Readers: what every `go PublishEventWrapper(...)` does.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				properties := map[string]string{}
				for k, v := range common.PropertiesSnapshot(ctx) {
					properties[k] = v
				}
				// A publish always carries the key the handler set first.
				if properties[common.PropertyTypeAppName.Name] != "some-app" {
					t.Errorf("snapshot lost app:name: %v", properties)
					return
				}
			}
		}()
	}

	// Writers: what the handlers and service/image.go's defer do.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				common.SetProperty(ctx, common.PropertyTypeImageUpdated.Name, "true")
				common.SetProperties(ctx, map[string]string{
					common.PropertyTypeContainerID.Name: fmt.Sprintf("container-%d", id),
				})
			}
		}(i)
	}

	wg.Wait()

	final := common.PropertiesSnapshot(ctx)
	assert.Equal(t, final[common.PropertyTypeAppName.Name], "some-app")
	assert.Equal(t, final[common.PropertyTypeImageUpdated.Name], "true")
}

// THE WRITE-BACK IS THE WHOLE POINT AND MUST SURVIVE THE FIX.
//
// service/image.go writes docker:image:updated in a DEFER precisely so that
// later publishes on the same context carry it, and route/v1/docker.go does the
// same with the container id. PublishEventWrapper reads through a DERIVED
// context (it wraps the caller's ctx in a timeout), so the value has to be
// visible through a child too.
//
// This is the test that fails if anyone "fixes" the race by making the getter
// return a copy and letting handlers write into that copy: the keys would
// quietly stop reaching the bus, with nothing else to notice.
func TestWritesAreVisibleThroughDerivedContexts(t *testing.T) {
	ctx := common.WithProperties(context.Background(), map[string]string{})

	common.SetProperty(ctx, common.PropertyTypeAppName.Name, "some-app")

	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Written after the child exists, exactly as the image-pull defer does.
	common.SetProperty(ctx, common.PropertyTypeImageUpdated.Name, "true")

	assert.DeepEqual(t, common.PropertiesSnapshot(timeoutCtx), map[string]string{
		common.PropertyTypeAppName.Name:      "some-app",
		common.PropertyTypeImageUpdated.Name: "true",
	})
}

// A snapshot is a copy: scribbling on what a publish received must not reach
// the context, or the fix would have moved the shared mutable state one step
// away rather than removed it.
func TestSnapshotIsACopy(t *testing.T) {
	ctx := common.WithProperties(context.Background(), map[string]string{
		common.PropertyTypeAppName.Name: "some-app",
	})

	snapshot := common.PropertiesSnapshot(ctx)
	snapshot[common.PropertyTypeMessage.Name] = "scribbled"
	delete(snapshot, common.PropertyTypeAppName.Name)

	assert.DeepEqual(t, common.PropertiesSnapshot(ctx), map[string]string{
		common.PropertyTypeAppName.Name: "some-app",
	})
}

// ONE MAP, TWO CONTEXTS — THE CASE THAT DECIDED THE DESIGN.
//
// service/compose_service.go seeds a detached background context from the
// request context's properties, so a single map can be reachable through two
// contexts. A mutex bundled with the map would have been duplicated by the
// second WithProperties, leaving two locks over one map and the race alive
// behind something that looks synchronised. The lock is per package for this
// reason, and this test pins the shape so the reasoning is not lost.
func TestOneMapSharedByTwoContextsStaysSafe(t *testing.T) {
	shared := map[string]string{common.PropertyTypeAppName.Name: "some-app"}

	requestCtx := common.WithProperties(context.Background(), shared)
	backgroundCtx := common.WithProperties(context.Background(), shared)

	var wg sync.WaitGroup
	for _, ctx := range []context.Context{requestCtx, backgroundCtx} {
		wg.Add(2)
		go func(ctx context.Context) {
			defer wg.Done()
			for n := 0; n < 500; n++ {
				common.SetProperty(ctx, common.PropertyTypeContainerID.Name, "855084f7")
			}
		}(ctx)
		go func(ctx context.Context) {
			defer wg.Done()
			for n := 0; n < 500; n++ {
				_ = common.PropertiesSnapshot(ctx)
			}
		}(ctx)
	}
	wg.Wait()

	// Still one map: a write through either context is visible from the other.
	assert.Equal(t, common.PropertiesSnapshot(backgroundCtx)[common.PropertyTypeContainerID.Name], "855084f7")
}

// A context that never went through WithProperties must not panic on a write —
// assigning through the old getter silently dropped it, and that is preserved.
func TestWriteWithoutPropertiesDoesNotPanic(t *testing.T) {
	common.SetProperty(context.Background(), common.PropertyTypeAppName.Name, "some-app")
	assert.Equal(t, len(common.PropertiesSnapshot(context.Background())), 0)
}

// WithProperties(ctx, nil) used to store a nil map, so the first write panicked.
func TestNilPropertiesIsWritable(t *testing.T) {
	ctx := common.WithProperties(context.Background(), nil)
	common.SetProperty(ctx, common.PropertyTypeAppName.Name, "some-app")
	assert.Equal(t, common.PropertiesSnapshot(ctx)[common.PropertyTypeAppName.Name], "some-app")
}
