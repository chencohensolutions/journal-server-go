package bundle

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/journal/server-go/search"
)

// RuntimeVersion is the ONNX Runtime release to download; the server works with 1.16 or later.
const RuntimeVersion = "1.31.0"

// SearchModels names the SigLIP 2 models by size: base (the default, google/siglip2-base-patch16-256) reads a photo
// in about a tenth of a second on a desktop CPU; large (google/siglip2-so400m-patch16-256) finds better but is
// about four times slower.
var SearchModels = map[string]string{
	"base":  "siglip2-base-patch16-256",
	"large": "siglip2-so400m-patch16-256",
}

// Suffix of the files of each precision in onnx-community's repositories.
var precisions = map[string]string{"fp32": "", "q8": "_quantized"}

// NudeNet's detector, from its GitHub releases (which give no digest for it, so it is checked against this one).
const (
	nudityRelease = "notAI-tech/NudeNet/releases/tags/v3.4-weights"
	nudityFile    = "320n.onnx"
	nuditySha256  = "c15d8273adad2d0a92f014cc69ab2d6c311a06777a55545f2c4eb46f51911f0f"
)

// LAION's aesthetic head (sac+logos+ava1-l14-linearMSE.pth), from its GitHub repository.
const (
	laionHeadURL    = "https://github.com/christophschuhmann/improved-aesthetic-predictor/raw/main/sac+logos+ava1-l14-linearMSE.pth"
	laionHeadSha256 = "21dd590f3ccdc646f0d53120778b296013b096a035a2718c9cb0d511bff0f1e0"
)

// The face models: Hugging Face repository → file, kept under the same name.
var faceModels = [][2]string{
	{"opencv/face_detection_yunet", "face_detection_yunet_2023mar.onnx"},
	{"opencv/face_recognition_sface", "face_recognition_sface_2021dec.onnx"},
}

// ONNXOptions says what ONNX downloads, and where.
type ONNXOptions struct {
	RuntimeDir string // folder for the ONNX Runtime library (replaced); "onnxruntime" when empty
	ModelsDir  string // folder each model's folder is made in; "models" when empty
	Model      string // a key of SearchModels; "base" when empty
	Precision  string // "fp32" (the default) or "q8": a quarter of the size and faster, a little less exact
	// The other indexes' models; each is left out when false.
	Faces, Nudity, Similar, OCR, Aesthetic bool
}

// AllIndexes is ONNXOptions with every index's model, as scripts/bundle-onnx downloads by default.
func AllIndexes() ONNXOptions {
	return ONNXOptions{Faces: true, Nudity: true, Similar: true, OCR: true, Aesthetic: true}
}

