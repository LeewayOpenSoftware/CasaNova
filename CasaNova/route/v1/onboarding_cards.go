package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/external"
)

// ---- First-boot onboarding YSK cards ----
//
// CasaNova-MessageBus already seeds three Zima-style cards on first
// boot (Data Station / File Management / Remote Access). We layer two
// CasaNova-specific cards on top of that — both purely diagnostic
// hints the user can dismiss permanently:
//
//   1. "Set up your storage" — fires when neither /DATA nor a mounted
//      disk under /media exists, which means apps will install to the
//      root filesystem (probably the small OS SSD).
//   2. "Pick a remote-access connector" — fires when none of Tailscale,
//      Cloudflared, or ZeroTier are installed, so the user knows the
//      box is currently LAN-only.
//
// We POST these into MessageBus on a slow ticker rather than once-at-
// boot so a user who plugs a disk in after install sees the card go
// away without needing to restart. MessageBus' upsert is idempotent
// keyed by card ID.
//
// This file ships the *publisher*. The seeding cards from MessageBus'
// own onboarding flow continue to work unchanged — we coexist.

const (
	cardIDStorageSetup = "casanova:onboarding:storage"
	cardIDRemoteAccess = "casanova:onboarding:remote_access"

	onboardingTickerInterval = 5 * time.Minute
)

// StartOnboardingCardsPublisher kicks off a goroutine that re-evaluates
// the onboarding hints every five minutes and upserts/deletes cards
// via the MessageBus client. Cheap: each evaluation is a couple of
// stat calls + an LookPath call. Cancellable via ctx so service
// shutdown propagates correctly.
func StartOnboardingCardsPublisher(ctx context.Context) {
	go func() {
		// One immediate evaluation so a fresh install doesn't have to
		// wait 5 min for the first card to appear.
		evaluateAndPublish(ctx)

		t := time.NewTicker(onboardingTickerInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				evaluateAndPublish(ctx)
			}
		}
	}()
}

func evaluateAndPublish(ctx context.Context) {
	// Storage card — fire when /DATA doesn't exist or is empty.
	if needsStorageHint() {
		upsertCard(ctx, cardIDStorageSetup, storageHintPayload())
	} else {
		deleteCard(ctx, cardIDStorageSetup)
	}

	// Remote-access card — fire when no connector is installed.
	if needsRemoteAccessHint() {
		upsertCard(ctx, cardIDRemoteAccess, remoteAccessHintPayload())
	} else {
		deleteCard(ctx, cardIDRemoteAccess)
	}
}

// needsStorageHint returns true when neither /DATA exists nor any
// reasonable storage is mounted under /DATA/. We don't probe /media
// or /mnt because those are user-mounted USB drives — the hint is
// specifically about the canonical /DATA mountpoint apps expect.
func needsStorageHint() bool {
	info, err := os.Stat("/DATA")
	if err != nil {
		// /DATA doesn't exist → definitely needs setup
		return true
	}
	if !info.IsDir() {
		return true
	}
	entries, err := os.ReadDir("/DATA")
	if err != nil {
		return true
	}
	// Empty /DATA also counts as "needs setup" because the only thing
	// a fresh CasaNova install creates is the dir itself (when an app
	// install bind-mounts into it). An empty /DATA means no real
	// storage is attached.
	return len(entries) == 0
}

func storageHintPayload() map[string]interface{} {
	return map[string]interface{}{
		"id":         cardIDStorageSetup,
		"cardType":   "long-notice",
		"renderType": "card",
		"content": map[string]interface{}{
			"titleIcon": "HardDrive",
			"titleText": "Set up your storage",
			"bodyIconWithText": map[string]string{
				"icon":        "AlertCircle",
				"description": "Apps will install to your system disk until you mount a drive. The Drive Setup Wizard walks through formatting, mounting, and pooling.",
			},
			"footerActions": []map[string]interface{}{
				{
					"side":  "right",
					"style": "primary",
					"text":  "Open Storage settings",
					"messageBus": map[string]interface{}{
						"sourceID": "casanova-ui",
						"name":     "casanova-ui:navigate",
						"properties": map[string]string{
							"path": "/app/settings/storage",
						},
					},
				},
			},
		},
	}
}

// needsRemoteAccessHint returns true when none of the three known
// connectors are detected. Re-uses the same probes as
// /v1/sys/remote-access/status to keep behavior consistent.
func needsRemoteAccessHint() bool {
	t := probeTailscale()
	c := probeCloudflared()
	z := probeZeroTier()
	return !t.Installed && !c.Installed && !z.Installed
}

func remoteAccessHintPayload() map[string]interface{} {
	return map[string]interface{}{
		"id":         cardIDRemoteAccess,
		"cardType":   "long-notice",
		"renderType": "card",
		"content": map[string]interface{}{
			"titleIcon": "Globe",
			"titleText": "Reach your server from outside",
			"bodyIconWithText": map[string]string{
				"icon":        "AlertCircle",
				"description": "No remote-access connector detected. Tailscale, Cloudflare Tunnel, or ZeroTier each give you a way to reach this box when you're not on the LAN.",
			},
			"footerActions": []map[string]interface{}{
				{
					"side":  "right",
					"style": "primary",
					"text":  "Open Remote Access",
					"messageBus": map[string]interface{}{
						"sourceID": "casanova-ui",
						"name":     "casanova-ui:navigate",
						"properties": map[string]string{
							"path": "/app/settings/remote-access",
						},
					},
				},
			},
		},
	}
}

// upsertCard publishes a ysk:card:upsert event with the card body as
// a JSON-encoded property. MessageBus listens for these events and
// writes the card to its sqlite store, then re-broadcasts on the WS
// so subscribed clients refresh. Errors are logged-and-swallowed —
// onboarding hints are best-effort.
func upsertCard(ctx context.Context, _ /*cardID*/ string, payload map[string]interface{}) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	publishMsgBusEvent(ctx, "casanova", "ysk:card:upsert", map[string]string{
		"body": string(body),
	})
}

func deleteCard(ctx context.Context, cardID string) {
	publishMsgBusEvent(ctx, "casanova", "ysk:card:delete", map[string]string{
		"id": cardID,
	})
}

// publishMsgBusEvent publishes over the trusted unix socket. Best-effort:
// a dial error just means the bus isn't up yet. (The old pre-check via
// GetMessageBusAddress was broken — it passed the FILE path as the runtime
// path, so the join produced ".../message-bus.url/message-bus.url", errored
// every time, and this publisher silently never published.)
func publishMsgBusEvent(ctx context.Context, sourceID, name string, properties map[string]string) {
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := external.PublishEventInSocket(pubCtx, sourceID, name, properties)
	if err != nil || resp == nil {
		return
	}
	// PublishEventInSocket already closes the body for us — see the
	// closed-body comment in CasaNova-Common/external/message_bus.go.
	_ = resp
	_ = http.StatusOK // keep net/http import alive even if PublishEventInSocket changes
}
