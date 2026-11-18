package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// askedSetup is journal-server setup --ask: the installer's wizard as questions in a console. A silent install (the
// Microsoft Store installs silently) only copies the programs and libraries, so the Start menu's "Set up Journal
// server" runs this afterwards. It asks for administrator rights first (the service needs them), asks what the
// wizard asks unless the folder already has a config.json, runs setup, and installs and starts the service.
func askedSetup(ctx context.Context, o setupOptions, args []string) error {
	in := bufio.NewReader(os.Stdin)
	relaunched := false
	err := func() error {
		elevated, err := relaunchElevated(args)
		if err != nil {
			return fmt.Errorf("setup needs administrator rights to install the service: %w", err)
		}
		if relaunched = elevated; relaunched {
			return nil
		}
		if _, err := os.Stat(filepath.Join(o.dir, configPath)); err == nil {
			fmt.Printf("This server is set up already (%s): checking it, and adding what is missing.\n\n", filepath.Join(o.dir, configPath))
		} else {
			asked, err := askSetupQuestions(in, os.Stdout, o)
			if err != nil {
				return err
			}
			o = asked
			fmt.Println()
		}
		if err := setup(ctx, o); err != nil {
			return err
		}
		if runtime.GOOS != "windows" {
			return nil
		}
		fmt.Println("\nStarting the JournalServer service...")
		if err := runServiceCommand([]string{"install"}); err != nil {
			return err
		}
		if err := runServiceCommand([]string{"start"}); err != nil {
			return fmt.Errorf("the service is installed but didn't start (see server.log): %w", err)
		}
		fmt.Println("The server is running. It starts by itself whenever this PC starts.")
		return nil
	}()
	if relaunched {
		return nil // the administrator's window goes on
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nerror:", err)
		fmt.Fprintln(os.Stderr, "Run \"Set up Journal server\" again to go on from where it stopped.")
	}
	// The console was opened for this: keep it until it has been read.
	fmt.Print("\nPress Enter to close this window.")
	_, _ = in.ReadString('\n')
	if err != nil {
		os.Exit(1)
	}
	return nil
}

// askSetupQuestions asks what the installer's wizard asks (installer/journal-server.iss) and returns o with the
// answers: move an existing server, or a new one with its first user, data folder, CA and search model.
func askSetupQuestions(in *bufio.Reader, out io.Writer, o setupOptions) (setupOptions, error) {
	ask := func(question, def string) (string, error) {
		if def != "" {
			fmt.Fprintf(out, "%s [%s]: ", question, def)
		} else {
			fmt.Fprintf(out, "%s: ", question)
		}
		line, err := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if err != nil && (err != io.EOF || line == "") {
			return "", errors.New("setup was not answered")
		}
		if line == "" {
			line = def
		}
		return line, nil
	}
	choose := func(question string, options ...string) (int, error) {
		fmt.Fprintln(out, question)
		for i, option := range options {
			fmt.Fprintf(out, "  %d. %s\n", i+1, option)
		}
		for {
			answer, err := ask("Choose", "1")
			if err != nil {
				return 0, err
			}
			for i := range options {
				if answer == fmt.Sprint(i+1) {
					return i, nil
				}
			}
			fmt.Fprintf(out, "Type a number from 1 to %d.\n", len(options))
		}
	}
	folderWith := func(question string, files ...string) (string, error) {
		for {
			folder, err := ask(question, "")
			if err != nil {
				return "", err
			}
			folder = strings.Trim(folder, `"`)
			missing := ""
			for _, f := range files {
				if _, err := os.Stat(filepath.Join(folder, f)); err != nil {
					missing = f
					break
				}
			}
			if folder != "" && missing == "" {
				return filepath.Abs(folder)
			}
			fmt.Fprintf(out, "That folder needs %s.\n", strings.Join(files, " and "))
		}
	}

	fmt.Fprintf(out, "Setting up the journal server in %s\n\n", o.dir)
	mode, err := choose("Is this a new server, or does it replace one?",
		"Move my existing server to this PC (copies its config.json, certificates and enrolled devices; the old folder is only read)",
		"Set up a new server")
	if err != nil {
		return o, err
	}
	if mode == 0 {
		o.moveFrom, err = folderWith("\nThe old server's folder (with its config.json and certs folder)", configPath)
		return o, err
	}

	for {
		o.user, err = ask("\nThe first user's name (letters and digits; it is the user's identity in their devices' certificates)", "")
		if err != nil {
			return o, err
		}
		if o.user != "" && !strings.ContainsAny(o.user, `/\:*?"<>| `) {
			break
		}
		fmt.Fprintln(out, "Use letters and digits only.")
	}
	defaultData := filepath.Join(filepath.VolumeName(o.dir)+string(filepath.Separator), "Journal")
	data, err := ask("\nWhere should "+o.user+"'s photos be kept? (an existing journal folder is used as it is)", defaultData)
	if err != nil {
		return o, err
	}
	if o.dataFolder, err = filepath.Abs(strings.Trim(data, `"`)); err != nil {
		return o, err
	}

	ca, err := choose("\nWhich certificate authority (CA)? Phones learn it when they enroll by scanning the QR code the server prints.",
		"Create this server's own CA (recommended)",
		"Use a CA I already have (ca.pem and ca-key.pem), so devices of another server keep working")
	if err != nil {
		return o, err
	}
	if ca == 1 {
		if o.importCA, err = folderWith("\nThe folder with ca.pem and ca-key.pem", "ca.pem", "ca-key.pem"); err != nil {
			return o, err
		}
	} else {
		o.newCA = true
	}

	model, err := choose("\nWhich text search model? Setup downloads the models for search, faces, text in photos, similar photos and the nudity scan: about 3 GB.",
		"Base: reads a photo in about a tenth of a second (recommended)",
		"Large: finds better, about four times slower to read photos")
	if err != nil {
		return o, err
	}
	if model == 1 {
		o.model, o.modelGiven = "large", true
	}
	return o, nil
}
