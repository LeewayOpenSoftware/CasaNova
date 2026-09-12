package common

import (
	"context"
	"sync"
)

type (
	keyTypeProperties       int
	keyTypeInterpolationMap int
)

const (
	keyProperties       keyTypeProperties       = iota
	keyInterpolationMap keyTypeInterpolationMap = iota
)

// propertiesMu guards EVERY property map stored in a context.
//
// THE BUG IT CLOSES IS A CRASH, NOT A WRONG FIELD. The map kept in the context
// is shared on purpose: handlers write into it so that later events carry what
// they wrote (service/image.go records whether the image was updated;
// route/v1/docker.go records the container id once it exists). Meanwhile
// PublishEventWrapper RANGES that same map, and almost every publish is
// launched as `go PublishEventWrapper(...)`. A range concurrent with a write is
// not a torn read in Go — the runtime aborts the process with "concurrent map
// read and map write", a fatal that recover() cannot catch. One event racing
// one image pull takes down the whole app-management daemon.
//
// WHY THE LOCK IS PER PACKAGE AND NOT PER MAP, which is the one non-obvious
// choice here: the same map OBJECT is deliberately stored in more than one
// context. service/compose_service.go hands the request context's map to
// WithProperties to seed the detached background context, so one map can be
// reachable through two contexts. A mutex bundled with the map would be created
// fresh by that second WithProperties, leaving two locks guarding one map and
// the race very much alive behind an arrangement that looks safe. A single lock
// for the whole class is the only formulation that holds given that map
// identity is shared. Contention is a non-issue: these maps carry fewer than
// ten keys and are written a handful of times per request.
var propertiesMu sync.RWMutex

// WithProperties returns a new context carrying the given properties for
// events. The map is stored as-is, so writes through SetProperty are visible to
// everything that later publishes on this context — that sharing is the
// intended mechanism, and it is what the lock above makes safe.
func WithProperties(ctx context.Context, properties map[string]string) context.Context {
	if properties == nil {
		// Storing nil would make the first SetProperty panic on a write to a
		// nil map. Reads behave identically either way.
		properties = map[string]string{}
	}

	return withMap(ctx, keyProperties, properties)
}

// PropertiesSnapshot returns a COPY of the event properties in the context.
//
// It replaces PropertiesFromContext, which handed out the stored map itself.
// The old name was deliberately REMOVED rather than redefined: the eleven call
// sites that did
//
//	properties := PropertiesFromContext(ctx)
//	properties[key] = value
//
// would still compile against a version that merely returned a copy, and their
// writes would vanish silently — keys that reach the bus today would simply
// stop arriving, with nothing to notice it. Deleting the symbol makes every one
// of those sites fail to build until it is converted to SetProperty on purpose.
func PropertiesSnapshot(ctx context.Context) map[string]string {
	propertiesMu.RLock()
	defer propertiesMu.RUnlock()

	stored := mapFromContext(ctx, keyProperties)

	properties := make(map[string]string, len(stored))
	for k, v := range stored {
		properties[k] = v
	}

	return properties
}

// SetProperty writes one event property into the map STORED IN the context, so
// every later publish on that context carries it. That write-back is the
// existing behaviour and is preserved exactly; only its atomicity is new.
func SetProperty(ctx context.Context, key string, value string) {
	SetProperties(ctx, map[string]string{key: value})
}

// SetProperties is SetProperty for several keys, applied as one atomic update.
//
// A context with no properties still yields a throwaway map here, exactly as
// assigning through the old getter did, so a caller that forgot WithProperties
// keeps losing its writes instead of panicking.
func SetProperties(ctx context.Context, properties map[string]string) {
	if len(properties) == 0 {
		return
	}

	propertiesMu.Lock()
	defer propertiesMu.Unlock()

	stored := mapFromContext(ctx, keyProperties)
	for k, v := range properties {
		stored[k] = v
	}
}

func withMap[T any](ctx context.Context, key T, value map[string]string) context.Context {
	return context.WithValue(ctx, key, value)
}

func mapFromContext[T any](ctx context.Context, key T) map[string]string {
	value := ctx.Value(key)
	if value == nil {
		return make(map[string]string)
	}

	if properties, ok := value.(map[string]string); ok {
		return properties
	}

	return make(map[string]string)
}
