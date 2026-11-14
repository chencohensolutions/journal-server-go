package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/bundle"
	"github.com/journal/server-go/certs"
)

const setupUsage = `usage:
  journal-server setup --dir <folder> --move-from <old server folder> [options]
      a server moved from another PC: copies its config.json, certificates and devices.json (only reads it)
  journal-server setup --dir <folder> --user <name> --data-folder <folder> [--import-ca <folder>] [options]
      a new server with its first user and its own new CA; --import-ca uses ca.pem and ca-key.pem from another server

Run it again at any time: what is there is kept, and only what is missing is made or downloaded.

options:
`

// setupOptions is what journal-server setup was asked to do (docs/install.md).
type setupOptions struct {
	dir               string
	moveFrom          string
	user, dataFolder  string
	importCA          string
	newCA             bool
	addresses         []string
	port              string
	reissueServerCert bool
	model, precision  string
	faces, nudity     bool
	similar, ocr      bool
	aesthetic         bool
	downloads         bool
	modelGiven        bool
}

// runSetup makes or completes a server folder: config.json, the certificates, and the libraries and models the
// server downloads. It writes only inside --dir, besides making a new user's data folder when there is none: an
// existing data folder, the folder moved from, and files already in --dir are never changed.
func runSetup(args []string) error {
	o, err := parseSetupArgs(args)
	if err != nil {
		return err
	}
	return setup(context.Background(), o)
}

func parseSetupArgs(args []string) (setupOptions, error) {
	o := setupOptions{}
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), setupUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.dir, "dir", ".", "the server's folder (made if missing)")
	fs.StringVar(&o.moveFrom, "move-from", "", "the folder of the server this one replaces")
	fs.StringVar(&o.user, "user", "", "a new server's first user")
	fs.StringVar(&o.dataFolder, "data-folder", "", "the first user's data folder (made if missing; an existing one is used as it is)")
	fs.StringVar(&o.importCA, "import-ca", "", "folder with ca.pem and ca-key.pem: another server's CA, to keep its devices (default: a new CA)")
	fs.BoolVar(&o.newCA, "new-ca", false, "make a new CA (the default when there is none and no --import-ca)")
	fs.Func("address", "a name or IP address devices reach the server by, beside this PC's own (repeatable; the first is public_url)", func(s string) error {
		o.addresses = append(o.addresses, s)
		return nil
	})
	fs.StringVar(&o.port, "port", "", "port devices connect to (default "+auth.DefaultPort+")")
	fs.BoolVar(&o.reissueServerCert, "reissue-server-cert", false, "make the server certificate again for this PC's addresses (the old one is kept as .bak)")
	fs.StringVar(&o.model, "model", "base", "text search model: base, or large (finds better, about 4× slower to read photos)")
	fs.StringVar(&o.precision, "precision", "fp32", "fp32, or q8 for the smaller 8-bit models")
	noFaces := fs.Bool("no-faces", false, "leave out face search")
	noNudity := fs.Bool("no-nudity", false, "leave out the nudity scan")
	noSimilar := fs.Bool("no-similar", false, "leave out similar photos and duplicates")
	noOCR := fs.Bool("no-ocr", false, "leave out text in photos")
	noAesthetic := fs.Bool("no-aesthetic", false, "leave out aesthetic scores")
	noDownloads := fs.Bool("no-downloads", false, "make the config and certificates only")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, fmt.Errorf("unexpected %q (journal-server setup --help)", fs.Arg(0))
	}
	fs.Visit(func(f *flag.Flag) { o.modelGiven = o.modelGiven || f.Name == "model" || f.Name == "precision" })
	o.faces, o.nudity, o.similar, o.ocr, o.aesthetic = !*noFaces, !*noNudity, !*noSimilar, !*noOCR, !*noAesthetic
	o.downloads = !*noDownloads

	switch {
	case o.moveFrom != "" && (o.user != "" || o.dataFolder != "" || o.newCA || o.importCA != ""):
		return o, errors.New("--move-from takes the users and the CA from the old server; leave out --user, --data-folder, --import-ca and --new-ca")
	case o.importCA != "" && o.newCA:
		return o, errors.New("--import-ca or --new-ca, not both")
	}
	// Folders given relative to where setup was started, before it changes to --dir.
	for _, p := range []*string{&o.dir, &o.moveFrom, &o.dataFolder, &o.importCA} {
		if *p != "" {
			abs, err := filepath.Abs(*p)
			if err != nil {
				return o, err
			}
			*p = abs
		}
	}
	return o, nil
}

