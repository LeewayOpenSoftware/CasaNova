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

// WHAT AN EVENT DECLARES MUST BE WHAT IT ACTUALLY PUTS ON THE WIRE.
//
// Six events declared an EMPTY PropertyTypeList while really publishing
// properties. The mechanism is easy to miss: PropertiesFromContext returns the
// map STORED IN THE CONTEXT, not a copy, so
//
//	eventProperties := common.PropertiesFromContext(ctx)
//	eventProperties[PropertyTypeAppName.Name] = a.Name
//
// mutates the context's own map, and every later PublishEventWrapper on that
// same ctx carries the keys — including calls that pass nil properties.
//
// This test does NOT hardcode the expected key list. It replays the real
// population sequence against the real ComposeApp code and compares the result
// to the declaration, so adding a property to that path fails here until the
// declaration is updated too. Asserting merely that the list is non-empty would
// pass a declaration that is still wrong.
func TestDeclaredPropertiesMatchWhatIsActuallyPublished(t *testing.T) {
	logger.LogInitConsoleOnly()

	storeComposeApp, err := service.NewComposeAppFromYAML([]byte(common.SampleComposeAppYAML), true, false)
	assert.NilError(t, err)

	// Exactly what UpdateComposeApp and ApplyChanges do before publishing.
	ctx := common.WithProperties(context.Background(), map[string]string{})
	eventProperties := common.PropertiesFromContext(ctx)
	eventProperties[common.PropertyTypeAppName.Name] = "some-app"
	assert.NilError(t, storeComposeApp.UpdateEventPropertiesFromStoreInfo(eventProperties))

	// Read back THROUGH THE CONTEXT rather than from the local variable: that is
	// what PublishEventWrapper merges, and it is the same map only because the
	// getter returns it by reference. If that ever changes, this test should be
	// the thing that notices.
	carried := sortedKeys(common.PropertiesFromContext(ctx))

	for _, eventType := range []message_bus.EventType{
		common.EventTypeAppUpdateBegin,
		common.EventTypeAppUpdateEnd,
		common.EventTypeAppApplyChangesBegin,
		common.EventTypeAppApplyChangesEnd,
	} {
		t.Run(eventType.Name, func(t *testing.T) {
			// SUBSET, not equality. When this was written these four events
			// were reachable only down this one path, so the two sets matched.
			// app:update-end is now also published by route/v2/docker.go after
			// RecreateContainer, whose PullLatestImage writes
			// docker:image:updated into the same map, so its declaration is the
			// UNION over paths and is legitimately larger than what this path
			// carries. Exactness in the other direction — no declared key
			// without a publisher — is enforced by
			// TestEveryDeclaredKeyIsPublished.
			declared := set(sortedPropertyNames(eventType.PropertyTypeList))
			for _, k := range carried {
				_, ok := declared[k]
				assert.Assert(t, ok, "%s carries %q on this path but does not declare it", eventType.Name, k)
			}
		})
	}
}

// The other two of the six are a DIFFERENT case and must not be "fixed" by
// copying app:name into them.
//
// app-store:register-begin/end are published from RegisterAppStore /
// RegisterAppStoreSync, whose context is built in route/v2/appstore.go as
// common.WithProperties(ctx, PropertiesFromQueryParams(ctx)). Nothing on that
// path ever writes an app property, so those two carry no event-specific
// property at all and their empty declaration is already honest. The only thing
// they can carry is the cross-cutting query-param pair, which can ride ANY
// event and therefore is not a property of these two.
//
// This test pins that reasoning so nobody later "completes" these declarations
// by analogy with their neighbours.
func TestAppStoreRegisterEventsDeclareNothingBecauseTheyCarryNothing(t *testing.T) {
	for _, eventType := range []message_bus.EventType{
		common.EventTypeAppStoreRegisterBegin,
		common.EventTypeAppStoreRegisterEnd,
	} {
		t.Run(eventType.Name, func(t *testing.T) {
			if len(eventType.PropertyTypeList) != 0 {
				t.Fatalf("%s declares %v. If a property was genuinely added to this "+
					"event's publish path, update this test with the evidence; if it was "+
					"added by analogy with app:update-*, it is wrong — nothing on the "+
					"RegisterAppStore path writes an app property",
					eventType.Name, sortedPropertyNames(eventType.PropertyTypeList))
			}
		})
	}
}

// A property type declared on an event must be a real, named property type —
// an empty name would make the declaration unmatchable against a payload key.
func TestDeclaredPropertyTypesAreNamed(t *testing.T) {
	for _, eventType := range common.EventTypes {
		for _, p := range eventType.PropertyTypeList {
			if p.Name == "" {
				t.Fatalf("%s declares a property type with an empty name", eventType.Name)
			}
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedPropertyNames(list []message_bus.PropertyType) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}
