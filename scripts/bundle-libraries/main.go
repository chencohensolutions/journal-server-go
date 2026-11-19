// Command bundle-libraries puts what the Windows installer ships beside journal-server.exe into a folder: ONNX
// Runtime, libvips and ffmpeg, and libraries.json, which tells journal-server setup to keep them (bundle.Libraries).
// The release runs it on its Windows runner before Inno Setup (go run ./scripts/bundle-libraries --out dist\installer).
package main

import (
	"context"
	"flag"
	"log"

	"github.com/journal/server-go/bundle"
)

func main() {
	out := flag.String("out", "dist/installer", "folder the installer is made from")
	flag.Parse()
	if err := bundle.Libraries(context.Background(), *out); err != nil {
		log.Fatalf("bundle-libraries: %v", err)
	}
}
