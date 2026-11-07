package peers

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"time"
)

// The peer API, shared with the client side in package copies (copies.Peer). Paths are relative to the copy's root,
// with forward slashes, as in a data folder.
//
//	POST   /peer/enroll                       {code, csr} → {name, quota, certificate}; anonymous, rate limited
//	POST   /peer/renew                        {csr} → {name, quota, certificate}; the old certificate works until the new is used
//	GET    /peer/space                        Space
//	GET    /peer/{user}/list?prefix=&recursive=1   one ListEntry per line (JSON)
//	HEAD   /peer/{user}/files/{path}          Content-Length and HeaderModified
//	GET    /peer/{user}/files/{path}          the file, with Range
//	PUT    /peer/{user}/files/{path}          the file (HeaderSha256, HeaderModified; Content-Range to resume)
//	PATCH  /peer/{user}/files/{path}          sets HeaderModified
//	DELETE /peer/{user}/files/{path}
//	GET    /peer/{user}/sha256/{path}         {sha256, size}, hashed here
//	POST   /peer/{user}/move/{path}           moves the file to HeaderMoveTo (a previous version kept)
//	HEAD   /peer/{user}/upload/{sha256}       HeaderOffset: how much of a resumed upload is here
//	GET    /peer/addresses                    Discovery: where the host is now, its DHT key, the peer's IP
const (
	// The whole file's sha256: a put is checked against it before the file takes its place.
	HeaderSha256 = "Journal-Sha256"
	// A file's modified time, RFC 3339 with nanoseconds (Last-Modified keeps only seconds).
	HeaderModified = "Journal-Modified"
	// How many bytes of an upload the host has.
	HeaderOffset = "Upload-Offset"
	// Where a move puts the file: a path in the same space, where no file is.
	HeaderMoveTo = "Journal-Move-To"
)

// Error codes in an error answer's JSON ({"error": code, "message": ...}).
const (
	CodeInvalidCode = "INVALID_CODE"
	CodeInvalidCSR  = "INVALID_CSR"
	CodeRateLimited = "RATE_LIMITED"
	// The peer's files would take more than its quota.
	CodeQuotaFull = "QUOTA_FULL"
	// The bytes put aren't the ones HeaderSha256 named.
	CodeDamaged = "DAMAGED"
	// A resumed put starts where the host's part doesn't end; HeaderOffset says where it does.
	CodeWrongOffset = "WRONG_OFFSET"
)

// EnrollRequest is what a peer sends with an invite code.
type EnrollRequest struct {
	Code string `json:"code"`
	CSR  []byte `json:"csr"` // PKCS#10, DER
}

// Enrolled is the host's answer: the name the invite gave the peer, its quota and its certificate (DER), and how
// to find the host when its address changes.
type Enrolled struct {
	Name        string     `json:"name"`
	Quota       int64      `json:"quota"`
	Certificate []byte     `json:"certificate"`
	Discovery   *Discovery `json:"discovery,omitempty"`
}

// Discovery is how a peer finds the host when the address it has stops answering (docs/peers.md, "Finding the
// host"): where the host is now, and the DHT key it publishes its addresses under.
type Discovery struct {
	// "https://host:port", the most likely first.
	Addresses []string `json:"addresses"`
	// The host's ed25519 key in the DHT, hex; "" when the host doesn't publish there ("discovery": false).
	Key string `json:"key,omitempty"`
	// The address the peer's request came from: its public IP, as the host sees it.
	Seen string `json:"seen,omitempty"`
}

// Space is how much a peer may still store.
type Space struct {
	Quota int64 `json:"quota"`
	Used  int64 `json:"used"`
	// What can still be written: the quota left, or the host's disk's free space if less.
	Free int64 `json:"free"`
}

// ListEntry is one file in a listing.
type ListEntry struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Fingerprint is how a peer recognizes the host's server: the sha256 of its certificate's public key, in hex. The
// invite carries it, and the peer checks it on every connection.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}
