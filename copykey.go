package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"golang.org/x/term"
)

// A user's copy key encrypts their copies on peers (docs/peers.md, "Encrypted copies"). copy-key new makes it once:
// this server keeps the public half and the naming key (copy_keys_dir/<user>.json); the key file, which alone can
// read the copies, goes where the owner says and is forgotten here.

func copyKeyFile(config auth.AppConfig, user string) string {
	return filepath.Join(config.CopyKeysDir, user+".json")
}

// loadCopyKey is the copy key of user that copy-key new made.
func loadCopyKey(config auth.AppConfig, user string) (copies.CopyKey, error) {
	data, err := os.ReadFile(copyKeyFile(config, user))
	if errors.Is(err, os.ErrNotExist) {
		return copies.CopyKey{}, fmt.Errorf("%s has no copy key for an encrypted copy: journal-server copy-key new %s --out <file>", user, user)
	}
	if err != nil {
		return copies.CopyKey{}, err
	}
	var key copies.CopyKey
	if err := json.Unmarshal(data, &key); err != nil || key.V != 1 || key.User != user || len(key.NamingKey) != 32 {
		return copies.CopyKey{}, fmt.Errorf("%s isn't readable: %v", copyKeyFile(config, user), err)
	}
	return key, nil
}

// copyKeyCommand: copy-key new <user> --out <file> [--passphrase].
func copyKeyCommand(config auth.AppConfig, args []string, in io.Reader, out io.Writer) error {
	if len(args) < 2 || args[0] != "new" {
		return errors.New("copy-key new <user> --out <file> [--passphrase]")
	}
	user, outFile, passphrase := args[1], "", false
	for i := 2; i < len(args); i++ {
		switch {
		case args[i] == "--out" && i+1 < len(args):
			outFile, i = args[i+1], i+1
		case args[i] == "--passphrase":
			passphrase = true
		default:
			return fmt.Errorf("copy-key new <user> --out <file> [--passphrase], not %q", args[i])
		}
	}
	if config.FindUser(user) == nil {
		return fmt.Errorf("user %q is not in config.json", user)
	}
	if outFile == "" {
		return errors.New("--out <file>: where the key file goes (a USB stick, a folder that is backed up elsewhere)")
	}
	if _, err := os.Stat(copyKeyFile(config, user)); err == nil {
		return fmt.Errorf("%s has a copy key already (%s): a new one couldn't read the copies made with it", user, copyKeyFile(config, user))
	}
	if _, err := os.Stat(outFile); err == nil {
		return fmt.Errorf("%s exists already; choose another file", outFile)
	}
	lines := bufio.NewReader(in)

	key, identity, err := copies.NewCopyKey(user, time.Now())
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(outFile)
	if err != nil {
		return err
	}
	key.KeyFile = abs
	var text bytes.Buffer
	fmt.Fprintf(&text, "# Journal copy key of %s, made %s by journal-server copy-key.\n", user, key.CreatedAt.Format("2006-01-02"))
	fmt.Fprintf(&text, "# It alone reads the encrypted copies of %s's files on friends' servers: without it nobody can, you included.\n", user)
	fmt.Fprintf(&text, "# Each file opens with the age tool too: age -d -i <this file> <file>.\n")
	fmt.Fprintf(&text, "# journal-user: %s\n", user)
	fmt.Fprintf(&text, "# journal-naming-key: %s\n", base64.StdEncoding.EncodeToString(key.NamingKey))
	fmt.Fprintf(&text, "%s\n", identity.String())
	data := text.Bytes()
	if passphrase {
		first, err := readSecret(lines, out, "Passphrase for the key file: ")
		if err != nil {
			return err
		}
		second, err := readSecret(lines, out, "Again: ")
		if err != nil {
			return err
		}
		if first != second || len(first) < 12 {
			return errors.New("the passphrases differ, or are shorter than 12 characters")
		}
		if data, err = sealWithPassphrase(data, first); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		return err
	}

	// The owner proves the key file is where they think: the last 6 characters of the key, typed back.
	secret := identity.String()
	fmt.Fprintf(out, "\nThe key file is written to %s.\n", abs)
	fmt.Fprintf(out, "Without it, %s's encrypted copies can't be read, by anyone. This server forgets the private key now.\n\n", user)
	fmt.Fprintf(out, "Open the file and type the last 6 characters of its last line: ")
	typed, _ := lines.ReadString('\n')
	if !strings.EqualFold(strings.TrimSpace(typed), secret[len(secret)-6:]) {
		os.Remove(abs)
		how := ""
		if passphrase {
			how = " (it opens with: age -d <file>)"
		}
		return fmt.Errorf("that isn't the end of the key%s: the key file is deleted and nothing changed; run copy-key new again", how)
	}
	stored, err := json.MarshalIndent(key, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.CopyKeysDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(copyKeyFile(config, user), stored, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nCopy key of %s made. To encrypt a copy on a peer, set \"encrypt\": true on it in config.json and restart.\n", user)
	fmt.Fprintf(out, "Keep the key file away from this server: a copy of it on a USB stick, and one elsewhere.\n")
	return nil
}

// readSecret reads a line without showing it when in is the terminal.
func readSecret(lines *bufio.Reader, out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	if f, ok := any(os.Stdin).(*os.File); ok && lines.Buffered() == 0 && term.IsTerminal(int(f.Fd())) && out == io.Writer(os.Stdout) {
		secret, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		return string(secret), err
	}
	line, err := lines.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// sealWithPassphrase encrypts the key file with age's passphrase mode, armored: age -d opens it.
func sealWithPassphrase(data []byte, passphrase string) ([]byte, error) {
	to, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, err
	}
	var sealed bytes.Buffer
	aw := armor.NewWriter(&sealed)
	w, err := age.Encrypt(aw, to)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	return sealed.Bytes(), nil
}
