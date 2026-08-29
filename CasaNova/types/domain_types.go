package types

// Domain-specific constants: person/user actions, tasks, search categories,
// and database relation types. Consolidated from person.go, task.go,
// search.go, and rely.go so related domain enums live together.

// Person / user actions (used in messaging and protocol handlers)
const PERSONADDFRIEND = "add_user"
const PERSONAGREEFRIEND = "agree_user"
const PERSONDOWNLOAD = "file_data"
const PERSONSUMMARY = "summary"
const PERSONGETIP = "get_ip"
const PERSONCONNECTION = "connection"
const PERSONDIRECTORY = "directory"
const PERSONHELLO = "hello"
const PERSONSHAREID = "share_id"
const PERSONUPLOAD = "upload"
const PERSONUPLOADDATA = "upload_data"
const PERSONINTERNALINSPECTION = "internal_inspection"
const PERSONPING = "ping"
const PERSONIMAGETHUMBNAIL = "image_thumbnail"
const PERSONCANCEL = "cancel" // Cancel Download

// Person file transfer state
const (
	PERSONFILEDOWNLOAD = iota // default state
	PERSONFILEUPLOAD
	PERSONFILERECEIVEUPLOAD // receive upload file
)

// Task definitions
const (
	TASK_TYPE_USER = iota
	TASK_TYPE_APP
)

const (
	TASK_DATA_TYPE_LINK = iota
	TASK_DATA_TYPE_TEXT
)

const (
	TASK_STATE_UNCOMPLETE = iota
	TASK_STATE_COMPLETED
)

// Search category constants
const (
	APPLICATION = iota
	MEDIA
	PICTURE
	MUSIC
	SEARCH
	UNKNOWN
)

// Database "rely" (dependency) types — originally in rely.go
const RELY_TYPE_MYSQL = iota
