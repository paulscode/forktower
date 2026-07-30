package alert

import (
	"context"

	"github.com/paulscode/forktower/internal/config"
)

// PayloadVersion marks the payload shape, so a user's own automation can tell
// this is Forktower and which format it is reading.
const PayloadVersion = "v1"

// Payload is what a transport sends.
//
// Subject and Message are absent or generic unless the transport is configured
// to include detail; Tier and Kind are always present, because a notification
// with no indication of severity or category cannot be triaged at all.
type Payload struct {
	Version string `json:"forktower"`
	Tier    string `json:"tier"`
	Kind    string `json:"kind"`
	Subject string `json:"subject,omitempty"`
	Message string `json:"message"`
}

// Transport delivers a payload somewhere the user will see it.
//
// Send is expected to return promptly or honour the context. It must not include
// a URL or credential in its error text — but scrubError is applied to whatever
// it returns regardless, because the errors that leak credentials usually come
// from the HTTP client underneath rather than from the transport itself.
type Transport interface {
	// Name is the configured name, recorded against every delivery attempt so a
	// transport that has quietly stopped working can be identified.
	Name() string
	Send(ctx context.Context, p Payload) error
}

// Route is a transport together with the two settings that decide what it
// receives and how much it is told.
type Route struct {
	Transport Transport
	MinTier   config.MinTier
	// IncludeDetail is already resolved against the per-type default by the
	// caller, so nothing downstream has to know which transports are
	// platform-local.
	IncludeDetail bool
}