// ONNX downloads what text search, face search and the other indexes need:
//
//   - ONNX Runtime, the library that runs the models, into RuntimeDir (from Microsoft's GitHub releases);
//   - a SigLIP 2 model, as ONNX files, into ModelsDir/<name> (from Hugging Face's onnx-community), and beside a
//     full-precision one its 8-bit text half, for phones that search themselves;
//   - the face models, YuNet (finds faces, MIT licence) and SFace (tells them apart, Apache 2.0), into
//     ModelsDir/yunet-sface (from OpenCV's model zoo on Hugging Face);
//   - NudeNet's detector (320n.onnx, AGPL-3.0), into ModelsDir/nudenet (from NudeNet's GitHub releases);
//   - DINOv2 small (Meta, Apache 2.0), for similar photos and duplicates, into ModelsDir/dinov2-small (from
//     onnx-community);
//   - a Hebrew and English PaddleOCR (a word detector and the PP-OCRv6 word recognizer of Rivok/paddleocr-hebrew,
//     Apache 2.0, whose 120 characters are Hebrew, Latin, digits and signs), for text in photos, into
//     ModelsDir/paddleocr-hebrew;
//   - CLIP ViT-L/14's vision half (OpenAI, MIT; 8-bit, from Xenova) and LAION's aesthetic predictor (the
//     sac+logos+ava1 linear head of christophschuhmann/improved-aesthetic-predictor, Apache 2.0), for aesthetic
//     scores, into ModelsDir/clip-vit-large-patch14-laion. The head's five linear layers (no activation between
//     them) are multiplied out here into one: 768 weights and a bias, kept as laion-head.json.
//
// It returns the search model's folder, for "search_model" in config.json (the default folder needs no entry).
func ONNX(ctx context.Context, o ONNXOptions) (searchModel string, err error) {
	if o.RuntimeDir == "" {
		o.RuntimeDir = "onnxruntime"
	}
	if o.ModelsDir == "" {
		o.ModelsDir = "models"
	}
	if o.Model == "" {
		o.Model = "base"
	}
	if o.Precision == "" {
		o.Precision = "fp32"
	}
	name, ok := SearchModels[o.Model]
	if !ok {
		return "", fmt.Errorf("no model %q (base or large)", o.Model)
	}
	suffix, ok := precisions[o.Precision]
	if !ok {
		return "", fmt.Errorf("no precision %q (fp32 or q8)", o.Precision)
	}

	if err := bundleRuntime(ctx, o.RuntimeDir); err != nil {
		return "", err
	}

	folder := name
	if o.Precision != "fp32" {
		folder += "-" + o.Precision
	}
	out := filepath.Join(o.ModelsDir, folder)
	if err := bundleModel(ctx, "onnx-community/"+name+"-ONNX", suffix, out); err != nil {
		return "", err
	}
	if o.Faces {
		facesOut := filepath.Join(o.ModelsDir, "yunet-sface")
		for _, m := range faceModels {
			hashes, err := repoHashes(ctx, m[0])
			if err != nil {
				return "", err
			}
			if err := download(ctx, "https://huggingface.co/"+m[0]+"/resolve/main/"+m[1], filepath.Join(facesOut, m[1]), hashes[m[1]]); err != nil {
				return "", err
			}
		}
		Logf("Face models in %s", facesOut)
	}
	if o.Nudity {
		var release struct {
			// The API's link to the file: its browser link may answer a web page instead.
			Assets []struct{ Name, URL string }
		}
		if err := getJSON(ctx, "https://api.github.com/repos/"+nudityRelease, &release); err != nil {
			return "", err
		}
		to := filepath.Join(o.ModelsDir, "nudenet", nudityFile)
		for _, a := range release.Assets {
			if a.Name == nudityFile {
				if err := download(ctx, a.URL, to, nuditySha256); err != nil {
					return "", err
				}
			}
		}
		if _, err := os.Stat(to); err != nil {
			return "", fmt.Errorf("%s is not in %s", nudityFile, nudityRelease)
		}
		Logf("NudeNet's detector in %s", to)
	}
	if o.Similar {
		if err := bundleFiles(ctx, "onnx-community/dinov2-small-ONNX", filepath.Join(o.ModelsDir, "dinov2-small"), map[string]string{
			"onnx/model.onnx": "model.onnx", "preprocessor_config.json": "preprocessor_config.json"}); err != nil {
			return "", err
		}
	}
	if o.OCR {
		if err := bundleFiles(ctx, "Rivok/paddleocr-hebrew", filepath.Join(o.ModelsDir, "paddleocr-hebrew"), map[string]string{
			"word-det/det.onnx": "det.onnx", "server-v6/rec.onnx": "rec.onnx", "charset_v2f.txt": "charset.txt"}); err != nil {
			return "", err
		}
	}
	if o.Aesthetic {
		aestheticOut := filepath.Join(o.ModelsDir, "clip-vit-large-patch14-laion")
		if err := bundleFiles(ctx, "Xenova/clip-vit-large-patch14", aestheticOut, map[string]string{
			"onnx/vision_model_quantized.onnx": "vision_model.onnx", "preprocessor_config.json": "preprocessor_config.json"}); err != nil {
			return "", err
		}
		pth := filepath.Join(aestheticOut, "sac+logos+ava1-l14-linearMSE.pth")
		if err := download(ctx, laionHeadURL, pth, laionHeadSha256); err != nil {
			return "", err
		}
		if err := writeLAIONHead(pth, filepath.Join(aestheticOut, "laion-head.json")); err != nil {
			return "", err
		}
		Logf("LAION's aesthetic head in %s", filepath.Join(aestheticOut, "laion-head.json"))
	}
	return out, nil
}

