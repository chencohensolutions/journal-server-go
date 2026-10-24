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
	// CodeSimilarOff: similar photos and duplicates are off on this server (no model or runtime).
	CodeSimilarOff = "SIMILAR_OFF"
	// CodeNotReadYet: the file hasn't been read by the index asked about yet (the server is still reading).
	CodeNotReadYet = "NOT_READ_YET"
	// CodeIndexingOff: the server reads no files for indexes (no model or runtime), so there is no run to start.
	CodeIndexingOff = "INDEXING_OFF"
)

// coded is an error with a code devices act on.
func coded(code, format string, args ...any) error {
	return &gqlerror.Error{Message: fmt.Sprintf(format, args...), Extensions: map[string]any{"code": code}}
}
