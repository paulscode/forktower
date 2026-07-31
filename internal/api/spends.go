package api

import (
	"net/http"

	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/words"
)

// Spend is one transaction that spent something Forktower was watching.
//
// The Details view. A reader who has opened this has asked for transaction ids
// and heights, so they are here — but the sentence explaining what happened is
// here too, because "commitment_unknown" is not a thing to show anybody.
type Spend struct {
	ID           int64  `json:"id"`
	Branch       string `json:"branch"`
	ChannelID    int64  `json:"channel_id,omitempty"`
	OutpointTxID string `json:"outpoint_txid"`
	OutpointVout int32  `json:"outpoint_vout"`
	SpendTxID    string `json:"spend_txid"`
	BlockHash    string `json:"block_hash,omitempty"`
	BlockHeight  int32  `json:"block_height,omitempty"`
	Shape        string `json:"shape"`
	Status       string `json:"status"`
	FirstSeenAt  int64  `json:"first_seen_at"`
	UpdatedAt    int64  `json:"updated_at"`

	// Display is the same event in words. Present even here, because Details is
	// still read by the same person.
	Display SpendDisplay `json:"display"`
}

// SpendDisplay is what a spend says in plain language.
type SpendDisplay struct {
	// What happened, in one sentence.
	What string `json:"what"`
	// Where it happened, in the vocabulary the rest of the page uses.
	Where string `json:"where"`
	// ShortTxID is the transaction, shortened. A full sixty-four characters is
	// not more identifying to this reader; it is just wider.
	ShortTxID string `json:"short_txid"`
	// Confirmed says whether this is a fact about the chain or a sighting.
	Confirmed bool `json:"confirmed"`
}

func (s *Server) handleSpends(w http.ResponseWriter, r *http.Request) {
	channelID, ok := intParam(w, r, "channel_id")
	if !ok {
		return
	}
	branch := store.Branch(r.URL.Query().Get("branch"))
	if branch != "" && !branch.Valid() {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"branch must be either \"sf\" or \"sq\"")
		return
	}

	rows, err := s.store.ListSpends(r.Context(), store.SpendFilter{
		Branch: branch, ChannelID: channelID, Limit: store.MaxSpendLimit,
	})
	if err != nil {
		s.fail(w, r, "reading what has happened to your channels", err)
		return
	}

	out := make([]Spend, 0, len(rows))
	for _, sp := range rows {
		out = append(out, Spend{
			ID: sp.ID, Branch: string(sp.Branch), ChannelID: sp.ChannelID,
			OutpointTxID: sp.OutpointTxID, OutpointVout: sp.OutpointVout,
			SpendTxID: sp.SpendTxID, BlockHash: sp.BlockHash,
			BlockHeight: sp.BlockHeight, Shape: string(sp.Shape),
			Status:      string(sp.Status),
			FirstSeenAt: sp.FirstSeenAt, UpdatedAt: sp.UpdatedAt,
			Display: describeSpend(sp),
		})
	}
	writeData(w, out)
}

// The sentences describing what a spend was. The vocabulary rule applies here as
// much as anywhere: a reader in Details has asked for more detail, not for the
// names of this program's internal states.
const (
	spendCoop      = "The channel was closed by agreement."
	spendOurs      = "Your own node closed this channel."
	spendUnknown   = "Somebody closed this channel — Forktower cannot tell whether it was fair."
	spendRevoked   = "Somebody tried to take money from this channel using an old balance."
	spendJustice   = "The claim that protects you was made."
	spendSweep     = "Somebody collected after the waiting period ended."
	spendHTLCClaim = "A payment that was in flight was claimed."
	spendOther     = "Something spent this output that Forktower does not recognise."
)

func describeSpend(sp store.Spend) SpendDisplay {
	return SpendDisplay{
		What:      spendSentence(sp.Shape),
		Where:     branchPhrase(sp.Branch),
		ShortTxID: shortID(sp.SpendTxID),
		Confirmed: sp.Status == store.SpendConfirmed,
	}
}

func spendSentence(shape store.SpendShape) string {
	switch shape {
	case store.ShapeMutualClose:
		return spendCoop
	case store.ShapeCommitmentOurs:
		return spendOurs
	case store.ShapeCommitmentUnknown:
		return spendUnknown
	case store.ShapeCommitmentRevoked:
		return spendRevoked
	case store.ShapeJustice:
		return spendJustice
	case store.ShapeDelayedSweep:
		return spendSweep
	case store.ShapeHTLCClaim:
		return spendHTLCClaim
	case store.ShapeUnknown:
		return spendOther
	default:
		return spendOther
	}
}

// branchPhrase names a chain the way everything else does.
func branchPhrase(b store.Branch) string {
	return words.Chain(string(b))
}
