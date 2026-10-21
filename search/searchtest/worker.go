// Package searchtest is a stand-in for the search model in tests, speaking siglip_worker.py's protocol without
// Python: a picture's vector is its average colour, and the words "red", "green" and "blue" are those colours (any
// other words are grey). A batch with a picture whose path has "choke" in it fails as a whole, as a real model may on
// one odd picture. A test runs its own test binary as the worker, with Env set; its TestMain calls Serve.
package searchtest

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"slices"
	"strings"
)

// Env is set to "1" in the environment of a test binary that is to act as the worker.
const Env = "JOURNAL_FAKE_SEARCH_WORKER"

// Serve answers requests on stdin until it closes.
func Serve() {
	out := json.NewEncoder(os.Stdout)
	out.Encode(map[string]any{"ready": true, "dims": 3, "device": "test"})
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var request struct {
			Images []string `json:"images"`
			Text   *string  `json:"text"`
		}
		json.Unmarshal(in.Bytes(), &request)
		if request.Text != nil {
			out.Encode(map[string]any{"vector": encode(words(*request.Text))})
			continue
		}
		if slices.ContainsFunc(request.Images, func(path string) bool { return strings.Contains(path, "choke") }) {
			out.Encode(map[string]any{"error": "the model choked"})
			continue
		}
		vectors, errs := make([]*string, len(request.Images)), make([]*string, len(request.Images))
		for i, path := range request.Images {
			v, err := colour(path)
			if err != nil {
				message := err.Error()
				errs[i] = &message
				continue
			}
			encoded := encode(v)
			vectors[i] = &encoded
		}
		out.Encode(map[string]any{"vectors": vectors, "errors": errs})
	}
}

func words(text string) [3]float64 {
	switch strings.ToLower(text) {
	case "red":
		return [3]float64{1, 0, 0}
	case "green":
		return [3]float64{0, 1, 0}
	case "blue":
		return [3]float64{0, 0, 1}
	}
	return [3]float64{1, 1, 1}
}

func colour(path string) ([3]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return [3]float64{}, err
	}
	defer f.Close()
	picture, _, err := image.Decode(f)
	if err != nil {
		return [3]float64{}, err
	}
	var sum [3]float64
	bounds := picture.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := picture.At(x, y).RGBA()
			sum[0], sum[1], sum[2] = sum[0]+float64(r), sum[1]+float64(g), sum[2]+float64(b)
		}
	}
	return sum, nil
}

// encode makes v unit length, as little-endian float32 in base64.
func encode(v [3]float64) string {
	length := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	raw := make([]byte, 12)
	for i, x := range v {
		binary.LittleEndian.PutUint32(raw[4*i:], math.Float32bits(float32(x/length)))
	}
	return base64.StdEncoding.EncodeToString(raw)
}
