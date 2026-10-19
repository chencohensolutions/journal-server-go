package model

import (
	"fmt"
	"io"
	"strconv"

	"github.com/99designs/gqlgen/graphql"
	"github.com/journal/server-go/media"
)

// MarshalSha256 writes a Sha256 scalar.
func MarshalSha256(s string) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		io.WriteString(w, strconv.Quote(s))
	})
}

// UnmarshalSha256 accepts only 64 lowercase hex characters, so resolvers never see anything else.
func UnmarshalSha256(v any) (string, error) {
	s, ok := v.(string)
	if !ok || !media.IsSha256(s) {
		return "", fmt.Errorf("Sha256 must be 64 lowercase hex characters")
	}
	return s, nil
}
