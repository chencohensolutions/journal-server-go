package resolver

import (
	"encoding/pem"

	"github.com/journal/server-go/aesthetic"
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/faces"
	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/localindex"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/ocr"
	"github.com/journal/server-go/search"
	"github.com/journal/server-go/similar"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require
// here.

type Resolver struct {
	Enrollment *enroll.Service
	Library    *media.Library
	// Text search; nil when it is off.
	Search *search.Service
	// Face search; nil when it is off.
	Faces *faces.Service
	// Text in photos (OCR), similar photos and duplicates (DINOv2), aesthetic scores (LAION); each nil when off.
	// Named so as not to clash with gqlgen's resolvers of Media.text and Media.aesthetic.
	TextInPhotos    *ocr.Service
	SimilarPhotos   *similar.Service
	AestheticScores *aesthetic.Service
	// New files are scanned for nudity (package nudity), for serverInfo.
	NudityScan bool
	// Previews and posters, for serverInfo to say whether they can be made.
	Previews *media.Previews
	// The server's build, shown by serverInfo.
	Version string
	// The indexes phones may keep (localIndexes); nil or with none when none can be.
	Local *localindex.Server
	// Starts, follows and pauses the reading of each user's files for the indexes; nil when no file is read.
	IndexRuns IndexRunner
	// Each user's copies of their data folder on other disks (copies); nil on a server that keeps none.
	Copies CopyRunner
	// Who this server is and where, for serverInfo.discovery; nil when unknown.
	Discovery func() *model.ServerDiscovery
	// Sets up each user's swaps from their phone; nil on a server that sets up none.
	SwapSetup SwapService
}

// APIVersion is serverInfo.api: raised when a change needs newer apps.
const APIVersion = 1

// caPEM is the CA that signs this server's certificates, PEM, for the apps to keep (EnrolledDevice.caCertificate,
// serverInfo.caCertificate); "" when there is no issuer.
func (r *Resolver) caPEM() string {
	if r.Enrollment == nil || r.Enrollment.Issuer == nil {
		return ""
	}
	ca, _ := r.Enrollment.Issuer.CA()
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}))
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
