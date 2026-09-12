package common

import (
	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/codegen/message_bus"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
)

// common properties
var (
	PropertyTypeMessage = message_bus.PropertyType{
		Name:        "message",
		Description: utils.Ptr("message at different levels, typically for error"),
	}
)

// app properties
var (
	PropertyTypeAppName = message_bus.PropertyType{
		Name:        "app:name",
		Description: utils.Ptr("name of the app which could be a container image name including version, a snap name or the name of any other forms of app"),
		Example:     utils.Ptr("hello-world:latest (this is the name of a container image"),
	}

	PropertyTypeAppTitle = message_bus.PropertyType{
		Name:        "app:title",
		Description: utils.Ptr("titles of the app in different languages - serialized as JSON"),
		Example:     utils.Ptr("{\"en_us\":\"OpenSpeedTest\"}"),
	}

	PropertyTypeAppIcon = message_bus.PropertyType{
		Name:        "app:icon",
		Description: utils.Ptr("icon of the app"),
		Example:     utils.Ptr("https://example.com/icon.png"),
	}

	PropertyTypeAppProgress = message_bus.PropertyType{
		Name:        "app:progress",
		Description: utils.Ptr("progress of the app"),
		Example:     utils.Ptr("64"),
	}

	PropertyTypeCheckPortConflict = message_bus.PropertyType{
		Name:        "check_port_conflict",
		Description: utils.Ptr("todo: the field should be remove in future. it is not need for most message"),
		Example:     utils.Ptr("true"),
	}

	PropertyTypeDryRun = message_bus.PropertyType{
		Name:        "dry_run",
		Description: utils.Ptr("todo: the field should be remove in future. it is not need for most message"),
		Example:     utils.Ptr("false"),
	}
)

// container properties
var (
	PropertyTypeContainerID = message_bus.PropertyType{
		Name:        "docker:container:id",
		Description: utils.Ptr("ID of the container"),
		Example:     utils.Ptr("855084f79fc89bea4de5111c69621b3329ecf0a1106863a7a83bbdef01d33b9e"),
	}

	PropertyTypeContainerName = message_bus.PropertyType{
		Name:        "docker:container:name",
		Description: utils.Ptr("name of the container"),
		Example:     utils.Ptr("hello-world"),
	}
)

// image properties
var (
	PropertyTypeImageName = message_bus.PropertyType{
		Name:        "docker:image:name",
		Description: utils.Ptr("name of the image"),
		Example:     utils.Ptr("hello-world:latest"),
	}

	PropertyTypeImageUpdated = message_bus.PropertyType{
		Name:        "docker:image:updated",
		Description: utils.Ptr("true if image is updated"),
	}
)

// QueryParamEventProperties is the DECLARED set of query parameters that may
// become properties of a bus event. Nothing outside this list enters the
// payload — see route/v2.PropertiesFromQueryParams.
//
// WHY THIS LIST IS SHORT, AND WHY IT IS NOT "EVERY DECLARED PROPERTY TYPE".
// The handlers used to copy EVERY incoming query parameter into the event
// payload, which made any HTTP client the author of arbitrary fields on an
// internal event. Two details make that sharper than it looks:
//
//   - PublishEventWrapper merges the context properties OVER the publisher's
//     own map (service/service.go), so a client-supplied key does not merely
//     get added to the event — it OVERWRITES what the service set. A request
//     carrying ?message=... rewrote the error text of an error event, and
//     ?app:icon=... would have pointed subscribers at an attacker's URL.
//   - the message bus does not filter either: its PublishEvent takes the
//     properties verbatim and relays them to every subscriber.
//
// So app:name, app:icon, app:title and message are deliberately ABSENT here
// even though they are declared property types: they are SERVICE-OWNED, set
// from the app's own record, and must never be author-able by a caller. Only
// these two are genuinely supplied by the client and reported onward — which is
// also why both carry a "should be removed in future" note above: they are the
// legacy passengers, not a general-purpose channel.
//
// Adding an entry here is a security decision, not a refactor.
var QueryParamEventProperties = []message_bus.PropertyType{
	PropertyTypeCheckPortConflict,
	PropertyTypeDryRun,
}

