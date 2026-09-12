package service_test

import (
	"context"
	"sort"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen/message_bus"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"gotest.tools/v3/assert"
)

// THE DECLARATION MUST MATCH THE WIRE, IN BOTH DIRECTIONS.
//
// An event's PropertyTypeList is a promise about what subscribers will see. Two
// ways to break it, and this file tests for both:
//
//   - UNDER-declaring: the event carries a key it never named. Thirty-one of
//     the forty-seven events did, for one reason — PropertiesFromContext hands
//     back the context's OWN map, handlers mutate it in place, and
//     PublishEventWrapper merges it OVER every later publish on that context,
//     including the many that pass nil. A key written in a route handler
//     therefore reaches events published deep inside the service layer.
//   - OVER-declaring: the event names a key nothing ever publishes. That is the
//     same lie pointing the other way, and docker:image:pull-progress is a live
//     example (declared, zero publishers anywhere in the tree).
//
// HOW THIS TEST AVOIDS BEING A COPY OF THE DECLARATION. It does not list the
// expected keys per event. It replays each publish PATH as an ordered sequence
// of the real steps — the same mutations the handler performs, against the real
// ComposeApp and the real UpdateEventPropertiesFromStoreInfo, on a real context
// built by common.WithProperties — and derives what each event carries from the
// state of that map AT THAT POINT IN THE SEQUENCE. Order is part of the answer:
// app:install-end is a DEFERRED publish and so sees a docker:container:id that
// app:install-begin, published earlier on the same context, cannot.
//
// Add a property to a path and the events downstream of it fail here until the
// declaration is updated. Delete a publisher and the completeness test notices
// that a declared key is no longer produced. Asserting "the list is non-empty",
// or comparing against a hand-written key list, would catch neither.

type publishSite struct {
	event    message_bus.EventType
	explicit []string // properties passed at the call site; nil means nil map
}

// A step is either a mutation the production code performs on the context's
// property map, or a publish. Exactly one field is set.
type step struct {
	what    string
	mutate  func(t *testing.T, props map[string]string)
	publish *publishSite
}

func writes(what string, kv map[string]string) step {
	return step{what: what, mutate: func(_ *testing.T, props map[string]string) {
		for k, v := range kv {
			props[k] = v
		}
	}}
}

func publishes(event message_bus.EventType, explicit ...string) step {
	return step{what: "publish " + event.Name, publish: &publishSite{event: event, explicit: explicit}}
}

type publishPath struct {
	name  string
	where string
	steps []step
}

