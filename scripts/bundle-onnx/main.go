// Command bundle-onnx downloads what text search, face search and the other indexes need (run from the server
// folder: go run ./scripts/bundle-onnx): ONNX Runtime into ./onnxruntime and every index's model into ./models.
// What each is, and where it comes from, is in bundle.ONNX; journal-server setup downloads the same.
//
// --model base (the default, google/siglip2-base-patch16-256) reads a photo in about a tenth of a second on a
// desktop CPU; --model large (google/siglip2-so400m-patch16-256) finds better but is about four times slower.
// --precision q8 downloads the 8-bit versions: a quarter of the size and faster, a little less exact.
// --faces=false, --nudity=false, --similar=false, --ocr=false and --aesthetic=false leave those models out.
// Files already downloaded and intact are kept. Then set "search_model" in config.json to the folder printed at
// the end (models/siglip2-base-patch16-256 is used when "search_model" is empty) and restart the server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/journal/server-go/bundle"
)

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
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}

	out, err := bundle.ONNX(context.Background(), bundle.ONNXOptions{
		RuntimeDir: *runtimeDir, ModelsDir: *modelsDir, Model: *modelFlag, Precision: *precision,
		Faces: *faces, Nudity: *nudity, Similar: *similar, OCR: *ocr, Aesthetic: *aesthetic,
	})
	if err != nil {
		log.Fatal(fmt.Errorf("bundle-onnx: %w", err))
	}
	fmt.Printf("Done. In config.json: \"search_model\": %q\n", filepath.ToSlash(out))
}