// bundleRuntime puts this platform's ONNX Runtime library (and the libraries it loads) in dir.
func bundleRuntime(ctx context.Context, dir string) error {
	var asset string
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		asset = "onnxruntime-win-x64-" + RuntimeVersion + ".zip"
	case "windows/arm64":
		asset = "onnxruntime-win-arm64-" + RuntimeVersion + ".zip"
	case "darwin/arm64":
		asset = "onnxruntime-osx-arm64-" + RuntimeVersion + ".tgz"
	case "linux/amd64":
		asset = "onnxruntime-linux-x64-" + RuntimeVersion + ".tgz"
	case "linux/arm64":
		asset = "onnxruntime-linux-aarch64-" + RuntimeVersion + ".tgz"
	default:
		return fmt.Errorf("no ONNX Runtime download for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	var release struct {
		Assets []struct {
			Name, Digest string
			URL          string `json:"browser_download_url"`
		}
	}
	if err := getJSON(ctx, "https://api.github.com/repos/microsoft/onnxruntime/releases/tags/v"+RuntimeVersion, &release); err != nil {
		return err
	}
	cache, err := cacheDir("onnxruntime")
	if err != nil {
		return err
	}
	archive := filepath.Join(cache, asset)
	for _, a := range release.Assets {
		if a.Name == asset {
			if err := download(ctx, a.URL, archive, strings.TrimPrefix(a.Digest, "sha256:")); err != nil {
				return err
			}
		}
	}
	if _, err := os.Stat(archive); err != nil {
		return fmt.Errorf("%s is not in ONNX Runtime %s", asset, RuntimeVersion)
	}

	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Only the shared libraries in lib/; headers and the static libraries aren't needed.
	wanted := func(name string) (string, bool) {
		dirName, base := path.Split(name)
		isLib := strings.HasSuffix(base, ".dll") || strings.Contains(base, ".dylib") || strings.Contains(base, ".so")
		return base, strings.HasSuffix(dirName, "/lib/") && isLib
	}
	n := 0
	if strings.HasSuffix(archive, ".zip") {
		z, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer z.Close()
		for _, f := range z.File {
			if base, ok := wanted(f.Name); ok {
				r, err := f.Open()
				if err != nil {
					return err
				}
				err = writeFile(filepath.Join(dir, base), r)
				r.Close()
				if err != nil {
					return err
				}
				n++
			}
		}
	} else {
		f, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		t := tar.NewReader(gz)
		var links [][2]string
		for {
			h, err := t.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			base, ok := wanted(h.Name)
			switch {
			case !ok:
			case h.Typeflag == tar.TypeSymlink:
				// libonnxruntime.dylib → libonnxruntime.1.31.0.dylib; a copy works everywhere.
				links = append(links, [2]string{filepath.Join(dir, path.Base(h.Linkname)), filepath.Join(dir, base)})
				n++
			case h.Typeflag == tar.TypeReg:
				if err := writeFile(filepath.Join(dir, base), t); err != nil {
					return err
				}
				n++
			}
		}
		for _, l := range links {
			if err := copyFile(l[0], l[1]); err != nil {
				return err
			}
		}
	}
	Logf("ONNX Runtime %s: %d files in %s", RuntimeVersion, n, dir)
	return nil
}