func setup(ctx context.Context, o setupOptions) error {
	if o.moveFrom != "" && samePath(o.moveFrom, o.dir) {
		return errors.New("--move-from is the folder being set up; give the new server its own --dir")
	}
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return err
	}
	if err := os.Chdir(o.dir); err != nil {
		return err
	}
	fmt.Printf("Setting up the server in %s\n", o.dir)

	// 1. config.json
	switch _, err := os.Stat(configPath); {
	case err == nil:
		fmt.Printf("config.json: kept\n")
	case o.moveFrom != "":
		if err := moveConfig(o.moveFrom); err != nil {
			return err
		}
	default:
		if err := writeNewConfig(o); err != nil {
			return err
		}
	}
	config, err := auth.LoadConfig(configPath)
	if err != nil {
		return err
	}
	for _, user := range config.Users {
		if _, err := os.Stat(user.DataFolder); err != nil {
			fmt.Printf("  ! %s's data folder %s isn't here: move it there, or change data_folder in config.json\n", user.Username, user.DataFolder)
		}
	}

	// 2. Certificates
	if err := setupCertificates(o, config); err != nil {
		return err
	}

	// 3. Downloads
	if o.downloads {
		if err := setupDownloads(ctx, o, config); err != nil {
			return fmt.Errorf("download: %w (run setup again to go on from here)", err)
		}
	}

	// 4. What's next
	fmt.Printf("\nReady. Devices connect to %s (port %s).\n", orNone(config.PublicURL), config.Port)
	if len(config.Users) > 0 {
		fmt.Printf("Once the server runs, enroll a phone with: journal-server enroll-code %s\n", config.Users[0].Username)
	}
	return nil
}

// moveConfig copies the old server's config.json, the certificates it names and its device registry into the
// current folder. The old folder is only read; admin.token isn't copied (the server writes a new one at start).
func moveConfig(from string) error {
	old, err := auth.LoadConfig(filepath.Join(from, configPath))
	if err != nil {
		return err
	}
	if err := copyNew(filepath.Join(from, configPath), configPath); err != nil {
		return err
	}
	fmt.Printf("config.json: copied from %s\n", from)
	files := []string{old.TLSCert, old.TLSKey, old.ClientCA, old.ClientCAKey, old.DevicesFile}
	for _, name := range files {
		if name == "" {
			continue
		}
		if filepath.IsAbs(name) {
			fmt.Printf("  ! %s is a full path in config.json: copy it to this PC yourself if it isn't here\n", name)
			continue
		}
		switch err := copyNew(filepath.Join(from, name), name); {
		case err == nil:
			fmt.Printf("%s: copied\n", filepath.ToSlash(name))
		case errors.Is(err, os.ErrExist):
			fmt.Printf("%s: kept\n", filepath.ToSlash(name))
		case errors.Is(err, os.ErrNotExist) && name == old.DevicesFile:
			// A server nobody has enrolled with yet has no registry.
		default:
			return err
		}
	}
	return nil
}

// newConfig is the config.json setup writes: only what it sets, so the server's defaults apply to the rest.
type newConfig struct {
	TLSCert     string    `json:"tls_cert"`
	TLSKey      string    `json:"tls_key"`
	ClientCA    string    `json:"client_ca"`
	ClientCAKey string    `json:"client_ca_key"`
	PublicURL   string    `json:"public_url,omitempty"`
	Port        string    `json:"port,omitempty"`
	SearchModel string    `json:"search_model,omitempty"`
	Users       []newUser `json:"users"`
}

