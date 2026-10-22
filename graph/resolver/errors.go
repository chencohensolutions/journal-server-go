package resolver

import (
	"fmt"

	"github.com/vektah/gqlparser/v2/gqlerror"
)

// Codes a device acts on, sent as extensions.code of a GraphQL error (docs/media.md, "Errors"). Other errors are
// for the log: a device shows them as a server problem.
const (
	// CodeUnauthorized: no client certificate was sent for a field that needs one (auth.RequireUserUnlessPublic).
	CodeUnauthorized = "UNAUTHORIZED"
	// CodeNoDataFolder: the user has no data_folder in config.json.
	CodeNoDataFolder = "NO_DATA_FOLDER"
	// CodeInvalidArgument: an argument is over its documented limit or malformed (a client bug, not a user error).
	CodeInvalidArgument = "INVALID_ARGUMENT"
	// CodeSearchOff: text search is off on this server (no model or runtime).
	CodeSearchOff = "SEARCH_OFF"
	// CodeWithheld: this device is kept from private files, so it can't unmark them.
	CodeWithheld = "WITHHELD"
)

// coded is an error with a code devices act on.
func coded(code, format string, args ...any) error {
	return &gqlerror.Error{Message: fmt.Sprintf(format, args...), Extensions: map[string]any{"code": code}}
}
