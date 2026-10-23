// Package auth holds the server's configuration and the identity of a request: the user and device
// that the client certificate named. See docs/authentication.md.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
)

type UserConfig struct {
	// Also the common name (CN) of the user's client certificates: enrollment signs CN=<username>.
	Username string `json:"username"`
	// The user's journal: originals with their sidecars, stories, and the rebuildable .cache/.
	DataFolder string `json:"data_folder"`
	// Keep the previews made for this user's devices (and developed DNGs) in the data folder's .cache, so
	// asking again is instant. Off by default: devices keep the previews they asked for themselves.
	PreviewCache bool `json:"preview_cache"`
}

type AppConfig struct {
	TLSCert  string `json:"tls_cert"`
	TLSKey   string `json:"tls_key"`
	ClientCA string `json:"client_ca"`
	// Private key of ClientCA; signs the certificates issued to enrolled devices.
	ClientCAKey string `json:"client_ca_key"`
	// Address devices use to reach the server (e.g. https://192.168.50.2:56398); printed by enroll-code.
	PublicURL string `json:"public_url"`
	// Port devices connect to (default 56398); a test server beside the real one uses another.
	Port string `json:"port"`
	// Registry of enrolled devices (default devices.json).
	DevicesFile string `json:"devices_file"`
	// Loopback-only listener for the admin GraphQL API (default 127.0.0.1:8081).
	AdminAddr string `json:"admin_addr"`
	// Token for the admin API, rewritten on every server start (default admin.token).
	AdminTokenFile string `json:"admin_token_file"`
	// Path of the libvips `vips` program, used for previews; found on the PATH or in Homebrew when empty.
	Vips string `json:"vips"`
	// Path of the libvips library to load into the server (libvips-42.dll, libvips.42.dylib, libvips.so.42); the one
	// beside the vips program, or the system's, when empty.
	LibVips string `json:"libvips"`
	// Keep libvips out of the server: previews and the indexes' pictures are made by the vips programs, as before
	// libvips was loaded in (each start of them costs about 350 ms on Windows). A crash in libvips can't take the
	// server down then.
	VipsAsProgram bool `json:"vips_as_program"`
	// How many files are decoded ahead of the indexes (nudity, faces, search) while they read the one before
	// (default 2).
	ReaderPrepare int `json:"reader_prepare"`
	// Path of the ffmpeg program, used for video posters (ffprobe beside it); found on the PATH or in Homebrew when empty.
	FFmpeg string `json:"ffmpeg"`
	// Most space the previews of one user may take before the least recently used are deleted (default 50).
	PreviewCacheMaxGB float64 `json:"preview_cache_max_gb"`
	// Folder of the SigLIP 2 model for text search, made by scripts/bundle-onnx; models/siglip2-base-patch16-256
	// when empty. Search is off when the folder isn't there.
	SearchModel string `json:"search_model"`
	// Folder of the face models (YuNet and SFace) for face search, made by scripts/bundle-onnx; models/yunet-sface
	// when empty. Face search is off when the folder isn't there.
	FaceModel string `json:"face_model"`
	// Path of NudeNet's detector (320n.onnx) for the nudity scan, downloaded by scripts/bundle-onnx;
	// models/nudenet/320n.onnx when empty. New files aren't scanned when it isn't there.
	NudityModel string `json:"nudity_model"`
	// Folder of DINOv2 for similar photos and duplicates, made by scripts/bundle-onnx; models/dinov2-small when
	// empty. Off when the folder isn't there.
	SimilarModel string `json:"similar_model"`
	// Folder of the OCR models (Hebrew and English) for text in photos, made by scripts/bundle-onnx;
	// models/paddleocr-hebrew when empty. Off when the folder isn't there.
	OCRModel string `json:"ocr_model"`
	// Folder of CLIP ViT-L/14 and LAION's head for aesthetic scores, made by scripts/bundle-onnx;
	// models/clip-vit-large-patch14-laion when empty. Off when the folder isn't there.
	AestheticModel string `json:"aesthetic_model"`
	// Path of the ONNX Runtime library, which runs the search, face and nudity models; the one in ./onnxruntime (scripts/bundle-onnx)
	// or Homebrew's when empty.
	OnnxRuntime string `json:"onnxruntime"`
	// CPU threads each half of the search model, and each face model, may use (default half the CPUs): reading new
	// files takes this many while the server answers devices with the rest.
	SearchThreads int          `json:"search_threads"`
	Users         []UserConfig `json:"users"`
}

