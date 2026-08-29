package maintenance

// The read-only panel (design §3.9), plus the categories that are designed but
// not yet delivered.
//
// This is not a category list with the buttons removed. It is the answer to
// the question the user is going to ask anyway — "and what about the 70 GB of
// Docker?" — and the reason it is read-only is in every Why line. The biggest
// number on this screen is the one nobody may touch, and saying so plainly is
// better than a button with a warning next to it.
//
// Bytes are left at zero here: most of these numbers come from Docker, which
// lives in another microservice, so the handler fills in what it can reach.
// The Key is stable and the UI translates from it; the English text is the
// fallback and the record of what we meant.

// ReportOnlyNotices returns the panel rows, in display order.
func ReportOnlyNotices() []Notice {
	return []Notice{
		{
			Key:   "docker-volumes-unused",
			Label: "Docker volumes with no container",
			Why:   "Docker does not know which app each one came from. \"Unused\" only means no container references it right now — and stopping an app removes its containers, so every app you switched off is on that list. Uninstalling an app is where its volumes get offered.",
			Link:  "/app/apps",
		},
		{
			Key:   "docker-containers-stopped",
			Label: "Stopped containers",
			Why:   "These are apps you turned off, not junk. Removing them loses the writable layer and makes the project disappear from compose ls, which is the \"installed but offline\" bug we already fixed once.",
			Link:  "/app/apps",
		},
		{
			Key:   "docker-images-tagged",
			Label: "Tagged images with no container",
			Why:   "These are the previous versions of your apps: the rollback. This box has already been left without internet while the LAN was fine, and an image deleted with no way to pull it again is a dead app.",
			Link:  "/app/apps",
		},
		{
			Key:   "docker-overlay2",
			Label: "Docker's layer store (/var/lib/docker/overlay2)",
			Why:   "That is the size of the tree, not what could be reclaimed. The layer mapping is Docker's internal business and changes between versions; dockerd is the only legitimate owner of it.",
		},
		{
			Key:   "appdata-without-app",
			Label: "AppData with no app installed",
			Why:   "\"No app installed\" also describes someone who uninstalled Nextcloud to reinstall it and expects to find their files again. Browse it yourself and decide.",
			Link:  "/app/files",
		},
		{
			Key:   "in-use",
			Label: "Images, volumes and data in use",
			Why:   "This is what makes the system work.",
		},
		{
			Key:   "user-caches",
			Label: "~/.cache of human accounts",
			Why:   "It belongs to a user account, not to the system. The owner already has go clean -cache and friends.",
		},
		{
			Key:   "trash",
			Label: "Trash",
			Why:   "The trash is the undo layer of a different feature, with 30 days of retention and its own janitor. It is emptied from its own screen, never from here.",
			Link:  "/app/files",
		},
		{
			Key:   "docker-dangling-images",
			Label: "Untagged Docker images (not implemented yet)",
			Why:   "Designed for a later phase. Doing it right means subtracting the images that running containers still reference, the ones every installed app's compose file names, and anything whose ${...} we could not resolve — and the Docker client lives in another service. Until all three subtractions exist, the number would be a promise we could not keep.",
		},
		{
			Key:   "container-log-rotation",
			Label: "Container logs that escape rotation (not implemented yet)",
			Why:   "Designed for a later phase. The fix is not deleting: log options are frozen when a container is CREATED, so containers older than the current daemon.json ignore it. The action worth shipping recreates those containers through our own compose flow — a button you press once instead of every month.",
		},
	}
}