var EventTypes = []message_bus.EventType{
	// app-store
	EventTypeAppStoreRegisterBegin, EventTypeAppStoreRegisterEnd, EventTypeAppStoreRegisterError,

	// app
	EventTypeAppInstallBegin, EventTypeAppInstallProgress, EventTypeAppInstallEnd, EventTypeAppInstallError,
	EventTypeAppUninstallBegin, EventTypeAppUninstallEnd, EventTypeAppUninstallError,
	EventTypeAppUpdateBegin, EventTypeAppUpdateEnd, EventTypeAppUpdateError,
	EventTypeAppApplyChangesBegin, EventTypeAppApplyChangesEnd, EventTypeAppApplyChangesError,
	EventTypeAppStartBegin, EventTypeAppStartEnd, EventTypeAppStartError,
	EventTypeAppStopBegin, EventTypeAppStopEnd, EventTypeAppStopError,
	EventTypeAppRestartBegin, EventTypeAppRestartEnd, EventTypeAppRestartError,

	// image
	EventTypeImagePullBegin, EventTypeImagePullProgress, EventTypeImagePullEnd, EventTypeImagePullError,

	// container
	EventTypeContainerCreateBegin, EventTypeContainerCreateEnd, EventTypeContainerCreateError,
	EventTypeContainerStartBegin, EventTypeContainerStartEnd, EventTypeContainerStartError,
	EventTypeContainerStopBegin, EventTypeContainerStopEnd, EventTypeContainerStopError,
	EventTypeContainerRenameBegin, EventTypeContainerRenameEnd, EventTypeContainerRenameError,
	EventTypeContainerRemoveBegin, EventTypeContainerRemoveEnd, EventTypeContainerRemoveError,
}

// WHY SO MANY EVENTS DECLARE PROPERTIES THEY NEVER NAME AT THE PUBLISH SITE.
//
// Thirty-one of these events used to declare LESS than they put on the wire.
// That is not thirty-one oversights, it is ONE mechanism seen from thirty-one
// places: PropertiesFromContext returns the map STORED IN THE CONTEXT, not a
// copy, so a handler that does
//
//	eventProperties := common.PropertiesFromContext(ctx)
//	eventProperties[PropertyTypeAppName.Name] = app.Name
//
// mutates the context's own map, and PublishEventWrapper then merges that map
// OVER every later publish on the same ctx — including the many call sites that
// pass nil properties. A key therefore travels to every event published
// downstream of wherever it was written, however far away that is.
//
// The declarations below are the UNION over the paths that actually reach each
// event. The paths, and the keys each one puts into the context map, are:
//
//	route/v1/docker.go AppInstall    app:name, app:icon, docker:image:name,
//	                                 and docker:container:id once the container
//	                                 exists — which is why app:install-end, the
//	                                 DEFERRED publish, carries an id that
//	                                 app:install-begin cannot
//	route/v1/docker.go AppUninstall  app:name, app:icon
//	route/v2/docker.go RecreateCtr   app:name, app:icon
//	route/v2/image.go PullImages     app:name, app:icon
//	service/compose_service.go       app:name + app:icon + app:title, the last
//	  Install / Uninstall            two via UpdateEventPropertiesFromStoreInfo
//	service/compose_app.go           app:name (+ store info on Update/Apply)
//	service/image.go PullLatestImage docker:image:updated, written in a DEFER so
//	                                 that everything published after the pull —
//	                                 app:update-end and every container event
//	                                 inside RecreateContainer — carries it
//
// DECLARING THE TRUTH IS NOT A BEHAVIOUR CHANGE; REMOVING THE PROPERTIES WOULD
// BE. On a feature-frozen line a subscriber may already read any of these, so
// the fix is to make the map match the territory, never to shrink the
// territory. Per-event filtering is still deliberately NOT built on top of
// this: a filter enforcing a declaration that lies drops real data silently,
// which is worse than no filter at all. The declarations tell the truth first.
//
// TWO THINGS ARE LEFT ALONE ON PURPOSE, AND BOTH ARE REPORTED RATHER THAN
// PATCHED:
//
//  1. app-store:register-begin/end declare nothing and publish nothing — their
//     context is built in route/v2/appstore.go and no code on that path writes
//     an app property. An empty declaration that is already honest must not be
//     "completed" by analogy with its neighbours.
//  2. docker:image:pull-progress declares app:name and message and has NO
//     publisher anywhere in the tree. That is the mirror image of the bug fixed
//     here — a map claiming territory that does not exist — and deleting it
//     would change the advertised event set, which is not a declaration fix.
//
// check_port_conflict and dry_run are also absent from most lists although
// PropertiesFromQueryParams admits them on every route that builds a context
// that way. They are CLIENT-supplied and cross-cutting: they can ride any event
// and so are a property of the inlet, not of any one event. They stay declared
// only on app:install-progress, where they were declared before this change.
// event types for app-store
var (
	EventTypeAppStoreRegisterBegin = message_bus.EventType{
		SourceID:         AppManagementServiceName,
		Name:             "app-store:register-begin",
		PropertyTypeList: []message_bus.PropertyType{},
	}

	EventTypeAppStoreRegisterEnd = message_bus.EventType{
		SourceID:         AppManagementServiceName,
		Name:             "app-store:register-end",
		PropertyTypeList: []message_bus.PropertyType{},
	}

	EventTypeAppStoreRegisterError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app-store:register-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeMessage,
		},
	}
)