const (
	DefaultPort           = "56398"
	DefaultDevicesFile    = "devices.json"
	DefaultAdminAddr      = "127.0.0.1:8081"
	DefaultAdminTokenFile = "admin.token"
	DefaultPreviewCacheGB = 50
	DefaultSearchModel    = "models/siglip2-base-patch16-256"
	DefaultFaceModel      = "models/yunet-sface"
	DefaultNudityModel    = "models/nudenet/320n.onnx"
	DefaultSimilarModel   = "models/dinov2-small"
	DefaultOCRModel       = "models/paddleocr-hebrew"
	DefaultAestheticModel = "models/clip-vit-large-patch14-laion"
)

// LoadConfig reads config.json (or another file) and fills in the defaults.
func LoadConfig(path string) (AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AppConfig{}, fmt.Errorf("failed to open %s: %w", path, err)
	}
	var config AppConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return AppConfig{}, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	// The username is the identity in every client certificate, so it must name one user only.
	seen := map[string]bool{}
	for _, user := range config.Users {
		if user.Username == "" {
			return AppConfig{}, fmt.Errorf("%s: a user has no username", path)
		}
		if seen[user.Username] {
			return AppConfig{}, fmt.Errorf("%s: user %q appears twice", path, user.Username)
		}
		seen[user.Username] = true
	}
	return config.WithDefaults(), nil
}

// WithDefaults fills in the optional settings.
func (c AppConfig) WithDefaults() AppConfig {
	if c.Port == "" {
		c.Port = DefaultPort
	}
	if c.DevicesFile == "" {
		c.DevicesFile = DefaultDevicesFile
	}
	if c.AdminAddr == "" {
		c.AdminAddr = DefaultAdminAddr
	}
	if c.AdminTokenFile == "" {
		c.AdminTokenFile = DefaultAdminTokenFile
	}
	if c.PreviewCacheMaxGB <= 0 {
		c.PreviewCacheMaxGB = DefaultPreviewCacheGB
	}
	if c.SearchModel == "" {
		c.SearchModel = DefaultSearchModel
	}
	if c.FaceModel == "" {
		c.FaceModel = DefaultFaceModel
	}
	if c.NudityModel == "" {
		c.NudityModel = DefaultNudityModel
	}
	if c.SimilarModel == "" {
		c.SimilarModel = DefaultSimilarModel
	}
	if c.OCRModel == "" {
		c.OCRModel = DefaultOCRModel
	}
	if c.AestheticModel == "" {
		c.AestheticModel = DefaultAestheticModel
	}
	if c.ReaderPrepare <= 0 {
		c.ReaderPrepare = 2
	}
	if c.SearchThreads <= 0 {
		c.SearchThreads = max(1, runtime.NumCPU()/2)
	}
	return c
}

func (c AppConfig) FindUser(username string) *UserConfig {
	for i := range c.Users {
		if c.Users[i].Username == username {
			return &c.Users[i]
		}
	}
	return nil
}

// DataFolderOf is the data folder of username, for the scripts that work on one user's folder.
func (c AppConfig) DataFolderOf(username string) (string, error) {
	user := c.FindUser(username)
	if user == nil {
		return "", fmt.Errorf("user %q is not in config.json", username)
	}
	if user.DataFolder == "" {
		return "", fmt.Errorf("user %q has no data_folder in config.json", username)
	}
	return user.DataFolder, nil
}