type newUser struct {
	Username   string `json:"username"`
	DataFolder string `json:"data_folder"`
}

func writeNewConfig(o setupOptions) error {
	if o.user == "" || o.dataFolder == "" {
		return errors.New("a new server needs --user and --data-folder (or --move-from an old server)")
	}
	if strings.ContainsAny(o.user, `/\:*?"<>| `) {
		return fmt.Errorf("user name %q: use letters and digits only", o.user)
	}
	port := o.port
	if port == "" {
		port = auth.DefaultPort
	}
	config := newConfig{
		TLSCert: "certs/server.pem", TLSKey: "certs/server-key.pem",
		ClientCA: "certs/ca.pem", ClientCAKey: "certs/ca-key.pem",
		Users: []newUser{{o.user, filepath.ToSlash(o.dataFolder)}},
	}
	if o.port != "" && o.port != auth.DefaultPort {
		config.Port = o.port
	}
	if address := firstAddress(o.addresses); address != "" {
		config.PublicURL = "https://" + address + ":" + port
	}
	if folder := searchModelFolder(o.model, o.precision); folder != auth.DefaultSearchModel {
		config.SearchModel = folder
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := writeNew(configPath, append(data, '\n')); err != nil {
		return err
	}
	fmt.Printf("config.json: written for %s\n", o.user)
	// A data folder that is there is used as it is: setup writes nothing into it.
	if _, err := os.Stat(o.dataFolder); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(o.dataFolder, 0o755); err != nil {
			return err
		}
		fmt.Printf("Data folder %s: made\n", o.dataFolder)
	}
	return nil
}

func setupCertificates(o setupOptions, config auth.AppConfig) error {
	var ca certs.CA
	var err error
	switch {
	case exists(config.ClientCA) && exists(config.ClientCAKey):
		if ca, err = certs.LoadCA(config.ClientCA, config.ClientCAKey); err != nil {
			return err
		}
		fmt.Printf("CA: kept\n")
	case o.importCA != "":
		if ca, err = certs.LoadCA(filepath.Join(o.importCA, "ca.pem"), filepath.Join(o.importCA, "ca-key.pem")); err != nil {
			return err
		}
		for from, to := range map[string]string{"ca.pem": config.ClientCA, "ca-key.pem": config.ClientCAKey} {
			if err := copyNew(filepath.Join(o.importCA, from), to); err != nil {
				return err
			}
		}
		fmt.Printf("CA: copied from %s\n", o.importCA)
	case o.moveFrom != "" || (o.user == "" && !o.newCA):
		// A server that has devices keeps the CA they trust: a new one would lock them all out.
		return fmt.Errorf("no CA at %s: put back ca.pem and ca-key.pem, or give --import-ca <folder> (--new-ca makes "+
			"a new one, and every device must enroll again)", config.ClientCA)
	default:
		// Each server has its own CA: the apps learn it when they enroll by the QR code (docs/install.md).
		if ca, err = certs.NewCA(config.ClientCA, config.ClientCAKey); err != nil {
			return err
		}
		fmt.Printf("CA: made this server's own.\n" +
			"  Phones enroll by scanning the QR code journal-server enroll-code prints, which tells them this\n" +
			"  server's key; they keep its CA from then on. Keep certs/ca-key.pem private and backed up.\n")
	}

	// The names devices may use: this PC's own, and the ones asked for.
	names := certs.LocalNames()
	for _, a := range o.addresses {
		if !slices.Contains(names, a) {
			names = append(names, a)
		}
	}
	if exists(config.TLSCert) && exists(config.TLSKey) {
		missing, err := certs.Missing(config.TLSCert, append(certs.PrivateAddresses(), o.addresses...))
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			fmt.Printf("Server certificate: kept\n")
			return nil
		}
		if !o.reissueServerCert {
			fmt.Printf("Server certificate: kept, but it isn't valid for %s.\n"+
				"  ! Devices connecting by that address will refuse the server. Run setup again with\n"+
				"    --reissue-server-cert, or give this PC the old server's address.\n", strings.Join(missing, ", "))
			return nil
		}
		for _, p := range []string{config.TLSCert, config.TLSKey} {
			if err := keepAsBackup(p); err != nil {
				return err
			}
		}
	}
	if err := certs.NewServerCert(ca, config.TLSCert, config.TLSKey, names); err != nil {
		return err
	}
	fmt.Printf("Server certificate: made for %s\n", strings.Join(names, ", "))
	return nil
}

