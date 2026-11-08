// Command sign-release makes the release key and signs a release's SHA256SUMS with it (docs/install.md, "Releases").
//
//	go run ./scripts/sign-release new-key <file>   write a new private key to <file> (for the RELEASE_SIGNING_KEY
//	                                               secret) and print its public half for update/release-keys.txt
//	go run ./scripts/sign-release sign <file>      write <file>.sig, signed with the key in $RELEASE_SIGNING_KEY
//	go run ./scripts/sign-release verify <file>    check <file>.sig against update/release-keys.txt
//
// The private key is the base64 of ed25519's 32-byte seed. Keep it out of the repository.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/journal/server-go/update"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: sign-release new-key <file> | sign <file> | verify <file>")
	}
	file := args[1]
	switch args[0] {
	case "new-key":
		if _, err := os.Stat(file); err == nil {
			return fmt.Errorf("%s exists; not overwritten", file)
		}
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		if err := os.WriteFile(file, []byte(base64.StdEncoding.EncodeToString(private.Seed())+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Printf("Private key written to %s: add it as the repository secret RELEASE_SIGNING_KEY, then keep it offline.\n", file)
		fmt.Printf("Public key, for update/release-keys.txt:\n%s\n", base64.StdEncoding.EncodeToString(public))
		return nil
	case "sign":
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("RELEASE_SIGNING_KEY")))
		if err != nil || len(seed) != ed25519.SeedSize {
			return errors.New("RELEASE_SIGNING_KEY is not set to a key from sign-release new-key")
		}
		key := ed25519.NewKeyFromSeed(seed)
		if !keyIsListed(key.Public().(ed25519.PublicKey)) {
			return errors.New("RELEASE_SIGNING_KEY's public half isn't in update/release-keys.txt: servers would refuse this release")
		}
		message, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, message))
		return os.WriteFile(file+".sig", []byte(sig+"\n"), 0o644)
	case "verify":
		message, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(file + ".sig")
		if err != nil {
			return err
		}
		if !update.Verify(update.ReleaseKeys(), message, sig) {
			return fmt.Errorf("%s.sig is not a signature of %s by a release key", file, file)
		}
		fmt.Println("Good signature")
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func keyIsListed(public ed25519.PublicKey) bool {
	for _, k := range update.ReleaseKeys() {
		if k.Equal(public) {
			return true
		}
	}
	return false
}
