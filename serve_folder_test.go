package main

import (
	"image/color"
	"strings"
	"testing"
)

// The folder a phone sends with startUpload is kept in the sidecar as it was sent, beside what the server's rules make
// of the source; one that no device should send is refused.
func TestUploadsKeepThePhonesFolder(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	sha := env.uploadPhotoWith(t, phone, plainJPEG(t, color.RGBA{220, 20, 20, 255}), "Screenshot_20240301_030715_Bumble.jpg",
		map[string]any{"source": "folder:Telegram", "folder": " Pictures/Telegram/ "})
	item, ok := env.library.Store("hen").Get(sha)
	if !ok {
		t.Fatal("not stored")
	}
	facts := item.Sidecar.Facts
	if facts.DeviceFolder == nil || *facts.DeviceFolder != "Pictures/Telegram/" || *facts.Source != "screenshots" || *facts.SentFrom != "folder:Telegram" {
		t.Errorf("folder %v, source %v, sentFrom %v", facts.DeviceFolder, facts.Source, facts.SentFrom)
	}

	input := map[string]any{"sha256": strings.Repeat("a", 64), "size": 5, "fileName": "x.jpg", "mimeType": "image/jpeg", "kind": "PHOTO",
		"folder": "Pictures/\x00/"}
	if start := env.graphql(t, phone, startUploadMutation, map[string]any{"input": input}); len(start.Errors) == 0 {
		t.Errorf("a folder with a control character: %s", start.Data["startUpload"])
	}
}