func paths(t *testing.T) []publishPath {
	t.Helper()

	// The real thing, so that app:icon and app:title come from the real
	// UpdateEventPropertiesFromStoreInfo rather than from this test's opinion
	// of what that function writes.
	composeApp, err := service.NewComposeAppFromYAML([]byte(common.SampleComposeAppYAML), true, false)
	assert.NilError(t, err)

	fromStoreInfo := step{what: "UpdateEventPropertiesFromStoreInfo", mutate: func(t *testing.T, props map[string]string) {
		assert.NilError(t, composeApp.UpdateEventPropertiesFromStoreInfo(props))
	}}

	appName := func() step {
		return writes("app name", map[string]string{common.PropertyTypeAppName.Name: "some-app"})
	}
	msg := common.PropertyTypeMessage.Name

	return []publishPath{
		{
			// route/v1/docker.go AppInstall -> pullAndInstall. One context, one
			// mutation site, and every event below inherits it.
			name:  "v1 install",
			where: "route/v1/docker.go:268",
			steps: []step{
				writes("app name, icon and image name", map[string]string{
					common.PropertyTypeAppName.Name:   "some-app",
					common.PropertyTypeAppIcon.Name:   "https://example.com/icon.png",
					common.PropertyTypeImageName.Name: "hello-world:latest",
				}),
				publishes(common.EventTypeAppInstallBegin),
				publishes(common.EventTypeImagePullBegin),
				// service.PullImage names the image at its own call site too.
				publishes(common.EventTypeImagePullBegin, common.PropertyTypeImageName.Name),
				publishes(common.EventTypeAppInstallProgress, common.PropertyTypeAppProgress.Name),
				publishes(common.EventTypeImagePullEnd, common.PropertyTypeImageName.Name),
				publishes(common.EventTypeImagePullEnd),
				publishes(common.EventTypeImagePullError, msg),
				publishes(common.EventTypeContainerCreateBegin),
				// Creation failing returns BEFORE the id is written, so
				// create-error never sees one — the deferred create-end on that
				// same branch does not either.
				publishes(common.EventTypeContainerCreateError, msg),
				// route/v1/docker.go:854 — on success the id lands in the
				// SHARED map, so everything published from here on carries it.
				writes("container id", map[string]string{common.PropertyTypeContainerID.Name: "855084f7"}),
				publishes(common.EventTypeContainerCreateEnd),
				publishes(common.EventTypeContainerStartBegin),
				publishes(common.EventTypeContainerStartEnd),
				publishes(common.EventTypeContainerStartError, msg),
				// Deferred in the outer goroutine: runs last, sees the id.
				publishes(common.EventTypeAppInstallError, msg),
				publishes(common.EventTypeAppInstallEnd),
			},
		},
		{
			name:  "v1 uninstall",
			where: "route/v1/docker.go:336",
			steps: []step{
				writes("app name and icon", map[string]string{
					common.PropertyTypeAppName.Name: "some-app",
					common.PropertyTypeAppIcon.Name: "https://example.com/icon.png",
				}),
				publishes(common.EventTypeAppUninstallBegin),
				publishes(common.EventTypeContainerStopBegin),
				publishes(common.EventTypeContainerStopEnd),
				publishes(common.EventTypeContainerStopError, msg),
				publishes(common.EventTypeContainerRemoveBegin),
				publishes(common.EventTypeContainerRemoveEnd),
				publishes(common.EventTypeContainerRemoveError, msg),
				publishes(common.EventTypeImageRemoveBegin),
				publishes(common.EventTypeImageRemoveEnd),
				publishes(common.EventTypeImageRemoveError, msg),
				publishes(common.EventTypeAppUninstallError, msg),
				publishes(common.EventTypeAppUninstallEnd),
			},
		},
		{
			// route/v2/docker.go RecreateContainer. PullLatestImage writes
			// docker:image:updated into the same map in a DEFER, so it is
			// present for everything RecreateContainer publishes afterwards
			// and for the deferred app:update-end.
			name:  "v2 container recreate",
			where: "route/v2/docker.go:45",
			steps: []step{
				writes("app name and icon", map[string]string{
					common.PropertyTypeAppName.Name: "some-app",
					common.PropertyTypeAppIcon.Name: "https://example.com/icon.png",
				}),
				publishes(common.EventTypeAppUpdateBegin),
				publishes(common.EventTypeImagePullBegin, common.PropertyTypeImageName.Name),
				writes("image updated (service/image.go:89, in a defer)", map[string]string{
					common.PropertyTypeImageUpdated.Name: "true",
				}),
				publishes(common.EventTypeImagePullEnd, common.PropertyTypeImageName.Name),
				publishes(common.EventTypeImagePullError, common.PropertyTypeImageName.Name, msg),
				publishes(common.EventTypeContainerCreateBegin, common.PropertyTypeContainerName.Name),
				publishes(common.EventTypeContainerCreateEnd, common.PropertyTypeContainerID.Name, common.PropertyTypeContainerName.Name),
				publishes(common.EventTypeContainerCreateError, common.PropertyTypeContainerName.Name, msg),
				publishes(common.EventTypeContainerStopBegin, common.PropertyTypeContainerID.Name),
				publishes(common.EventTypeContainerStopEnd, common.PropertyTypeContainerID.Name),
				publishes(common.EventTypeContainerStopError, common.PropertyTypeContainerID.Name, msg),
				publishes(common.EventTypeContainerStartBegin, common.PropertyTypeContainerID.Name),
				publishes(common.EventTypeContainerStartEnd, common.PropertyTypeContainerID.Name),
				publishes(common.EventTypeContainerStartError, common.PropertyTypeContainerID.Name, msg),
				publishes(common.EventTypeContainerRemoveBegin, common.PropertyTypeContainerID.Name),
				publishes(common.EventTypeContainerRemoveEnd, common.PropertyTypeContainerID.Name),
				publishes(common.EventTypeContainerRemoveError, common.PropertyTypeContainerID.Name, msg),
				publishes(common.EventTypeContainerRenameBegin, common.PropertyTypeContainerID.Name, common.PropertyTypeContainerName.Name),
				publishes(common.EventTypeContainerRenameEnd, common.PropertyTypeContainerID.Name, common.PropertyTypeContainerName.Name),
				publishes(common.EventTypeContainerRenameError, common.PropertyTypeContainerID.Name, common.PropertyTypeContainerName.Name, msg),
				publishes(common.EventTypeAppUpdateError, msg),
				publishes(common.EventTypeAppUpdateEnd),
			},
		},
		{
			name:  "v2 image pull",
			where: "route/v2/image.go:109",
			steps: []step{
				writes("app name and icon", map[string]string{
					common.PropertyTypeAppName.Name: "some-app",
					common.PropertyTypeAppIcon.Name: "https://example.com/icon.png",
				}),
				publishes(common.EventTypeImagePullBegin, common.PropertyTypeImageName.Name),
				publishes(common.EventTypeAppInstallProgress, common.PropertyTypeAppProgress.Name),
				writes("image updated", map[string]string{common.PropertyTypeImageUpdated.Name: "false"}),
				publishes(common.EventTypeImagePullEnd, common.PropertyTypeImageName.Name, common.PropertyTypeImageUpdated.Name),
				publishes(common.EventTypeImagePullError, common.PropertyTypeImageName.Name, msg),
			},
		},
		{
			name:  "compose install",
			where: "service/compose_service.go:322",
			steps: []step{
				appName(), fromStoreInfo,
				publishes(common.EventTypeAppInstallBegin),
				publishes(common.EventTypeImagePullBegin, common.PropertyTypeImageName.Name),
				publishes(common.EventTypeAppInstallProgress, common.PropertyTypeAppProgress.Name),
				publishes(common.EventTypeImagePullEnd, common.PropertyTypeImageName.Name),
				publishes(common.EventTypeImagePullError, common.PropertyTypeImageName.Name, msg),
				publishes(common.EventTypeContainerCreateBegin),
				publishes(common.EventTypeContainerCreateEnd),
				publishes(common.EventTypeContainerCreateError, msg),
				publishes(common.EventTypeContainerStartBegin),
				publishes(common.EventTypeContainerStartEnd),
				publishes(common.EventTypeContainerStartError, msg),
				publishes(common.EventTypeAppInstallError, msg),
				publishes(common.EventTypeAppInstallEnd),
			},
		},
		{
			name:  "compose uninstall",
			where: "service/compose_service.go:403",
			steps: []step{
				appName(), fromStoreInfo,
				publishes(common.EventTypeAppUninstallBegin),
				publishes(common.EventTypeContainerStopBegin),
				publishes(common.EventTypeContainerStopEnd),
				publishes(common.EventTypeContainerStopError, msg),
				publishes(common.EventTypeContainerRemoveBegin),
				publishes(common.EventTypeContainerRemoveEnd),
				publishes(common.EventTypeImageRemoveError, msg),
				publishes(common.EventTypeAppUninstallError, msg),
				publishes(common.EventTypeAppUninstallEnd),
			},
		},
		{
			name:  "compose update",
			where: "service/compose_app.go:309",
			steps: []step{
				appName(), fromStoreInfo,
				publishes(common.EventTypeAppUpdateBegin),
				publishes(common.EventTypeImagePullBegin, common.PropertyTypeImageName.Name),
				publishes(common.EventTypeImagePullEnd, common.PropertyTypeImageName.Name),
				publishes(common.EventTypeImagePullError, common.PropertyTypeImageName.Name, msg),
				publishes(common.EventTypeAppUpdateError, msg),
				publishes(common.EventTypeAppUpdateEnd),
			},
		},
		{
			name:  "compose apply changes",
			where: "service/compose_app.go:1148",
			steps: []step{
				appName(), fromStoreInfo,
				publishes(common.EventTypeAppApplyChangesBegin),
				publishes(common.EventTypeContainerCreateBegin),
				publishes(common.EventTypeContainerCreateEnd),
				publishes(common.EventTypeContainerCreateError, msg),
				publishes(common.EventTypeContainerStartBegin),
				publishes(common.EventTypeContainerStartEnd),
				publishes(common.EventTypeContainerStartError, msg),
				publishes(common.EventTypeAppApplyChangesError, msg),
				publishes(common.EventTypeAppApplyChangesEnd),
			},
		},
		{
			// SetStatus writes app:name and nothing else — no store info. Its
			// six events are honest today and MUST NOT gain app:icon/app:title
			// by analogy with their neighbours.
			name:  "compose set status",
			where: "service/compose_app.go:1347",
			steps: []step{
				appName(),
				publishes(common.EventTypeAppStartBegin),
				publishes(common.EventTypeAppStartEnd),
				publishes(common.EventTypeAppStartError, msg),
				publishes(common.EventTypeAppStopBegin),
				publishes(common.EventTypeAppStopEnd),
				publishes(common.EventTypeAppStopError, msg),
				publishes(common.EventTypeAppRestartBegin),
				publishes(common.EventTypeAppRestartEnd),
				publishes(common.EventTypeAppRestartError, msg),
			},
		},
		{
			// Nothing on this path writes an app property: the empty
			// declarations of begin/end are already honest.
			name:  "app store register",
			where: "service/appstore_management.go:155",
			steps: []step{
				publishes(common.EventTypeAppStoreRegisterBegin),
				publishes(common.EventTypeAppStoreRegisterEnd),
				publishes(common.EventTypeAppStoreRegisterError, msg),
			},
		},
		{
			// A background sweep with no context properties at all, publishing
			// a container event with a name and a message the container-stop
			// declaration never mentioned.
			name:  "status sync",
			where: "service/status_sync.go:93",
			steps: []step{
				publishes(common.EventTypeContainerStopEnd,
					common.PropertyTypeContainerID.Name,
					common.PropertyTypeContainerName.Name,
					msg),
			},
		},
	}
}

