package auth

import (
	"context"

	"github.com/journal/server-go/media"
)

type contextKey string

const (
	userContextKey      contextKey = "user"
	clientIPContextKey  contextKey = "client_ip"
	deviceContextKey    contextKey = "device"
	withholdContextKey  contextKey = "withhold_private"
	showAudioContextKey contextKey = "show_audio"
	serialContextKey    contextKey = "serial"
)

// WithSerial records the serial of the client certificate.
func WithSerial(ctx context.Context, serial string) context.Context {
	return context.WithValue(ctx, serialContextKey, serial)
}

// SerialFromContext is the client certificate's serial, or "" for an anonymous request.
func SerialFromContext(ctx context.Context) string {
	serial, _ := ctx.Value(serialContextKey).(string)
	return serial
}

// WithUser records the user the client certificate named.
func WithUser(ctx context.Context, user *UserConfig) context.Context {
	return context.WithValue(ctx, userContextKey, user)
}

// FromContext returns the user authenticated by client certificate, or nil for an anonymous request.
func FromContext(ctx context.Context) *UserConfig {
	user, _ := ctx.Value(userContextKey).(*UserConfig)
	return user
}

func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPContextKey, ip)
}

func ClientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPContextKey).(string)
	return ip
}

// WithDeviceName records which enrolled device sent the request.
func WithDeviceName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, deviceContextKey, name)
}

// DeviceNameFromContext is the enrolled device's name, or "" for certificates issued outside enrollment.
func DeviceNameFromContext(ctx context.Context) string {
	name, _ := ctx.Value(deviceContextKey).(string)
	return name
}

// SoundRecordings is what a device made for sound recordings (Audio Logs, enroll.Device.SeesSoundRecordings) shows of
// them, whatever the user hides on other devices: recordings always, calls and voice messages when the user turned
// them on in that app (setSoundRecordingsShown).
type SoundRecordings struct {
	Calls         bool
	VoiceMessages bool
}

// WithSoundRecordings records that the device sees sound recordings as shown says.
func WithSoundRecordings(ctx context.Context, shown SoundRecordings) context.Context {
	return context.WithValue(ctx, showAudioContextKey, shown)
}

// SoundRecordingsFromContext is what the device shows of sound recordings, and false for a device that follows what
// the user hides (every app but Audio Logs).
func SoundRecordingsFromContext(ctx context.Context) (SoundRecordings, bool) {
	shown, ok := ctx.Value(showAudioContextKey).(SoundRecordings)
	return shown, ok
}

// ViewFor is the view of store the device making the request has: kept from private files or not, and what it shows
// of sound recordings (media.Store.ViewFor).
func ViewFor(ctx context.Context, store *media.Store) media.View {
	var audio *media.AudioHiding
	if shown, ok := SoundRecordingsFromContext(ctx); ok {
		audio = &media.AudioHiding{Calls: !shown.Calls, VoiceMessages: !shown.VoiceMessages}
	}
	return store.ViewFor(WithholdPrivateFromContext(ctx), audio)
}

// WithWithholdPrivate records that the device is kept from the files the user marked private.
func WithWithholdPrivate(ctx context.Context) context.Context {
	return context.WithValue(ctx, withholdContextKey, true)
}

// WithholdPrivateFromContext reports whether the device is kept from private files; false for certificates issued
// outside enrollment.
func WithholdPrivateFromContext(ctx context.Context) bool {
	withhold, _ := ctx.Value(withholdContextKey).(bool)
	return withhold
}
