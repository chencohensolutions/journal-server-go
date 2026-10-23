// Command local-fixtures writes what the apps test their own search against, when they keep the indexes and search
// on the phone (docs/search.md "On the phone"): the server's answers to made-up cases, as JSON files in --out.
//
//		go run ./scripts/local-fixtures --out ../journal-client-kmp/sharedLogic/src/commonTest/resources/local-fixtures
//
//	  - tokens.json: how SigLIP 2's text half reads queries (lowercased, SentencePiece, at most 63 pieces, <eos>,
//	    then <pad> to 64), with each query's vector from the full text half and from the phone's 8-bit one;
//	  - ocr.json: words as text in photos splits them, which words answer which, and Find's order over a few texts;
//	  - groups.json: how search hits are grouped into sets and moments;
//	  - thresholds.json: the numbers the indexes decide by.
//
// Everything is made up here: nothing is read from a data folder, so the files can be committed. tokens.json needs
// the search model (go run ./scripts/bundle-onnx) and is left out without it; the 8-bit vectors likewise.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/faces"
	"github.com/journal/server-go/ocr"
	"github.com/journal/server-go/onnx"
	"github.com/journal/server-go/search"
	"github.com/journal/server-go/similar"
)

// The queries tokens.json reads: English, Hebrew, mixed, case, signs, and one longer than the model takes.
var queries = []string{
	"a dog on the beach at sunset",
	"A Dog On The Beach",
	"קבלה מהסופר",
	"ילדים בים בשקיעה",
	"birthday cake עם נרות",
	"2024 receipt #4521, total: ₪89.90",
	"",
	strings.Repeat("a very long description ", 20),
}

func main() {
	out := flag.String("out", "", "folder to write the files into")
	model := flag.String("model", auth.DefaultSearchModel, "the search model's folder")
	runtimePath := flag.String("onnxruntime", "", "ONNX Runtime's library (found as the server finds it when empty)")
	flag.Parse()
	if *out == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	check(os.MkdirAll(*out, 0o755))
	write(*out, "ocr.json", ocrCases())
	write(*out, "groups.json", groupCases())
	write(*out, "thresholds.json", map[string]any{
		"search": map[string]any{"setSimilarity": search.SetSimilarity, "setWindowSeconds": search.SetWindow.Seconds(),
			"momentWindowSeconds": search.MomentWindow.Seconds()},
		"similar": map[string]any{"duplicateSimilarity": similar.DuplicateSimilarity},
		"faces":   map[string]any{"minGroup": faces.MinGroup, "likelySamePerson": 0.36},
	})
	if tokens, ok := tokenCases(*model, *runtimePath); ok {
		write(*out, "tokens.json", tokens)
	}
}

type tokenCase struct {
	Text   string    `json:"text"`
	Tokens []int64   `json:"tokens"`
	FP32   []float32 `json:"fp32"`
	Q8     []float32 `json:"q8,omitempty"`
}

func tokenCases(folder, runtimePath string) (map[string]any, bool) {
	path, err := onnx.FindRuntime(runtimePath)
	if err != nil {
		log.Printf("tokens.json left out: %v", err)
		return nil, false
	}
	rt, err := onnx.LoadRuntime(path)
	if err != nil {
		log.Printf("tokens.json left out: %v", err)
		return nil, false
	}
	full, err := search.LoadTextModel(rt, folder, 4)
	if err != nil {
		log.Printf("tokens.json left out: %s: %v", folder, err)
		return nil, false
	}
	defer full.Close()
	phone, err := search.LoadPhoneTextModel(rt, folder, 4)
	if err != nil {
		log.Printf("tokens.json without 8-bit vectors: %v", err)
		phone = nil
	} else {
		defer phone.Close()
	}
	var cases []tokenCase
	for _, q := range queries {
		c := tokenCase{Text: q, Tokens: full.Tokens(q)}
		c.FP32, err = full.TextVector(q)
		check(err)
		if phone != nil {
			c.Q8, err = phone.TextVector(q)
			check(err)
		}
		cases = append(cases, c)
	}
	return map[string]any{"model": full.Name, "length": len(cases[0].Tokens), "cases": cases}, true
}

