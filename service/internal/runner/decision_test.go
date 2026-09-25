package runner

import (
	"testing"

	k3 "github.com/doipster/asset_cracker/service/internal/engine"
)

// The journal row says which probabilities the engine formed, so the store writes NULL for the
// others and never a 0 that the scorecard would read as a price.
func TestTheJournalRowSaysWhichProbabilitiesWereFormed(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	id := g.bucketID()
	for _, c := range []struct {
		name              string
		d                 k3.Decision
		noModel, noMarket bool
		model, market     float64
	}{
		{"both", k3.Decision{BucketID: id, HasModel: true, HasProb: true, ModelProb: 0.9, MarketProb: 0.6}, false, false, 0.9, 0.6},
		{"one-sided book", k3.Decision{BucketID: id, HasModel: true, ModelProb: 0.9}, false, true, 0.9, 0},
		{"no view", k3.Decision{BucketID: id}, true, true, 0, 0},
	} {
		row := r.decisionRow(c.d)
		if row.NoModelProb != c.noModel || row.NoMarketProb != c.noMarket || row.ModelProb != c.model || row.MarketProb != c.market {
			t.Errorf("%s: %+v", c.name, row)
		}
	}
}