// event types for app
var (
	EventTypeAppInstallBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:install-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
		},
	}

	EventTypeAppInstallProgress = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:install-progress",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppProgress,
			PropertyTypeAppTitle,
			PropertyTypeCheckPortConflict,
			PropertyTypeDryRun,
			PropertyTypeImageName,
		},
	}

	EventTypeAppInstallEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:install-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
			PropertyTypeContainerID,
		},
	}

	EventTypeAppInstallError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:install-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeMessage,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
			PropertyTypeContainerID,
		},
	}

	EventTypeAppUninstallBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:uninstall-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
		},
	}

	EventTypeAppUninstallEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:uninstall-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
		},
	}

	EventTypeAppUninstallError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:uninstall-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
		},
	}

	// Declared because these ARE published. The handler writes app:name onto the
	// context map and UpdateEventPropertiesFromStoreInfo adds app:icon and
	// app:title; PropertiesFromContext hands back THAT MAP BY REFERENCE, so
	// mutating it changes what every later publish on the same context carries.
	// The list said {} while the wire carried three properties.
	//
	// Declaring the reality is not a behaviour change; REMOVING the properties
	// would be, and on a frozen line could break a subscriber already reading
	// them. Per-event filtering is deliberately NOT built on top of this yet: a
	// filter that silently drops against a declaration that lies is worse than
	// no filter, so the declaration tells the truth first.
	EventTypeAppUpdateBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:update-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
		},
	}

	// Declared because these ARE published. The handler writes app:name onto the
	// context map and UpdateEventPropertiesFromStoreInfo adds app:icon and
	// app:title; PropertiesFromContext hands back THAT MAP BY REFERENCE, so
	// mutating it changes what every later publish on the same context carries.
	// The list said {} while the wire carried three properties.
	//
	// Declaring the reality is not a behaviour change; REMOVING the properties
	// would be, and on a frozen line could break a subscriber already reading
	// them. Per-event filtering is deliberately NOT built on top of this yet: a
	// filter that silently drops against a declaration that lies is worse than
	// no filter, so the declaration tells the truth first.
	EventTypeAppUpdateEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:update-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeAppUpdateError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:update-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeMessage,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageUpdated,
		},
	}

	// Declared because these ARE published. The handler writes app:name onto the
	// context map and UpdateEventPropertiesFromStoreInfo adds app:icon and
	// app:title; PropertiesFromContext hands back THAT MAP BY REFERENCE, so
	// mutating it changes what every later publish on the same context carries.
	// The list said {} while the wire carried three properties.
	//
	// Declaring the reality is not a behaviour change; REMOVING the properties
	// would be, and on a frozen line could break a subscriber already reading
	// them. Per-event filtering is deliberately NOT built on top of this yet: a
	// filter that silently drops against a declaration that lies is worse than
	// no filter, so the declaration tells the truth first.
	EventTypeAppApplyChangesBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:apply-changes-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
		},
	}

	// Declared because these ARE published. The handler writes app:name onto the
	// context map and UpdateEventPropertiesFromStoreInfo adds app:icon and
	// app:title; PropertiesFromContext hands back THAT MAP BY REFERENCE, so
	// mutating it changes what every later publish on the same context carries.
	// The list said {} while the wire carried three properties.
	//
	// Declaring the reality is not a behaviour change; REMOVING the properties
	// would be, and on a frozen line could break a subscriber already reading
	// them. Per-event filtering is deliberately NOT built on top of this yet: a
	// filter that silently drops against a declaration that lies is worse than
	// no filter, so the declaration tells the truth first.
	EventTypeAppApplyChangesEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:apply-changes-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
		},
	}

	EventTypeAppApplyChangesError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:apply-changes-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeMessage,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
		},
	}

	EventTypeAppStartBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:start-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppStartEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:start-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppStartError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:start-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}

	EventTypeAppStopBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:stop-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppStopEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:stop-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppStopError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:stop-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}

	EventTypeAppRestartBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:restart-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppRestartEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:restart-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppRestartError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:restart-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}
)

// event types for image
var (
	EventTypeImagePullBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:pull-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
		},
	}

	EventTypeImagePullProgress = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:pull-progress",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}

	EventTypeImagePullEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:pull-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeImageUpdated,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
		},
	}

	EventTypeImagePullError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:pull-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeImageRemoveBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:remove-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
		},
	}

	EventTypeImageRemoveEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:remove-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
		},
	}

	EventTypeImageRemoveError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:remove-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
		},
	}
)

// event types for container
var (
	EventTypeContainerCreateBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:create-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerName,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerCreateEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:create-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeContainerName,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerCreateError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:create-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerName,
			PropertyTypeMessage,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerStartBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:start-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerStartEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:start-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerStartError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:start-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeMessage,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageName,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerStopBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:stop-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerStopEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:stop-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageUpdated,
			PropertyTypeContainerName,
			PropertyTypeMessage,
		},
	}

	EventTypeContainerStopError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:stop-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeMessage,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerRenameBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:rename-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeContainerName,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerRenameEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:rename-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeContainerName,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerRenameError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:rename-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeContainerName,
			PropertyTypeMessage,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerRemoveBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:remove-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerRemoveEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:remove-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppTitle,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeContainerRemoveError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:remove-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeMessage,
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeImageUpdated,
		},
	}
)
