package anchors

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// The names under the data directory. A list and its detached signature, kept
// together because neither is any use without the other.
const (
	activeListName = "active.txt"
	activeSigName  = "active.txt.sig"
	activeDirName  = "anchors"
)

// Store loads and replaces the list of anchor peers.
//
// The I/O half of this package. The rules live next door in list.go and
// verify.go, which cannot open a file or reach the network — the split is what
// keeps the question "would this list be trusted?" answerable by reading one
// screen of code.
type Store struct {
	// dir is Forktower's data directory.
	dir string
	// builtInPath is the list shipped with the release, used when there is no
	// verified replacement and whenever one fails to verify.
	builtInPath string
	log         *slog.Logger
}

// NewStore builds a Store. Nothing is read here.
func NewStore(dataDir, builtInPath string, log *slog.Logger) *Store {
	if log == nil {
		log = slog.New(discardHandler{})
	}
	return &Store{dir: dataDir, builtInPath: builtInPath, log: log}
}

// Active is the list in use, and why it is the one in use.
type Active struct {
	List
	// Fallback is set when an imported list was present but not usable, naming
	// what was wrong with it. The built-in list is in use in that case.
	//
	// **Surfaced rather than swallowed.** Quietly falling back would leave a user
	// believing they were running the peers they imported.
	Fallback string
}

// Load returns the list to use.
//
// **Re-verified on every load, not trusted because it was trusted once.** The
// file sits in a data directory that a backup restore, a misplaced volume mount
// or anyone with a shell could have rewritten since. The signature is what makes
// it trustworthy, so the signature is checked every time it is read; there is no
// separate record of "this was fine yesterday", because such a record is exactly
// what an attacker would edit instead.
//
// Never fails: a list that cannot be verified means the built-in one is used and
// the reason is reported. A daemon that refused to start because a peer file was
// wrong would be a daemon that an attacker could stop by corrupting a file.
func (s *Store) Load() Active {
	builtIn := s.loadBuiltIn()

	raw, sig, err := s.readImported()
	switch {
	case errors.Is(err, errNothingImported):
		// The ordinary state. Nothing imported, nothing to say.
		return Active{List: builtIn}
	case err != nil:
		// **Including a list whose signature is missing**, which is not the same
		// thing as nothing being imported and must not be quiet. The two files
		// are written together and are useless apart, so finding one without the
		// other means a write was interrupted or a file was removed — and a
		// removed signature is exactly what somebody would leave behind after
		// editing the list it used to vouch for.
		return s.fallback(builtIn, "the imported list could not be read", err)
	}

	// Compared against the *built-in* version rather than against itself: a file
	// on disk cannot vouch for its own freshness, and asking whether it beats
	// what the release shipped is the question that has an answer.
	imported, err := Accept(raw, sig, builtIn)
	if err != nil {
		return s.fallback(builtIn, "the imported list was not accepted", err)
	}
	return Active{List: imported}
}

func (s *Store) fallback(builtIn List, what string, err error) Active {
	s.log.Warn(what+"; using the list this release shipped with",
		slog.String("error", err.Error()))
	return Active{List: builtIn, Fallback: err.Error()}
}

// loadBuiltIn reads the shipped list.
//
// A build with no readable list is not an error either — it is a build with no
// anchors, which is the shipped state anyway. The node then finds peers the
// ordinary way, which is what it would have done regardless.
func (s *Store) loadBuiltIn() List {
	raw, err := os.ReadFile(s.builtInPath)
	if err != nil {
		return List{Source: SourceBuiltIn}
	}
	list, err := Parse(raw)
	if err != nil {
		s.log.Warn("the anchor list shipped with this release is unreadable",
			slog.String("path", s.builtInPath), slog.String("error", err.Error()))
		return List{Source: SourceBuiltIn}
	}
	list.Source = SourceBuiltIn
	return list
}

// errNothingImported means no list has been imported at all — the ordinary
// state, and the only one that is not worth mentioning.
var errNothingImported = errors.New("anchors: no imported list")

func (s *Store) readImported() (raw, sig []byte, err error) {
	raw, err = os.ReadFile(filepath.Join(s.dir, activeDirName, activeListName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, errNothingImported
	}
	if err != nil {
		return nil, nil, err
	}
	// Deliberately not folded into the case above. A missing signature beside a
	// present list is a fault to report, not an absence to shrug at.
	sig, err = os.ReadFile(filepath.Join(s.dir, activeDirName, activeSigName))
	if err != nil {
		return nil, nil, fmt.Errorf("the list has no signature beside it: %w", err)
	}
	return raw, sig, nil
}

// Import verifies a list and, if it is good, makes it the active one.
//
// Returns what would now be loaded, or an error saying why nothing changed. The
// existing list is left exactly as it was on any failure: there is no state in
// which half a peer list is in use, because half a peer list is not a safer peer
// list.
//
// Takes bytes rather than a path. The caller has already decided this is
// something the user asked for — this function's job is to decide whether it can
// be trusted, and it should not also be able to go looking for input.
func (s *Store) Import(raw, sig []byte) (List, error) {
	current := s.Load().List

	accepted, err := Accept(raw, sig, current)
	if err != nil {
		return List{}, err
	}

	dir := filepath.Join(s.dir, activeDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return List{}, fmt.Errorf("making room for the anchor list: %w", err)
	}

	// The signature is written first. If the machine loses power between the two
	// writes, the next Load finds a signature that does not match the old list
	// and falls back to the built-in one — noisy, but honest. The other order
	// would leave a new list standing behind an old signature, which is the
	// shape of a file that verifies against nothing and is trusted anyway.
	if err := os.WriteFile(filepath.Join(dir, activeSigName), sig, 0o600); err != nil {
		return List{}, fmt.Errorf("writing the anchor list signature: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, activeListName), raw, 0o600); err != nil {
		return List{}, fmt.Errorf("writing the anchor list: %w", err)
	}

	s.log.Info("a new anchor-peer list was imported",
		slog.Int64("version", accepted.Version),
		slog.Int("peers", len(accepted.Peers)))
	return accepted, nil
}

// discardHandler drops log records, for a store built without a logger.
type discardHandler struct{ slog.Handler }

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }
