package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/journal/server-go/media"
)

func TestServerInfoSaysWhatWorks(t *testing.T) {
	env := newTestEnv(t)
	resp := env.graphql(t, env.enrolledClient(t), `{ serverInfo { api version search previews posters } }`, nil)
	var info struct {
		API                       int
		Version                   string
		Search, Previews, Posters bool
	}
	if err := json.Unmarshal(resp.Data["serverInfo"], &info); err != nil {
		t.Fatalf("serverInfo = %s %+v", resp.Data["serverInfo"], resp.Errors)
	}
	_, _, vipsErr := media.FindVips("")
	_, _, ffmpegErr := media.FindFFmpeg("")
	if info.API != 1 || info.Version == "" || info.Search || info.Previews != (vipsErr == nil) || info.Posters != (ffmpegErr == nil) {
		t.Fatalf("serverInfo = %+v (vips: %v, ffmpeg: %v)", info, vipsErr, ffmpegErr)
	}
}

func TestErrorsDevicesActOnCarryACode(t *testing.T) {
	env := newTestEnv(t)
	code := func(resp gqlResponse) string {
		t.Helper()
		if len(resp.Errors) == 0 {
			t.Fatal("no error")
		}
		c, _ := resp.Errors[0].Extensions["code"].(string)
		return c
	}
	if c := code(env.graphql(t, env.client(nil), `{ storageUsage { bytes } }`, nil)); c != "UNAUTHORIZED" {
		t.Fatalf("anonymous: %q", c)
	}
	phone := env.enrolledClient(t)
	hashes := make([]string, 1001)
	for i := range hashes {
		hashes[i] = strings.Repeat("a", 64)
	}
	if c := code(env.graphql(t, phone, `query($h: [Sha256!]!) { missingMedia(hashes: $h) }`, map[string]any{"h": hashes})); c != "INVALID_ARGUMENT" {
		t.Fatalf("too many hashes: %q", c)
	}
	if c := code(env.graphql(t, phone, `{ searchMedia(text: "dog") { total } }`, nil)); c != "SEARCH_OFF" {
		t.Fatalf("search off: %q", c)
	}
}
