// Command bundle-onnx downloads what text search needs (run from the server folder: go run ./scripts/bundle-onnx):
//
//   - ONNX Runtime, the library that runs the model, into ./onnxruntime (from Microsoft's GitHub releases);
//   - a SigLIP 2 model, as ONNX files, into ./models/<name> (from Hugging Face's onnx-community).
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
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
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

func main() {
	modelFlag := flag.String("model", "base", "base or large")
	precision := flag.String("precision", "fp32", "fp32, or q8 for the smaller 8-bit files")
	runtimeDir := flag.String("runtime", "onnxruntime", "folder for the ONNX Runtime library (replaced)")
	modelsDir := flag.String("models", "models", "folder the model's folder is made in")
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

// download fetches url into path unless a file with sha256 hash is already there; an empty hash is not checked.
func download(url, path, hash string) {
	if hash != "" {
		if sum, err := fileSha256(path); err == nil && sum == hash {
			return
		}
	}
	check(os.MkdirAll(filepath.Dir(path), 0o755))
	log.Printf("Downloading %s", url)
	resp, err := http.Get(url)
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
