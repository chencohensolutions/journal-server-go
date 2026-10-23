// Command bundle-onnx downloads what text search, face search and the nudity scan need (run from the server folder:
// go run ./scripts/bundle-onnx):
//
//   - ONNX Runtime, the library that runs the models, into ./onnxruntime (from Microsoft's GitHub releases);
//   - a SigLIP 2 model, as ONNX files, into ./models/<name> (from Hugging Face's onnx-community);
//   - the face models, YuNet (finds faces, MIT licence) and SFace (tells them apart, Apache 2.0), into
//     ./models/yunet-sface (from OpenCV's model zoo on Hugging Face). --faces=false leaves them out;
//   - NudeNet's detector (320n.onnx, AGPL-3.0), into ./models/nudenet (from NudeNet's GitHub releases).
//     --nudity=false leaves it out;
//   - DINOv2 small (Meta, Apache 2.0), for similar photos and duplicates, into ./models/dinov2-small (from
//     onnx-community). --similar=false leaves it out;
//   - a Hebrew and English PaddleOCR (a word detector and the PP-OCRv6 word recognizer of Rivok/paddleocr-hebrew,
//     Apache 2.0, whose 120 characters are Hebrew, Latin, digits and signs), for text in photos, into
//     ./models/paddleocr-hebrew. --ocr=false leaves it out;
//   - CLIP ViT-L/14's vision half (OpenAI, MIT; 8-bit, from Xenova) and LAION's aesthetic predictor (the
//     sac+logos+ava1 linear head of christophschuhmann/improved-aesthetic-predictor, Apache 2.0), for aesthetic
//     scores, into ./models/clip-vit-large-patch14-laion. The head's five linear layers (no activation between
//     them) are multiplied out here into one: 768 weights and a bias, kept as laion-head.json.
//     --aesthetic=false leaves them out.
//
// --model base (the default, google/siglip2-base-patch16-256) reads a photo in about a tenth of a second on a
// desktop CPU; --model large (google/siglip2-so400m-patch16-256) finds better but is about four times slower.
// --precision q8 downloads the 8-bit versions: a quarter of the size and faster, a little less exact.
// Files already downloaded and intact are kept. Then set "search_model" in config.json to the folder printed at
// the end (models/siglip2-base-patch16-256 is used when "search_model" is empty) and restart the server.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// The ONNX Runtime release to download; the server works with 1.16 or later.
const runtimeVersion = "1.31.0"

