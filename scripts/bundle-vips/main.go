// Command bundle-vips puts a portable libvips for Windows in ./vips (run from the server folder:
// go run ./scripts/bundle-vips). The official Windows builds of libvips leave out the HEVC decoder, so
// they can't read iPhone HEIC photos; MSYS2's builds include it. This fetches MSYS2's packages straight
// from its repository (no MSYS2 install needed) and copies only the programs, modules and DLLs they load.
package main

import (
	"bufio"
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	repo   = "https://repo.msys2.org/mingw/ucrt64/"
	prefix = "mingw-w64-ucrt-x86_64-"
)

// Programs and plugins to bundle, relative to the MSYS2 ucrt64 folder; DLLs they load come along.
var wanted = []string{
	"bin/vips.exe",
	"bin/vipsheader.exe",
	"lib/vips-modules-*/vips-heif.dll",
	"lib/libheif/plugins/*.dll",
}

type pkg struct {
	name, file, sha256 string
	depends, provides  []string
}

func main() {
	out := flag.String("out", "vips", "folder to write the bundle to (replaced)")
	flag.Parse()

	cache, err := os.UserCacheDir()
	check(err)
	cache = filepath.Join(cache, "journal-server", "msys2")
	check(os.MkdirAll(cache, 0o755))

	pkgs := readDatabase(cache)
	names := resolve(pkgs, "libvips", "libheif", "libde265")
	log.Printf("%d packages", len(names))

	staging := filepath.Join(cache, "staging")
	check(os.RemoveAll(staging))
	check(os.MkdirAll(staging, 0o755))
	for _, name := range names {
		p := pkgs[name]
		archive := download(cache, p)
		// Some packages only pull in others and have no ucrt64 folder; their .PKGINFO lands beside it.
		run("tar", "-xf", archive, "-C", staging)
	}
	root := filepath.Join(staging, "ucrt64")

	check(os.RemoveAll(*out))
	var queue []string
	for _, pattern := range wanted {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		check(err)
		if len(matches) == 0 && !strings.Contains(pattern, "plugins") {
			log.Fatalf("%s is not in the packages", pattern)
		}
		queue = append(queue, matches...)
	}

	// Copy each file and follow its imports to the DLLs in ucrt64/bin; Windows' own DLLs aren't there.
	bin := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(root, "bin"))
	check(err)
	for _, e := range entries {
		bin[strings.ToLower(e.Name())] = filepath.Join(root, "bin", e.Name())
	}
	copied := map[string]bool{}
	for len(queue) > 0 {
		file := queue[0]
		queue = queue[1:]
		if copied[file] {
			continue
		}
		copied[file] = true
		rel, err := filepath.Rel(root, file)
		check(err)
		copyFile(file, filepath.Join(*out, rel))
		for _, lib := range imports(file) {
			if dll, ok := bin[strings.ToLower(lib)]; ok {
				queue = append(queue, dll)
			}
		}
	}
	log.Printf("Wrote %d files to %s", len(copied), *out)
}

// readDatabase downloads the repository's package list and reads each package's desc.
func readDatabase(cache string) map[string]*pkg {
	db := filepath.Join(cache, "ucrt64.db")
	fetch(repo+"ucrt64.db", db)
	dir := filepath.Join(cache, "db")
	check(os.RemoveAll(dir))
	check(os.MkdirAll(dir, 0o755))
	run("tar", "-xf", db, "-C", dir)

	pkgs := map[string]*pkg{}
	entries, err := os.ReadDir(dir)
	check(err)
	for _, e := range entries {
		f, err := os.Open(filepath.Join(dir, e.Name(), "desc"))
		if err != nil {
			continue
		}
		p := &pkg{}
		var field string
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				field = ""
			case strings.HasPrefix(line, "%") && strings.HasSuffix(line, "%"):
				field = line
			case field == "%NAME%":
				p.name = line
			case field == "%FILENAME%":
				p.file = line
			case field == "%SHA256SUM%":
				p.sha256 = line
			case field == "%DEPENDS%":
				p.depends = append(p.depends, bareName(line))
			case field == "%PROVIDES%":
				p.provides = append(p.provides, bareName(line))
			}
		}
		f.Close()
		pkgs[p.name] = p
	}
	for _, p := range pkgs {
		for _, name := range p.provides {
			if _, ok := pkgs[name]; !ok {
				pkgs[name] = p
			}
		}
	}
	return pkgs
}

// bareName drops a version constraint: "foo>=1.2" is "foo".
func bareName(dep string) string {
	if i := strings.IndexAny(dep, "<>="); i >= 0 {
		return dep[:i]
	}
	return dep
}

// resolve returns the packages the roots need, each once.
func resolve(pkgs map[string]*pkg, roots ...string) []string {
	seen := map[string]bool{}
	var order []string
	var visit func(name string)
	visit = func(name string) {
		p, ok := pkgs[name]
		if !ok {
			log.Fatalf("package %s is not in the repository", name)
		}
		if seen[p.name] {
			return
		}
		seen[p.name] = true
		order = append(order, p.name)
		for _, dep := range p.depends {
			visit(dep)
		}
	}
	for _, root := range roots {
		visit(prefix + root)
	}
	return order
}

// download fetches a package once and checks it against the repository's sha256.
func download(cache string, p *pkg) string {
	path := filepath.Join(cache, "packages", p.file)
	if sum, err := fileSha256(path); err == nil && sum == p.sha256 {
		return path
	}
	check(os.MkdirAll(filepath.Dir(path), 0o755))
	log.Printf("Downloading %s", p.file)
	fetch(repo+p.file, path)
	if sum, err := fileSha256(path); err != nil || sum != p.sha256 {
		log.Fatalf("%s: sha256 does not match the repository", p.file)
	}
	return path
}

func fetch(url, path string) {
	resp, err := http.Get(url)
	check(err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("%s: %s", url, resp.Status)
	}
	f, err := os.Create(path)
	check(err)
	_, err = io.Copy(f, resp.Body)
	check(err)
	check(f.Close())
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

func imports(path string) []string {
	f, err := pe.Open(path)
	check(err)
	defer f.Close()
	// ImportedLibraries isn't implemented for PE files; each imported symbol is "name:library".
	symbols, err := f.ImportedSymbols()
	check(err)
	seen := map[string]bool{}
	var libs []string
	for _, s := range symbols {
		if _, lib, ok := strings.Cut(s, ":"); ok && !seen[lib] {
			seen[lib] = true
			libs = append(libs, lib)
		}
	}
	return libs
}

func copyFile(from, to string) {
	check(os.MkdirAll(filepath.Dir(to), 0o755))
	data, err := os.ReadFile(from)
	check(err)
	check(os.WriteFile(to, data, 0o755))
}

func run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("%s %s: %v", name, strings.Join(args, " "), err)
	}
}

func check(err error) {
	if err != nil {
		log.Fatal(fmt.Errorf("bundle-vips: %w", err))
	}
}
