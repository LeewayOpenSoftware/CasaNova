package service

import "testing"

// Reproduces the navidrome 2026-07-17 data-loss: a truncated compose (no
// volumes, no env, untagged image) applied over an installed app must NOT drop
// the installed volumes/env/image-tag.
func TestPreserveInstalledServiceData_NavidromeSkeleton(t *testing.T) {
	installedYAML := []byte(`name: navidrome
services:
  navidrome:
    container_name: navidrome
    image: deluan/navidrome:0.58.5
    environment:
      ND_LOGLEVEL: info
      TZ: Etc/UTC
    volumes:
      - type: bind
        source: /mnt/UNIT5/DATA/AppData/navidrome/data
        target: /data
      - type: bind
        source: /mnt/UNIT1a_UNIT/MEDIA/Musica
        target: /music
`)
	// The exact 184-byte skeleton that wiped navidrome.
	truncatedYAML := []byte(`name: navidrome
services:
  navidrome:
    container_name: navidrome
    image: deluan/navidrome
    ports:
      - "4533:4533"
    restart: unless-stopped
`)

	installed, err := NewComposeAppFromYAML(installedYAML, true, true)
	if err != nil {
		t.Fatalf("load installed: %v", err)
	}
	got, err := NewComposeAppFromYAMLForUpdate(truncatedYAML, installed)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	svc, ok := got.Services["navidrome"]
	if !ok {
		t.Fatal("navidrome service missing after update")
	}
	if len(svc.Volumes) != 2 {
		t.Fatalf("volumes dropped: want 2 (data+music), got %d — DATA LOSS", len(svc.Volumes))
	}
	if len(svc.Environment) == 0 {
		t.Fatal("environment dropped — config loss")
	}
	if tagOf(svc.Image) == "" {
		t.Fatalf("image left untagged: %q — would pull :latest", svc.Image)
	}

	// A real edit (non-empty volumes) must WIN, not be overridden.
	editedYAML := []byte(`name: navidrome
services:
  navidrome:
    image: deluan/navidrome:0.59.0
    volumes:
      - type: bind
        source: /mnt/UNIT5/DATA/AppData/navidrome/data
        target: /data
`)
	edited, err := NewComposeAppFromYAMLForUpdate(editedYAML, installed)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := len(edited.Services["navidrome"].Volumes); got != 1 {
		t.Fatalf("explicit volume edit not honored: want 1, got %d", got)
	}
	if img := edited.Services["navidrome"].Image; tagOf(img) != "0.59.0" {
		t.Fatalf("explicit tag change not honored: %q", img)
	}
}
