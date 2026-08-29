package utils

import (
	"fmt"

	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/pkg/ysk"
)

// zimaSettingsPeerName is the verbatim peer/app name the ZimaOS client's YSK
// card renderer matches to open its in-app Settings module. The client
// dispatches by EXACT equality (name === "icewhale_settings") and then routes
// on routerPath (/storage, /network, /migration) — see the ZimaOS web bundle.
// This is a ZimaOS-client wire contract the client UI parses verbatim, NOT a
// CasaNova brand leftover; do NOT rename it.
const zimaSettingsPeerName = "icewhale_settings"

var (
	ApplicationInstallProgress = ysk.YSKCard{
		Id:         "task:application:install",
		CardType:   ysk.CardTypeTask,
		RenderType: ysk.RenderTypeCardTask,
		Content: ysk.YSKCardContent{
			TitleIcon:        ysk.AppStoreIcon,
			TitleText:        "APP Installing",
			BodyProgress:     &ysk.YSKCardProgress{},
			BodyIconWithText: nil,
			BodyList:         nil,
			FooterActions:    nil,
		},
	}

	ZimaOSDataStationNotice = ysk.YSKCard{
		Id:         "long-notice:disk:insert",
		CardType:   ysk.CardTypeLongNote,
		RenderType: ysk.RenderTypeCardIconTextNotice,
		Content: ysk.YSKCardContent{
			TitleIcon:    ysk.ServerIcon,
			TitleText:    "Build data station",
			BodyProgress: nil,
			BodyIconWithText: &ysk.YSKCardIconWithText{
				Icon:        ysk.DiskIcon,
				Description: "For a data station with more storage capacity, it is recommended to add more hard drives.",
			},
			BodyList: nil,
			FooterActions: []ysk.YSKCardFooterAction{
				{
					Side:  ysk.ActionPositionRight,
					Style: "primary",
					Text:  "Learn more",
					MessageBus: ysk.YSKCardMessageBusAction{
						Key:     "casaos-ui/casaos-ui:app:mircoapp_communicate",
						Payload: fmt.Sprintf(`{"action":"open","peerType":"settings","name":%q,"routerPath":"/storage"}`, zimaSettingsPeerName),
					},
				},
			},
		},
	}

	ZimaOSRemoteAccessNotice = ysk.YSKCard{
		Id:         "long-notice:remote:access",
		CardType:   ysk.CardTypeLongNote,
		RenderType: ysk.RenderTypeCardIconTextNotice,
		Content: ysk.YSKCardContent{
			TitleIcon:    ysk.ServerIcon,
			TitleText:    "Remote Access",
			BodyProgress: nil,
			BodyIconWithText: &ysk.YSKCardIconWithText{
				Icon:        ysk.ServerIcon,
				Description: "Configure Remote Access to access your home cloud remotely from anywhere.",
			},
			BodyList: nil,
			FooterActions: []ysk.YSKCardFooterAction{
				{
					Side:  ysk.ActionPositionRight,
					Style: "primary",
					Text:  "Learn more",
					MessageBus: ysk.YSKCardMessageBusAction{
						Key:     "casaos-ui/casaos-ui:app:mircoapp_communicate",
						Payload: fmt.Sprintf(`{"action":"open","peerType":"settings","name":%q,"routerPath":"/network"}`, zimaSettingsPeerName),
					},
				},
			},
		},
	}

	ZimaOSFileManagementNotice = ysk.YSKCard{
		Id:         "long-notice:file:management",
		CardType:   ysk.CardTypeLongNote,
		RenderType: ysk.RenderTypeCardIconTextNotice,
		Content: ysk.YSKCardContent{
			TitleIcon:    ysk.FileIcon,
			TitleText:    "File Management",
			BodyProgress: nil,
			BodyIconWithText: &ysk.YSKCardIconWithText{
				Icon:        ysk.FileIcon,
				Description: "Use Files to manage your data from different locations, such as your computer, phone, netdisk and server.",
			},
			BodyList: nil,
			FooterActions: []ysk.YSKCardFooterAction{
				{
					Side:  ysk.ActionPositionRight,
					Style: "primary",
					Text:  "Learn more",
					MessageBus: ysk.YSKCardMessageBusAction{
						// The ZimaOS client opens this URL verbatim
						// (window.open(origin + "/" + url)) to reach its Files
						// module; it is a client wire path, do NOT rename.
						Key:     "casaos-ui:open_files",
						Payload: `{"url": "/modules/icewhale_files/#"}`,
					},
				},
			},
		},
	}

	ApplicationUpdateNotice = ysk.YSKCard{
		Id:         "short-notice:application:update",
		CardType:   ysk.CardTypeShortNote,
		RenderType: ysk.RenderTypeCardListNotice,
		Content: ysk.YSKCardContent{
			TitleIcon: ysk.AppStoreIcon,
			TitleText: "App update available",
			BodyList: []ysk.YSKCardListItem{
				{
					Icon:        "jellyfin logo",
					Description: "Jellyfin 10.7.0",
					RightText:   "2 days ago",
				},
				{
					Icon:        "next-cloud logo",
					Description: "NextCloud 10.7.0",
					RightText:   "2 days ago",
				},
			},
			FooterActions: []ysk.YSKCardFooterAction{
				{
					Side:  ysk.ActionPositionRight,
					Style: "primary",
					Text:  "Update all",
					MessageBus: ysk.YSKCardMessageBusAction{
						Key:     "open:application:update",
						Payload: "{'type':'all'}",
					},
				},
			},
		},
	}

	DiskInsertNotice = ysk.YSKCard{
		Id:         "long-notice:disk:insert",
		CardType:   ysk.CardTypeLongNote,
		RenderType: ysk.RenderTypeCardListNotice,
		Content: ysk.YSKCardContent{
			TitleIcon: ysk.ServerIcon,
			TitleText: "Found a new device",
			BodyList: []ysk.YSKCardListItem{
				{
					Icon:        ysk.StorageIcon,
					Description: "ZimaOS-HD",
					RightText:   "2TB",
				}, {
					Icon:        ysk.StorageIcon,
					Description: "Safe-Storage",
					RightText:   "2TB",
				},
			},
			FooterActions: []ysk.YSKCardFooterAction{
				{
					Side:  ysk.ActionPositionRight,
					Style: "primary",
					Text:  "Manage",
					MessageBus: ysk.YSKCardMessageBusAction{
						Key:     "casaos-ui/casaos-ui:app:mircoapp_communicate",
						Payload: fmt.Sprintf(`{"action":"open","peerType":"settings","name":%q,"routerPath":"/storage"}`, zimaSettingsPeerName),
					},
				},
			},
		},
	}
)
