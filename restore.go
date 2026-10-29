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

// restoreCommand: restore <user> (--from <peer> | --from-folder <folder>) --to <empty folder> [--key <key file>].
// It brings a copy back into an empty folder (docs/peers.md, "Restore"); the server needn't be running. --to is
// always given: a restore never fills a folder it wasn't pointed at, the user's data_folder included.
func restoreCommand(config auth.AppConfig, args []string, in io.Reader, out io.Writer) error {
	usage := errors.New("restore <user> (--from <peer> | --from-folder <folder>) --to <empty folder> [--key <key file>]")
	if len(args) < 1 {
		return usage
	}
	user := args[0]
	var peer, fromFolder, to, keyPath string
	for i := 1; i < len(args); i++ {
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
		default:
			return usage
		}
		i++
	}
	if to == "" || (peer == "") == (fromFolder == "") {
		return usage
	}

	var src copies.Target
	if peer != "" {
		info, cert, err := loadPeer(config, peer)
		if err != nil {
			return err
		}
		src = copies.NewPeer(info.Name, info.URL, user, cert, info.Fingerprint)
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
	if keyPath != "" {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return err
		}
		lines := bufio.NewReader(in)
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
	result, err := copies.Restore(ctx, src, key, to, func(p copies.RestoreProgress) {
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
