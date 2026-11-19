package bundle

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	// Deprecated for new uses, but it reads and checks the RSA and DSA signatures MSYS2's packagers make, which is
	// all this needs; no other OpenPGP library is a dependency of the server.
	"golang.org/x/crypto/openpgp" //nolint:staticcheck
)

//go:embed msys2keys/*.pgp
var msys2KeyFiles embed.FS

var msys2Keys = sync.OnceValues(func() (openpgp.EntityList, error) {
	files, err := fs.Glob(msys2KeyFiles, "msys2keys/*.pgp")
	if err != nil {
		return nil, err
	}
	var keys openpgp.EntityList
	for _, name := range files {
		data, err := msys2KeyFiles.ReadFile(name)
		if err != nil {
			return nil, err
		}
		list, err := openpgp.ReadKeyRing(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		keys = append(keys, list...)
	}
	if len(keys) == 0 {
		return nil, errors.New("no MSYS2 keys")
	}
	return keys, nil
})

// checkMSYS2Signature reports whether signature (a detached OpenPGP signature, as the repository's list gives it)
// is one of MSYS2's packagers' over the file at path.
func checkMSYS2Signature(path string, signature []byte) error {
	return checkSignature(path, signature, msys2Keys)
}

func checkSignature(path string, signature []byte, keys func() (openpgp.EntityList, error)) error {
	if len(signature) == 0 {
		return errors.New("the package has no signature")
	}
	ring, err := keys()
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := openpgp.CheckDetachedSignature(ring, f, bytes.NewReader(signature)); err != nil {
		return fmt.Errorf("not signed by an MSYS2 packager: %w", err)
	}
	return nil
}