// bundleModel downloads the vision and text models of repo (in the precision of suffix), the tokenizer and the
// image settings into out, under the names the server looks for.
func bundleModel(ctx context.Context, repo, suffix, out string) error {
	hashes, err := repoHashes(ctx, repo)
	if err != nil {
		return err
	}
	files := map[string]string{ // in the repository → in out
		"tokenizer.model":          "tokenizer.model",
		"preprocessor_config.json": "preprocessor_config.json",
	}
	for _, part := range []string{"vision_model", "text_model"} {
		files["onnx/"+part+suffix+".onnx"] = part + ".onnx"
		// The full-precision large model keeps its weights beside the graph, under the name the graph gives.
		if _, ok := hashes["onnx/"+part+suffix+".onnx_data"]; ok {
			files["onnx/"+part+suffix+".onnx_data"] = part + suffix + ".onnx_data"
		}
	}
	// Phones that search themselves may take the 8-bit text half instead, a quarter of the size.
	if suffix == "" {
		files["onnx/text_model_quantized.onnx"] = search.PhoneTextModelQ8
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for from, to := range files {
		hash, ok := hashes[from]
		if !ok {
			return fmt.Errorf("%s has no %s", repo, from)
		}
		if err := download(ctx, "https://huggingface.co/"+repo+"/resolve/main/"+from, filepath.Join(out, to), hash); err != nil {
			return err
		}
	}
	Logf("%s in %s", repo, out)
	return nil
}

// bundleFiles downloads files of the Hugging Face repository repo (from → to) into out, checked against the
// repository's hashes.
func bundleFiles(ctx context.Context, repo, out string, files map[string]string) error {
	hashes, err := repoHashes(ctx, repo)
	if err != nil {
		return err
	}
	for from, to := range files {
		hash, ok := hashes[from]
		if !ok {
			return fmt.Errorf("%s has no %s", repo, from)
		}
		if err := download(ctx, "https://huggingface.co/"+repo+"/resolve/main/"+from, filepath.Join(out, to), hash); err != nil {
			return err
		}
	}
	Logf("%s in %s", repo, out)
	return nil
}

// writeLAIONHead reads LAION's aesthetic head (a PyTorch archive of five linear layers: 768→1024→128→64→16→1,
// stored in that order as archive/data/0 to 9, weights then biases, row by row) and writes it as one layer:
// {"weights": [768 numbers], "bias": b}, so the server needs no PyTorch.
func writeLAIONHead(pth, out string) error {
	z, err := zip.OpenReader(pth)
	if err != nil {
		return err
	}
	defer z.Close()
	tensors := map[string][]float64{}
	for _, f := range z.File {
		dir, name := path.Split(f.Name)
		if !strings.HasSuffix(dir, "/data/") {
			continue
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			return err
		}
		values := make([]float64, len(raw)/4)
		for i := range values {
			values[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:])))
		}
		tensors[name] = values
	}
	sizes := [][2]int{{1024, 768}, {128, 1024}, {64, 128}, {16, 64}, {1, 16}} // out, in
	// Start from the last layer and multiply backwards: w = W4·W3·W2·W1·W0, bias summed through the same.
	w, bias := []float64{1}, 0.0 // a 1-wide row
	for layer := len(sizes) - 1; layer >= 0; layer-- {
		rows, cols := sizes[layer][0], sizes[layer][1]
		weight, b := tensors[fmt.Sprint(2*layer)], tensors[fmt.Sprint(2*layer+1)]
		if len(weight) != rows*cols || len(b) != rows || len(w) != rows {
			return fmt.Errorf("%s: layer %d is not %d×%d", pth, layer, rows, cols)
		}
		for r := range rows {
			bias += w[r] * b[r]
		}
		next := make([]float64, cols)
		for r := range rows {
			for c := range cols {
				next[c] += w[r] * weight[r*cols+c]
			}
		}
		w = next
	}
	data, err := json.Marshal(struct {
		Weights []float64 `json:"weights"`
		Bias    float64   `json:"bias"`
	}{w, bias})
	if err != nil {
		return err
	}
	return os.WriteFile(out, data, 0o644)
}

// repoHashes is the files of the Hugging Face repository repo, with the sha256 of each kept in LFS ("" for the
// others).
func repoHashes(ctx context.Context, repo string) (map[string]string, error) {
	var info struct {
		Siblings []struct {
			Name string `json:"rfilename"`
			LFS  *struct{ Sha256 string }
		}
	}
	if err := getJSON(ctx, "https://huggingface.co/api/models/"+repo+"?blobs=true", &info); err != nil {
		return nil, err
	}
	hashes := map[string]string{}
	for _, s := range info.Siblings {
		hashes[s.Name] = ""
		if s.LFS != nil {
			hashes[s.Name] = s.LFS.Sha256
		}
	}
	return hashes, nil
}
