package bundle

import (
	"bufio"
	"context"
	"debug/pe"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	msys2Repo   = "https://repo.msys2.org/mingw/ucrt64/"
	msys2Prefix = "mingw-w64-ucrt-x86_64-"
)

// Programs and plugins to bundle, relative to the MSYS2 ucrt64 folder; DLLs they load come along.
var vipsWanted = []string{
	"bin/vips.exe",
	"bin/vipsheader.exe",
	"lib/vips-modules-*/vips-heif.dll",
	// JPEG XL: the raw data of DNG 1.7 files (Samsung Expert RAW and Pro mode), see media/develop.go.
	"lib/vips-modules-*/vips-jxl.dll",
	"lib/libheif/plugins/*.dll",
}

type msys2Package struct {
	name, file, sha256 string
	depends, provides  []string
}

// Vips puts a portable libvips for Windows in dir (replaced), with libjxl beside it (the server loads libjxl.dll
// from vips/bin to keep JPEG photos as JPEG XL). The official Windows builds of libvips leave out the HEVC
// decoder, so they can't read iPhone HEIC photos; MSYS2's builds include it. This fetches MSYS2's packages straight
// from its repository (no MSYS2 install needed) and copies only the programs, modules and DLLs they load.
func Vips(ctx context.Context, dir string) error {
	cache, err := cacheDir("msys2")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}

	pkgs, err := readMSYS2Database(ctx, cache)
	if err != nil {
		return err
	}
	names, err := resolveMSYS2(pkgs, "libvips", "libheif", "libde265", "libjxl")
	if err != nil {
		return err
	}
	Logf("%d packages", len(names))

	staging := filepath.Join(cache, "staging")
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		p := pkgs[name]
		archive := filepath.Join(cache, "packages", p.file)
		if err := download(ctx, msys2Repo+p.file, archive, p.sha256); err != nil {
			return err
		}
		// Some packages only pull in others and have no ucrt64 folder; their .PKGINFO lands beside it.
		if err := extractTar(archive, staging); err != nil {
			return err
		}
	}
	root := filepath.Join(staging, "ucrt64")

	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	var queue []string
	for _, pattern := range vipsWanted {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			return err
		}
		if len(matches) == 0 && !strings.Contains(pattern, "plugins") {
			return fmt.Errorf("%s is not in the packages", pattern)
		}
		queue = append(queue, matches...)
	}

	// Copy each file and follow its imports to the DLLs in ucrt64/bin; Windows' own DLLs aren't there.
	bin := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(root, "bin"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		bin[strings.ToLower(e.Name())] = filepath.Join(root, "bin", e.Name())
	}
	// The server loads libjxl.dll by name (media/jxllib_windows.go); vips-jxl.dll brings it too, but this
	// doesn't depend on that.
	if dll, ok := bin["libjxl.dll"]; ok {
		queue = append(queue, dll)
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
		if err != nil {
			return err
		}
		if err := copyFile(file, filepath.Join(dir, rel)); err != nil {
			return err
		}
		libs, err := peImports(file)
		if err != nil {
			return err
		}
		for _, lib := range libs {
			if dll, ok := bin[strings.ToLower(lib)]; ok {
				queue = append(queue, dll)
			}
		}
	}
	Logf("libvips: %d files in %s", len(copied), dir)
	return nil
}

// readMSYS2Database downloads the repository's package list and reads each package's desc.
func readMSYS2Database(ctx context.Context, cache string) (map[string]*msys2Package, error) {
	db := filepath.Join(cache, "ucrt64.db")
	// The list changes as packages are updated, so it is fetched every time.
	os.Remove(db)
	if err := download(ctx, msys2Repo+"ucrt64.db", db, ""); err != nil {
		return nil, err
	}
	dir := filepath.Join(cache, "db")
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := extractTar(db, dir); err != nil {
		return nil, err
	}

	pkgs := map[string]*msys2Package{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		f, err := os.Open(filepath.Join(dir, e.Name(), "desc"))
		if err != nil {
			continue
		}
		p := &msys2Package{}
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
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		pkgs[p.name] = p
	}
	for _, p := range pkgs {
		for _, name := range p.provides {
			if _, ok := pkgs[name]; !ok {
				pkgs[name] = p
			}
		}
	}
	return pkgs, nil
}

// bareName drops a version constraint: "foo>=1.2" is "foo".
func bareName(dep string) string {
	if i := strings.IndexAny(dep, "<>="); i >= 0 {
		return dep[:i]
	}
	return dep
}

// resolveMSYS2 returns the packages the roots need, each once.
func resolveMSYS2(pkgs map[string]*msys2Package, roots ...string) ([]string, error) {
	seen := map[string]bool{}
	var order []string
	var visit func(name string) error
	visit = func(name string) error {
		p, ok := pkgs[name]
		if !ok {
			return fmt.Errorf("package %s is not in the repository", name)
		}
		if seen[p.name] {
			return nil
		}
		seen[p.name] = true
		order = append(order, p.name)
		for _, dep := range p.depends {
			if err := visit(dep); err != nil {
				return err
			}
		}
		return nil
	}
	for _, root := range roots {
		if err := visit(msys2Prefix + root); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func peImports(path string) ([]string, error) {
	f, err := pe.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// ImportedLibraries isn't implemented for PE files; each imported symbol is "name:library".
	symbols, err := f.ImportedSymbols()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var libs []string
	for _, s := range symbols {
		if _, lib, ok := strings.Cut(s, ":"); ok && !seen[lib] {
			seen[lib] = true
			libs = append(libs, lib)
		}
	}
	return libs, nil
}