var models = map[string]string{
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

func main() {
	modelFlag := flag.String("model", "base", "base or large")
	precision := flag.String("precision", "fp32", "fp32, or q8 for the smaller 8-bit files")
	runtimeDir := flag.String("runtime", "onnxruntime", "folder for the ONNX Runtime library (replaced)")
	modelsDir := flag.String("models", "models", "folder the model's folder is made in")
	faces := flag.Bool("faces", true, "also download the face models")
	nudity := flag.Bool("nudity", true, "also download NudeNet's detector, for the nudity scan")
	similar := flag.Bool("similar", true, "also download DINOv2 small, for similar photos and duplicates")
	ocr := flag.Bool("ocr", true, "also download the Hebrew and English OCR models")
	aesthetic := flag.Bool("aesthetic", true, "also download CLIP ViT-L/14 and LAION's aesthetic head")
	flag.Parse()
	name, ok := models[*modelFlag]
	suffix, ok2 := precisions[*precision]
	if !ok || !ok2 || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}

	bundleRuntime(*runtimeDir)

	folder := name
	if *precision != "fp32" {
		folder += "-" + *precision
	}
	out := filepath.Join(*modelsDir, folder)
	bundleModel("onnx-community/"+name+"-ONNX", suffix, out)
	if *faces {
		facesOut := filepath.Join(*modelsDir, "yunet-sface")
		for _, m := range faceModels {
			download("https://huggingface.co/"+m[0]+"/resolve/main/"+m[1], filepath.Join(facesOut, m[1]), repoHashes(m[0])[m[1]])
		}
		log.Printf("Face models in %s", facesOut)
	}
	if *nudity {
		var release struct {
			// The API's link to the file: its browser link may answer a web page instead.
			Assets []struct{ Name, URL string }
		}
		getJSON("https://api.github.com/repos/"+nudityRelease, &release)
		to := filepath.Join(*modelsDir, "nudenet", nudityFile)
		for _, a := range release.Assets {
			if a.Name == nudityFile {
				download(a.URL, to, nuditySha256)
			}
		}
		if _, err := os.Stat(to); err != nil {
			log.Fatalf("%s is not in %s", nudityFile, nudityRelease)
		}
		log.Printf("NudeNet's detector in %s", to)
	}
	if *similar {
		bundleFiles("onnx-community/dinov2-small-ONNX", filepath.Join(*modelsDir, "dinov2-small"), map[string]string{
			"onnx/model.onnx": "model.onnx", "preprocessor_config.json": "preprocessor_config.json"})
	}
	if *ocr {
		bundleFiles("Rivok/paddleocr-hebrew", filepath.Join(*modelsDir, "paddleocr-hebrew"), map[string]string{
			"word-det/det.onnx": "det.onnx", "server-v6/rec.onnx": "rec.onnx", "charset_v2f.txt": "charset.txt"})
	}
	if *aesthetic {
		out := filepath.Join(*modelsDir, "clip-vit-large-patch14-laion")
		bundleFiles("Xenova/clip-vit-large-patch14", out, map[string]string{
			"onnx/vision_model_quantized.onnx": "vision_model.onnx", "preprocessor_config.json": "preprocessor_config.json"})
		pth := filepath.Join(out, "sac+logos+ava1-l14-linearMSE.pth")
		download(laionHeadURL, pth, laionHeadSha256)
		check(writeLAIONHead(pth, filepath.Join(out, "laion-head.json")))
		log.Printf("LAION's aesthetic head in %s", filepath.Join(out, "laion-head.json"))
	}
	fmt.Printf("Done. In config.json: \"search_model\": %q\n", filepath.ToSlash(out))
}

// bundleRuntime puts this platform's ONNX Runtime library (and the libraries it loads) in dir.
func bundleRuntime(dir string) {
	var asset string
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		asset = "onnxruntime-win-x64-" + runtimeVersion + ".zip"
	case "windows/arm64":
		asset = "onnxruntime-win-arm64-" + runtimeVersion + ".zip"
	case "darwin/arm64":
		asset = "onnxruntime-osx-arm64-" + runtimeVersion + ".tgz"
	case "linux/amd64":
		asset = "onnxruntime-linux-x64-" + runtimeVersion + ".tgz"
	case "linux/arm64":
		asset = "onnxruntime-linux-aarch64-" + runtimeVersion + ".tgz"
	default:
		log.Fatalf("no ONNX Runtime download for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	var release struct {
		Assets []struct {
			Name, Digest string
			URL          string `json:"browser_download_url"`
		}
	}
	getJSON("https://api.github.com/repos/microsoft/onnxruntime/releases/tags/v"+runtimeVersion, &release)
	cache, err := os.UserCacheDir()
	check(err)
	archive := filepath.Join(cache, "journal-server", "onnxruntime", asset)
	for _, a := range release.Assets {
		if a.Name == asset {
			download(a.URL, archive, strings.TrimPrefix(a.Digest, "sha256:"))
		}
	}
	if _, err := os.Stat(archive); err != nil {
		log.Fatalf("%s is not in ONNX Runtime %s", asset, runtimeVersion)
	}

	check(os.RemoveAll(dir))
	check(os.MkdirAll(dir, 0o755))
	// Only the shared libraries in lib/; headers and the static libraries aren't needed.
	wanted := func(name string) (string, bool) {
		dirName, base := path.Split(name)
		isLib := strings.HasSuffix(base, ".dll") || strings.Contains(base, ".dylib") || strings.Contains(base, ".so")
		return base, strings.HasSuffix(dirName, "/lib/") && isLib
	}
	n := 0
	if strings.HasSuffix(archive, ".zip") {
		z, err := zip.OpenReader(archive)
		check(err)
		defer z.Close()
		for _, f := range z.File {
			if base, ok := wanted(f.Name); ok {
				r, err := f.Open()
				check(err)
				writeFile(filepath.Join(dir, base), r)
				r.Close()
				n++
			}
		}
	} else {
		f, err := os.Open(archive)
		check(err)
		defer f.Close()
		gz, err := gzip.NewReader(f)
		check(err)
		t := tar.NewReader(gz)
		for {
			h, err := t.Next()
			if err == io.EOF {
				break
			}
			check(err)
			base, ok := wanted(h.Name)
			switch {
			case !ok:
			case h.Typeflag == tar.TypeSymlink:
				// libonnxruntime.dylib → libonnxruntime.1.31.0.dylib; a copy works everywhere.
				defer copyFile(filepath.Join(dir, path.Base(h.Linkname)), filepath.Join(dir, base))
				n++
			case h.Typeflag == tar.TypeReg:
				writeFile(filepath.Join(dir, base), t)
				n++
			}
		}
	}
	log.Printf("ONNX Runtime %s: %d files in %s", runtimeVersion, n, dir)
}

