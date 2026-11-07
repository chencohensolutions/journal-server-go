package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
)

// restoreCommand: restore <user> (--from <peer> | --from-folder <folder>) --to <empty folder>
// [--key <key file> | --passphrase] [--as-of <date>].
// It brings a copy back into an empty folder (docs/peers.md, "Restore"); the server needn't be running. --to is
// always given: a restore never fills a folder it wasn't pointed at, the user's data_folder included.
func restoreCommand(config auth.AppConfig, args []string, in io.Reader, out io.Writer) error {
	usage := errors.New("restore <user> (--from <peer> | --from-folder <folder>) --to <empty folder> [--key <key file> | --passphrase] [--as-of 2026-10-20]")
	if len(args) < 1 {
		return usage
	}
	user := args[0]
	var peer, fromFolder, to, keyPath, asOfText string
	passphrase := false
	for i := 1; i < len(args); i++ {
		if args[i] == "--passphrase" {
			passphrase = true
			continue
		}
		if i+1 >= len(args) {
			return usage
		}
		switch args[i] {
		case "--from":
			peer = args[i+1]
		case "--from-folder":
			fromFolder = args[i+1]
		case "--to":
			to = args[i+1]
		case "--key":
			keyPath = args[i+1]
		case "--as-of":
			asOfText = args[i+1]
		default:
			return usage
		}
		i++
	}
	if to == "" || (peer == "") == (fromFolder == "") || (passphrase && keyPath != "") {
		return usage
	}
	var asOf time.Time
	if asOfText != "" {
		var err error
		if asOf, err = parseAsOf(asOfText); err != nil {
			return err
		}
	}

	var src copies.Target
	if peer != "" {
		info, cert, err := loadPeer(config, peer)
		if err != nil {
			return err
		}
		src = peerFor(config, info, user, cert)
	} else {
		if info, err := os.Stat(fromFolder); err != nil || !info.IsDir() {
			return fmt.Errorf("%s isn't a folder", fromFolder)
		}
		src = copies.Folder(fromFolder)
	}
	if err := src.Ready(); err != nil {
		return err
	}

	var key *copies.KeyFile
	lines := bufio.NewReader(in)
	if passphrase {
		secret, err := readSecret(lines, out, "Passphrase of the copy key: ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "Deriving the key (a second or so)...")
		kf, err := copies.KeyFromPassphrase(src, user, secret)
		if err != nil {
			return err
		}
		key = &kf
	}
	if keyPath != "" {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return err
		}
		kf, err := copies.ReadKeyFile(data, func() (string, error) { return readSecret(lines, out, "Passphrase of the key file: ") })
		if err != nil {
			return err
		}
		if kf.Key.User != user {
			return fmt.Errorf("the key file is %s's, not %s's", kf.Key.User, user)
		}
		key = &kf
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	started, shown := time.Now(), time.Time{}
	result, err := copies.RestoreAsOf(ctx, src, key, to, asOf, func(p copies.RestoreProgress) {
		if time.Since(shown) >= 2*time.Second {
			shown = time.Now()
			fmt.Fprintf(out, "%d of %d files (%d there already), %s, %d failed\n", p.Done+p.Skipped+p.Failed, p.Files,
				p.Skipped, copies.Size(p.Bytes), p.Failed)
		}
	})
	if errors.Is(err, context.Canceled) {
		fmt.Fprintf(out, "Stopped. Run the same command again to go on where it stopped.\n")
		return nil
	}
	if err != nil {
		return err
	}
	kind := "plain"
	if result.Encrypted {
		kind = "encrypted"
	}
	fmt.Fprintf(out, "Restored %s's %s copy into %s: %d files back, %d there already, %s, in %s.\n", user, kind, to,
		result.Done, result.Skipped, copies.Size(result.Bytes), time.Since(started).Round(time.Second))
	if result.Failed > 0 {
		var failed []string
		for path, why := range result.FailedFiles {
			failed = append(failed, "  "+path+": "+why)
		}
		sort.Strings(failed)
		fmt.Fprintf(out, "%d files couldn't be brought back:\n%s\nRun the same command again to try them again.\n",
			result.Failed, strings.Join(failed, "\n"))
		return fmt.Errorf("%d files not restored", result.Failed)
	}
	fmt.Fprintf(out, "Every original matched its sha256. To serve it, point the user's data_folder at %s and start the\n"+
		"server: the indexes and previews are made again, as when .cache/ is missing.\n", to)
	return nil
}

// parseAsOf reads --as-of: a day ("2026-10-20", as it was at that day's end, local time) or a time in it
// ("2026-10-20 15:00").
func parseAsOf(text string) (time.Time, error) {
	if day, err := time.ParseInLocation("2006-01-02", text, time.Local); err == nil {
		return day.AddDate(0, 0, 1), nil
	}
	if at, err := time.ParseInLocation("2006-01-02 15:04", text, time.Local); err == nil {
		return at, nil
	}
	return time.Time{}, fmt.Errorf("--as-of is a day like 2026-10-20, or a time like \"2026-10-20 15:00\", not %q", text)
}
