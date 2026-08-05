// Package anchors holds the list of peers the second Bitcoin node starts from,
// and the rules for deciding which version of that list to trust.
//
// **Whoever controls this list controls who Forktower peers with on the chain it
// is defending.** That is the whole reason any of this exists: an attacker who
// could replace it could feed the second node a fabricated quiet chain and the
// daemon would report that nothing was happening, which is the one lie that
// costs a user money. So a list is used only if it is signed by a key pinned
// into this binary at build time, and only if it is newer than the one already
// in use — because handing back an old list of peers that have since gone dark
// is an attack that needs no signing key at all.
//
// Lists arrive by app update or by the user importing a file. Never by fetching:
// a daemon that downloads its own configuration is a daemon whose configuration
// belongs to whoever holds the other end.
package anchors

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// List is a set of peer addresses with the version that names it.
type List struct {
	// Version is the monotonic counter. Higher replaces lower; equal or lower is
	// refused, which is what makes a rollback fail.
	Version int64
	// Peers are addresses in host:port form, in the order they were written.
	Peers []string
	// Source says where this list came from, for the dashboard. Not part of what
	// is signed — it is a fact about this installation, not about the list.
	Source Source
}

// Source is where a list came from.
type Source string

const (
	// SourceBuiltIn is the list compiled into the release.
	SourceBuiltIn Source = "built-in"
	// SourceImported is a signed list the user imported.
	SourceImported Source = "imported"
)

// FormatTag is the first directive of a list file, so that a file which is not
// one of these fails as unreadable rather than as empty.
const FormatTag = "forktower-anchors"

// FormatVersion is the layout this code understands. Bumped only if the shape
// changes; the *list* version in a file is a different number and moves far more
// often.
const FormatVersion = 1

// MaxPeers bounds a list.
//
// Not a guess at how many are useful — a few dozen would be plenty — but a limit
// on what a single file can do to a node. Bitcoin Core will try to hold an
// `addnode` connection to every entry, and a list of ten thousand would be a
// denial of service delivered as a configuration file.
const MaxPeers = 256

// Errors a caller may want to tell apart.
var (
	// ErrNotAList means the file is not an anchor list at all.
	ErrNotAList = errors.New("anchors: not an anchor-peer list")
	// ErrUnsupportedFormat means the layout is newer than this build understands.
	ErrUnsupportedFormat = errors.New("anchors: unsupported list format")
	// ErrNoVersion means the file carries no version counter, which would make
	// rollback protection impossible.
	ErrNoVersion = errors.New("anchors: the list has no version")
	// ErrTooManyPeers means the file exceeds MaxPeers.
	ErrTooManyPeers = errors.New("anchors: too many peers")
)

// Parse reads a list file.
//
// Deliberately strict. This runs on bytes that may have been handed to the
// daemon by someone hostile, and the cost of being generous here is a peer list
// that does not say what its author thought it said.
func Parse(raw []byte) (List, error) {
	var (
		out       List
		sawFormat bool
		sawVer    bool
	)

	for n, line := range strings.Split(string(raw), "\n") {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		key, value, found := strings.Cut(text, ":")
		if !found {
			return List{}, fmt.Errorf("%w: line %d is not `key: value`", ErrNotAList, n+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case FormatTag:
			format, err := strconv.Atoi(value)
			if err != nil {
				return List{}, fmt.Errorf("%w: unreadable format version", ErrNotAList)
			}
			// Refused rather than best-effort. A newer format may mean something
			// by a field this build would silently ignore, and silently ignoring
			// part of a peer list is how you end up peering with less than you
			// were told.
			if format != FormatVersion {
				return List{}, fmt.Errorf("%w: file is format %d, this build reads %d",
					ErrUnsupportedFormat, format, FormatVersion)
			}
			sawFormat = true

		case "version":
			version, err := strconv.ParseInt(value, 10, 64)
			if err != nil || version < 0 {
				return List{}, fmt.Errorf("%w: unreadable version %q", ErrNoVersion, value)
			}
			out.Version = version
			sawVer = true

		case "peer":
			if value == "" {
				return List{}, fmt.Errorf("%w: line %d has an empty peer", ErrNotAList, n+1)
			}
			if len(out.Peers) >= MaxPeers {
				return List{}, fmt.Errorf("%w: more than %d", ErrTooManyPeers, MaxPeers)
			}
			out.Peers = append(out.Peers, value)

		default:
			return List{}, fmt.Errorf("%w: line %d has unknown key %q", ErrNotAList, n+1, key)
		}
	}

	if !sawFormat {
		return List{}, fmt.Errorf("%w: no %s directive", ErrNotAList, FormatTag)
	}
	if !sawVer {
		return List{}, ErrNoVersion
	}
	return out, nil
}

// Supersedes reports whether candidate should replace current.
//
// **Strictly greater, never equal.** Two different lists carrying the same
// version is either a mistake by whoever signs them or an attempt to swap one
// for the other without moving the counter, and neither is a reason to change
// which peers a node trusts.
func (l List) Supersedes(current List) bool { return l.Version > current.Version }

// Empty reports whether the list names no peers.
//
// An empty list is valid and is the shipped state: naming nodes that have since
// gone dark is worse than naming none, because it looks like a measure that is
// working. The node then relies on ordinary peer discovery, which is what it
// would do anyway.
func (l List) Empty() bool { return len(l.Peers) == 0 }
