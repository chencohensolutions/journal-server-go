package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

// TestUploadedSoundRecordingsGetTheirFacts uploads sound recordings as a phone backing up a folder does: a call is
// told from its file name (or from the phone saying so), and its date comes from the name.
func TestUploadedSoundRecordingsGetTheirFacts(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	store := env.library.Store("hen")
	modified := "2025-07-26T15:30:00+03:00"

	// Bytes ffprobe can't read: the phone's facts stay, and the name still says who and when.
	named := env.uploadPhotoWith(t, phone, []byte("a call, as far as the test goes"), "Call recording 0501234567_250726_152307.m4a",
		map[string]any{"kind": "AUDIO", "mimeType": "audio/mp4", "takenAt": modified})
	item, ok := store.Get(named)
	if !ok {
		t.Fatal("not stored")
	}
	facts := item.Sidecar.Facts
	if facts.Kind != media.KindAudio || facts.Call == nil || facts.Call.Number == nil || *facts.Call.Number != "0501234567" {
		t.Fatalf("call %+v (kind %s)", facts.Call, facts.Kind)
	}
	if facts.TakenAt == nil || facts.TakenAt.Format(time.RFC3339) != "2025-07-26T15:23:07+03:00" {
		t.Errorf("takenAt %v, want the name's time in the phone's zone", facts.TakenAt)
	}

	// A folder of calls whose names say nothing: the phone sends an empty call.
	unnamed := env.uploadPhotoWith(t, phone, []byte("a call from a calls folder"), "2020-04-10 00.02.41.ogg",
		map[string]any{"kind": "AUDIO", "mimeType": "audio/ogg", "takenAt": modified, "call": map[string]any{}})
	if item, _ := store.Get(unnamed); item.Sidecar.Facts.Call == nil || media.AudioCollectionOf(item.Sidecar.Facts) != media.AudioCalls {
		t.Errorf("unnamed call: %+v", item.Sidecar.Facts.Call)
	}

	// A FLAC the phone made from a WAV: asking for the WAV finds it.
	wavSha := strings.Repeat("ab", 32)
	flac := env.uploadPhotoWith(t, phone, []byte("a FLAC made on the phone"), "recording.flac",
		map[string]any{"kind": "AUDIO", "mimeType": "audio/flac", "takenAt": modified,
			"encodedFrom": map[string]any{"sha256": wavSha, "mimeType": "audio/wav", "size": 100}})
	if got := string(env.graphql(t, phone, `query($h: [Sha256!]!) { missingMedia(hashes: $h) }`, map[string]any{"h": []string{wavSha, flac}}).Data["missingMedia"]); got != `[]` {
		t.Errorf("missingMedia = %s, want the WAV found by its FLAC", got)
	}
	start := env.graphql(t, phone, startUploadMutation, map[string]any{"input": map[string]any{
		"sha256": wavSha, "size": 100, "fileName": "recording.wav", "mimeType": "audio/wav", "kind": "AUDIO",
	}})
	if !strings.Contains(string(start.Data["startUpload"]), "MediaExists") {
		t.Errorf("startUpload of the WAV = %s", start.Data["startUpload"])
	}
}

// TestAnUploadedWAVIsKeptAsFLAC uploads a WAV as it is: the server keeps a FLAC with both sha256s, and the WAV's
// sha256 finds it everywhere a device asks.
func TestAnUploadedWAVIsKeptAsFLAC(t *testing.T) {
	ffmpeg, _, err := media.FindFFmpeg("")
	if err != nil {
		t.Skip("needs ffmpeg")
	}
	wavPath := filepath.Join(t.TempDir(), "20210809_114634.wav")
	if out, err := exec.Command(ffmpeg, "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:a", "pcm_s16le", wavPath).CombinedOutput(); err != nil {
		t.Fatalf("making the WAV: %v: %s", err, out)
	}
	wav, _ := os.ReadFile(wavPath)
	sum := sha256.Sum256(wav)
	wavSha := hex.EncodeToString(sum[:])

	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	store := env.library.Store("hen")
	if store.FFmpeg == "" {
		t.Skip("the test server found no ffmpeg")
	}
	env.uploadPhotoWith(t, phone, wav, "20210809_114634.wav", map[string]any{"kind": "AUDIO", "mimeType": "audio/wav", "takenAt": "2021-08-09T11:47:42+03:00"})

	if _, ok := store.Get(wavSha); ok {
		t.Fatal("the WAV was kept")
	}
	item, ok := store.Find(wavSha)
	if !ok {
		t.Fatal("the WAV's sha256 finds nothing")
	}
	facts := item.Sidecar.Facts
	if facts.MimeType != "audio/flac" || facts.FileName != "20210809_114634.flac" || facts.EncodedFrom == nil || facts.EncodedFrom.Sha256 != wavSha {
		t.Fatalf("stored as %s %s, encodedFrom %+v", facts.FileName, facts.MimeType, facts.EncodedFrom)
	}
	if facts.DurationMs == nil || *facts.DurationMs < 900 {
		t.Errorf("duration %v", facts.DurationMs)
	}
	if facts.TakenAt == nil || facts.TakenAt.Format("15:04:05") != "11:46:34" {
		t.Errorf("takenAt %v, want the name's 11:46:34", facts.TakenAt)
	}
	if got := string(env.graphql(t, phone, `query($h: [Sha256!]!) { missingMedia(hashes: $h) }`, map[string]any{"h": []string{wavSha, item.Sha256()}}).Data["missingMedia"]); got != `[]` {
		t.Errorf("missingMedia = %s", got)
	}
	resp, err := phone.Post(env.server.URL+"/media/"+wavSha+"/verify", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"result":"ok"`) {
		t.Errorf("verify the WAV: %d %s", resp.StatusCode, body)
	}
}