func ocrCases() map[string]any {
	texts := []string{
		"קַבָּלָה מס׳ 4521", "בקבלה הזאת", "Total: 89.90 ₪", "RECEIPT", "שלום-עולם", "מקום מלא", "לחם וחלב",
	}
	var words []map[string]any
	for _, t := range texts {
		words = append(words, map[string]any{"text": t, "words": ocr.Tokenize(t)})
	}
	pairs := [][2]string{
		{"קבלה", "קבלה"}, {"בקבלה", "קבלה"}, {"ובקבלה", "קבלה"}, {"שבבקבלה", "קבלה"}, {"בקבלות", "קבלה"},
		{"receipt", "rec"}, {"receipt", "re"}, {"breceipt", "receipt"}, {"מקומ", "מקו"}, {"הלחמ", "לחמ"},
		{"4521", "452"}, {"4521", "45"}, {"total", "total"},
	}
	var matches []map[string]any
	for _, p := range pairs {
		matches = append(matches, map[string]any{"word": p[0], "query": p[1], "matches": ocr.Matches(p[0], p[1])})
	}
	files := map[string][]ocr.Line{
		sha('a'): {{Text: "קבלה מס׳ 4521"}, {Text: "סה״כ 89.90"}},
		sha('b'): {{Text: "בקבלה הזאת"}, {Text: "קבלה קבלה"}},
		sha('c'): {{Text: "RECEIPT total 12"}},
		sha('d'): {{Text: "לחם וחלב"}},
	}
	var finds []map[string]any
	for _, q := range []string{"קבלה", "קבלה 4521", "receipt", "rec", "חלב", "nothing", "וחלב"} {
		finds = append(finds, map[string]any{"query": q, "found": orEmpty(ocr.FindIn(files, q))})
	}
	return map[string]any{"tokenize": words, "matches": matches, "texts": files, "find": finds}
}

func groupCases() map[string]any {
	at := func(minutes float64) *time.Time {
		t := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(minutes * float64(time.Minute)))
		return &t
	}
	type hit struct {
		Sha256  string     `json:"sha256"`
		Vector  []float32  `json:"vector,omitempty"`
		TakenAt *time.Time `json:"takenAt,omitempty"`
	}
	hits := []hit{
		{sha('1'), []float32{1, 0}, at(0)},
		{sha('2'), []float32{0.99, 0.141}, at(2)},  // like the first, soon after: the same set
		{sha('3'), []float32{0.99, 0.141}, at(30)}, // alike, but half an hour later: another set, same moment
		{sha('4'), []float32{0, 1}, at(5)},         // another look: another set, same moment
		{sha('5'), []float32{1, 0}, at(300)},       // hours later: another moment
		{sha('6'), []float32{1, 0}, nil},           // no time: its own
		{sha('7'), nil, at(1)},                     // no vector: its own set
	}
	searchHits := make([]search.Hit, len(hits))
	byS := map[string]hit{}
	for i, h := range hits {
		searchHits[i] = search.Hit{Sha256: h.Sha256}
		byS[h.Sha256] = h
	}
	sets, moments := search.Groups(searchHits, func(s string) []float32 { return byS[s].Vector },
		func(s string) *time.Time { return byS[s].TakenAt })
	return map[string]any{"hits": hits, "sets": sets, "moments": moments}
}

func sha(c byte) string { return strings.Repeat(string(c), 64) }

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func write(dir, name string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	check(err)
	check(os.WriteFile(filepath.Join(dir, name), append(data, '\n'), 0o644))
	log.Printf("Wrote %s", filepath.Join(dir, name))
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
