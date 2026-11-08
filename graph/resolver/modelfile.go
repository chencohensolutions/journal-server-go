package resolver

import (
	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/localindex"
)

// modelFileOf is a model file as the schema gives it, with its source only when the server knows one.
func modelFileOf(f localindex.ModelFile) *model.ModelFile {
	out := &model.ModelFile{Name: f.Name, Precision: f.Precision, Bytes: f.Bytes, Sha256: f.Sha256, URL: f.URL}
	if f.Source != "" {
		source := f.Source
		out.Source = &source
	}
	return out
}
