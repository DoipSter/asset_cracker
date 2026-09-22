package app

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/doipster/asset_cracker/service/internal/web"
)

// tracked is the list of coins the home page shows and the trade stream follows: every active
// Coinbase spot instrument in the table, whether a migration seeded it or the assets page
// switched it on. There is no list in code. It is re-read from the table at most once a
// minute, so a coin switched on shows up, priced, within about that long.
//
// The seeded coins come first in the table's order, then the switched-on ones, so the rail
// does not reorder under the reader when something is added.
type tracked struct {
	db  *store.Store
	mu  sync.Mutex
	at  time.Time
	all []store.Instrument
}

func newTracked(db *store.Store, initial []store.Instrument) *tracked {
	return &tracked{db: db, at: time.Now(), all: initial}
}

func (t *tracked) instruments() []store.Instrument {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.at) > time.Minute {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		all, err := t.db.ActiveInstruments(ctx)
		t.at = time.Now()
		if err != nil {
			slog.Warn("could not re-read the tracked assets; the last list stands", "err", err)
		} else {
			t.all = all
		}
	}
	return t.all
}

// spots are the Coinbase spot instruments, seeded first.
func (t *tracked) spots() []store.Instrument {
	var seeded, selected []store.Instrument
	for _, in := range t.instruments() {
		if in.Source != "coinbase" || in.Kind != "spot" {
			continue
		}
		if isSelected(in) {
			selected = append(selected, in)
		} else {
			seeded = append(seeded, in)
		}
	}
	return append(seeded, selected...)
}

// Assets is what the home page lists.
func (t *tracked) Assets() []web.Asset {
	var out []web.Asset
	for _, in := range t.spots() {
		out = append(out, assetOf(in))
	}
	return out
}

// Products is what the trade stream subscribes to.
func (t *tracked) Products() []string {
	var out []string
	for _, in := range t.spots() {
		out = append(out, in.Symbol)
	}
	return out
}

// assetOf turns one spot instrument into a listed asset: the coin is its underlying (or the
// symbol before the dash), the presentation is the widget's where it had one, and a
// "decimals" in the spec overrides the default precision.
func assetOf(in store.Instrument) web.Asset {
	coin := strings.ToUpper(in.Underlying)
	if coin == "" {
		coin = strings.ToUpper(strings.SplitN(in.Symbol, "-", 2)[0])
	}
	a := web.Style(coin, in.Symbol)
	if d, ok := in.Spec["decimals"].(float64); ok && d >= 0 && d <= 8 {
		a.Decimals = int(d)
	}
	if name, ok := in.Spec["name"].(string); ok && name != "" {
		a.Name = name
	}
	return a
}
