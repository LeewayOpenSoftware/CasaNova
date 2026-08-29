package types

// State constants for user/friend relationships, notifications, and downloads.
// Grouped here (formerly in friend.go, notify.go, person_download.go) so
// related enums live together and are easier to locate.

// Friend relationship state
const (
	FRIENDSTATEDEFAULT = iota
	FRIENDSTATEWAIT
	FRIENDSTATEREQUEST
)

// Notification read state
const (
	NOTIFY_DYNAMICE = iota
	NOTIFY_UNREAD
	NOTIFY_READ
)

// Notification type / severity
const (
	NOTIFY_TYPE_UNIMPORTANT = iota + 1
	NOTIFY_TYPE_NEED_CONFIRM
	NOTIFY_TYPE_ERROR
	NOTIFY_TYPE_INSTALL_LOG
	NOTIFY_TYPE_PERSION_FIRNED_LEAVE
	NOTIFY_TYPE_PERSION_FIRNED_LIVE
	NOTIFY_TYPE_HEALTH_CHECK
)

// Notification category
const (
	NOTIFY_APP = iota
)

// File download state
const (
	DOWNLOADAWAIT = iota // default state
	DOWNLOADING
	DOWNLOADPAUSE
	DOWNLOADFINISH
	DOWNLOADERROR
	DOWNLOADFINISHED
)