// No event may put a key on the wire that its declaration does not name.
func TestEveryPublishedKeyIsDeclared(t *testing.T) {
	logger.LogInitConsoleOnly()

	for _, p := range paths(t) {
		t.Run(p.name, func(t *testing.T) {
			ctx := common.WithProperties(context.Background(), map[string]string{})

			for _, s := range p.steps {
				if s.mutate != nil {
					// Mutate THROUGH the getter, exactly as production does.
					s.mutate(t, common.PropertiesFromContext(ctx))
					continue
				}

				carried := carriedBy(ctx, s.publish)
				declared := set(sortedPropertyNames(s.publish.event.PropertyTypeList))

				for _, k := range carried {
					if _, ok := declared[k]; !ok {
						t.Errorf("%s publishes %q but does not declare it.\n"+
							"path %s (%s), after step %q.\n"+
							"carried: %v\ndeclared: %v",
							s.publish.event.Name, k, p.name, p.where, s.what,
							carried, sortedPropertyNames(s.publish.event.PropertyTypeList))
					}
				}
			}
		})
	}
}

// ...and no event may declare a key that no path produces. Over-declaring is
// the same lie in the other direction: it tells a subscriber to expect a field
// that will never arrive, and it is what makes a future per-event filter unsafe
// to trust.
func TestEveryDeclaredKeyIsPublished(t *testing.T) {
	logger.LogInitConsoleOnly()

	produced := map[string]map[string]bool{}
	for _, p := range paths(t) {
		ctx := common.WithProperties(context.Background(), map[string]string{})
		for _, s := range p.steps {
			if s.mutate != nil {
				s.mutate(t, common.PropertiesFromContext(ctx))
				continue
			}
			name := s.publish.event.Name
			if produced[name] == nil {
				produced[name] = map[string]bool{}
			}
			for _, k := range carriedBy(ctx, s.publish) {
				produced[name][k] = true
			}
		}
	}

	// docker:image:pull-progress is DECLARED AND NEVER PUBLISHED — no call site
	// exists anywhere in the tree. It is reported as its own defect rather than
	// silently deleted here: removing an event from the advertised set is a
	// behaviour change, not a declaration fix. Listed explicitly so that the
	// exemption is visible instead of being an absent test.
	neverPublished := map[string]bool{
		common.EventTypeImagePullProgress.Name: true,
	}

	// check_port_conflict and dry_run are not produced by any path modelled
	// here, and that is correct: they are not service-published at all. The
	// route inlet copies them from the CLIENT's query string onto whatever
	// context the request builds (route/v2.PropertiesFromQueryParams), so they
	// can ride any event on any route and are a property of the inlet rather
	// than of app:install-progress, the one event that happens to declare them.
	// Modelling them here would push them onto all forty-seven declarations,
	// which is a separate decision about the inlet and not a declaration fix.
	inletSupplied := map[string]bool{
		common.PropertyTypeCheckPortConflict.Name: true,
		common.PropertyTypeDryRun.Name:            true,
	}

	for _, eventType := range common.EventTypes {
		if neverPublished[eventType.Name] {
			assert.Assert(t, produced[eventType.Name] == nil,
				"%s is listed as having no publisher, but a path publishes it — "+
					"remove it from neverPublished", eventType.Name)
			continue
		}

		t.Run(eventType.Name, func(t *testing.T) {
			got := produced[eventType.Name]
			assert.Assert(t, got != nil, "%s is declared but no modelled path publishes it", eventType.Name)

			for _, declared := range eventType.PropertyTypeList {
				if inletSupplied[declared.Name] {
					continue
				}
				if !got[declared.Name] {
					t.Errorf("%s declares %q but no path puts it on the wire. "+
						"Either a publisher was removed (then remove the declaration) "+
						"or a path is missing from this test (then add it)",
						eventType.Name, declared.Name)
				}
			}
		})
	}
}

// carriedBy reproduces PublishEventWrapper's merge: the call site's own map
// first, then the context's map OVER it. Both the union and the direction
// matter — the context wins, which is why a client- or handler-supplied key
// does not merely add a field but can replace one the publisher just set.
func carriedBy(ctx context.Context, site *publishSite) []string {
	keys := map[string]bool{}
	for _, k := range site.explicit {
		keys[k] = true
	}
	for k := range common.PropertiesFromContext(ctx) {
		keys[k] = true
	}

	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func set(keys []string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}