// bundleModel downloads the vision and text models of repo (in the precision of suffix), the tokenizer and the
// image settings into out, under the names the server looks for.
func bundleModel(repo, suffix, out string) {
	hashes := repoHashes(repo)
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
	check(os.MkdirAll(out, 0o755))
	for from, to := range files {
		hash, ok := hashes[from]
		if !ok {
			log.Fatalf("%s has no %s", repo, from)
		}
		download("https://huggingface.co/"+repo+"/resolve/main/"+from, filepath.Join(out, to), hash)
	}
	log.Printf("%s in %s", repo, out)
}

// bundleFiles downloads files of the Hugging Face repository repo (from → to) into out, checked against the
// repository's hashes.
func bundleFiles(repo, out string, files map[string]string) {
	hashes := repoHashes(repo)
	for from, to := range files {
		hash, ok := hashes[from]
		if !ok {
			log.Fatalf("%s has no %s", repo, from)
		}
		download("https://huggingface.co/"+repo+"/resolve/main/"+from, filepath.Join(out, to), hash)
	}
	log.Printf("%s in %s", repo, out)
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
func repoHashes(repo string) map[string]string {
	var info struct {
		Siblings []struct {
			Name string `json:"rfilename"`
			LFS  *struct{ Sha256 string }
		}
	}
	getJSON("https://huggingface.co/api/models/"+repo+"?blobs=true", &info)
	hashes := map[string]string{}
	for _, s := range info.Siblings {
		hashes[s.Name] = ""
		if s.LFS != nil {
			hashes[s.Name] = s.LFS.Sha256
		}
	}
	return hashes
}

// download fetches url into path unless a file with sha256 hash is already there; an empty hash is not checked.
func download(url, path, hash string) {
	if hash != "" {
		if sum, err := fileSha256(path); err == nil && sum == hash {
			return
		}
	}
	check(os.MkdirAll(filepath.Dir(path), 0o755))
	log.Printf("Downloading %s", url)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	check(err)
	if strings.HasPrefix(url, "https://api.github.com/") {
		req.Header.Set("Accept", "application/octet-stream") // the file, not its description
	}
	resp, err := http.DefaultClient.Do(req)
	check(err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("%s: %s", url, resp.Status)
	}
	partial := path + ".partial"
	writeFile(partial, resp.Body)
	if hash != "" {
		if sum, err := fileSha256(partial); err != nil || sum != hash {
			os.Remove(partial)
			log.Fatalf("%s: sha256 does not match", url)
		}
	}
	check(os.Rename(partial, path))
}

func getJSON(url string, v any) {
	resp, err := http.Get(url)
	check(err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("%s: %s", url, resp.Status)
	}
	check(json.NewDecoder(resp.Body).Decode(v))
}

func writeFile(path string, r io.Reader) {
	f, err := os.Create(path)
	check(err)
	_, err = io.Copy(f, r)
	check(err)
	check(f.Close())
}

func copyFile(from, to string) {
	f, err := os.Open(from)
	check(err)
	defer f.Close()
	writeFile(to, f)
}

func fileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func check(err error) {
	if err != nil {
		log.Fatal(fmt.Errorf("bundle-onnx: %w", err))
	}
}