func setupDownloads(ctx context.Context, o setupOptions, config auth.AppConfig) error {
	model, precision := o.model, o.precision
	if !o.modelGiven {
		// A moved server keeps the search model it has: its vectors were made with that one.
		model, precision = modelOfFolder(config.SearchModel)
	}
	if _, err := bundle.ONNX(ctx, bundle.ONNXOptions{
		Model: model, Precision: precision,
		Faces: o.faces, Nudity: o.nudity, Similar: o.similar, OCR: o.ocr, Aesthetic: o.aesthetic,
	}); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if err := bundle.Vips(ctx, "vips"); err != nil {
			return err
		}
		if err := bundle.FFmpeg(ctx, "ffmpeg"); err != nil {
			return err
		}
	} else {
		fmt.Printf("libvips and ffmpeg: install them with your package manager (brew install vips ffmpeg)\n")
	}
	for setting, value := range map[string]string{"vips": config.Vips, "ffmpeg": config.FFmpeg, "onnxruntime": config.OnnxRuntime} {
		if value != "" {
			fmt.Printf("  ! config.json sets %q to %s, so the server uses that, not the one downloaded here\n", setting, value)
		}
	}
	return nil
}

// searchModelFolder is where bundle.ONNX puts the search model of that size and precision.
func searchModelFolder(model, precision string) string {
	folder := bundle.SearchModels[model]
	if precision != "" && precision != "fp32" {
		folder += "-" + precision
	}
	return "models/" + folder
}

// modelOfFolder is the size and precision of the search model in folder (config.json's search_model), or the
// default when it isn't one bundle.ONNX makes.
func modelOfFolder(folder string) (model, precision string) {
	for _, m := range []string{"base", "large"} {
		for _, p := range []string{"fp32", "q8"} {
			if filepath.ToSlash(filepath.Clean(folder)) == searchModelFolder(m, p) {
				return m, p
			}
		}
	}
	return "base", "fp32"
}

// firstAddress is the address public_url names: the first one asked for, else this PC's first private address.
func firstAddress(asked []string) string {
	if len(asked) > 0 {
		return asked[0]
	}
	if addrs := certs.PrivateAddresses(); len(addrs) > 0 {
		return addrs[0]
	}
	return ""
}

// copyNew copies a file to a path that must not exist yet (os.ErrExist if it does); from is only read.
func copyNew(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(to)
		return err
	}
	return dst.Close()
}

// writeNew writes a file that must not exist yet.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// keepAsBackup renames path to path.bak (or path.<time>.bak when that is taken), so nothing is lost.
func keepAsBackup(path string) error {
	backup := path + ".bak"
	if exists(backup) {
		backup = path + "." + time.Now().Format("20060102-150405") + ".bak"
	}
	if err := os.Rename(path, backup); err != nil {
		return err
	}
	fmt.Printf("%s: kept as %s\n", filepath.ToSlash(path), filepath.ToSlash(backup))
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// samePath reports whether two absolute paths name the same folder (ignoring case on Windows and macOS, whose
// file systems usually do).
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "linux" {
		return a == b
	}
	return strings.EqualFold(a, b)
}

func orNone(s string) string {
	if s == "" {
		return "(no public_url in config.json)"
	}
	return s
}
