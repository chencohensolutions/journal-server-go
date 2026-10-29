package main

import (
	"fmt"
	"log"
	"os"

	"github.com/journal/server-go/auth"
)

const configPath = "config.json"

const usage = `usage:
  journal-server                      run the server
  journal-server enroll-code <user>   create a one-time enrollment code
  journal-server devices [user]       list enrolled devices
  journal-server revoke <serial>      revoke an enrolled device
  journal-server private <serial> withhold|allow
                                      keep private files from a device, or send them to it again
  journal-server index                read new files for the indexes once, then exit (for "index": "daily";
                                      the server starts it at "index_at")
  journal-server index --only nudity,faces
                                      the same, with only the indexes named (nudity, faces, ocr, search,
                                      similar, aesthetic), to bring those up to date first
  journal-server index --user <name>  the same, for one user's files only
  journal-server index --progress     also write how the run is going to stdout, for the server that started it
  journal-server copy [user]          start a full pass of the user's copies now (config.json "copies")
  journal-server copy report [user] [--list]
                                      how each copy is doing, and what it lacks now
  journal-server peer invite --name <name> --quota <size>
                                      let another server keep copies here (needs peers_folder): prints its code
  journal-server peer add <name> <url> <code> [--trusted]
                                      keep copies on another server, with the code its peer invite printed;
                                      --trusted allows plain copies there (its admin can read them)
  journal-server peers                the servers our copies are on, and those whose copies are here
  journal-server peer revoke <name>   refuse a server that keeps copies here; its files stay
  journal-server peer remove <name>   stop using a server for copies (take it out of config.json first)
  journal-server copy-key new <user> --out <file> [--passphrase]
                                      make the key that encrypts the user's copies on peers; only the key file
                                      written to <file> can read them
  journal-server copy-key new <user> --from-passphrase
                                      the same from a passphrase: no key file, the passphrase alone reads them
  journal-server copy-key export <user> --out <file> [--passphrase]
                                      a key file of a key made from a passphrase
  journal-server restore <user> (--from <peer> | --from-folder <folder>) --to <empty folder> [--key <file> | --passphrase]
                                      bring a copy back into an empty folder; an encrypted one needs the key file
`

func main() {
	config, err := auth.LoadConfig(configPath)
	if err != nil {
		log.Fatal(err)
	}

	if len(os.Args) < 2 {
		serve(config)
		return
	}
	if os.Args[1] == "index" {
		args, err := parseIndexArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		if err := runIndex(config, args); err != nil {
			log.Fatal(err)
		}
		return
	}
	run := runAdminCommand
	switch os.Args[1] {
	case "peer", "peers":
		run = runPeerCommand
	case "copy-key":
		run = func(config auth.AppConfig, _ string, args []string) error {
			return copyKeyCommand(config, args, os.Stdin, os.Stdout)
		}
	case "restore":
		run = func(config auth.AppConfig, _ string, args []string) error {
			return restoreCommand(config, args, os.Stdin, os.Stdout)
		}
	}
	if err := run(config, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
