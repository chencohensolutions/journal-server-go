// Command bundle-vips puts a portable libvips for Windows, with libjxl, in ./vips (run from the server folder:
// go run ./scripts/bundle-vips). The official Windows builds of libvips leave out the HEVC decoder, so they can't
// read iPhone HEIC photos; MSYS2's builds include it. bundle.Vips fetches MSYS2's packages straight from its
// repository (no MSYS2 install needed) and copies only the programs, modules and DLLs they load; journal-server
// setup does the same on Windows.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/journal/server-go/bundle"
)

func main() {
	out := flag.String("out", "vips", "folder to write the bundle to (replaced)")
	flag.Parse()
	if err := bundle.Vips(context.Background(), *out); err != nil {
		log.Fatal(fmt.Errorf("bundle-vips: %w", err))
	}
}
