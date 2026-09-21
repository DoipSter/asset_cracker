package runner

import "github.com/doipster/asset_cracker/service/internal/store"

// The third engine's typed view, for the home page and the value snapshots. Like books.go,
// everything here comes from memory; nothing asks the database. It lives in a file of its own so
// that the third runner can be read, and removed, without touching the first two engines' books.

// Book is every bucket the third engine HOLDS, tradable or settle-only, and every open bet.
//
// Halted is the reason v3 is SUSPENDED, or a panic that has been recovered and not yet acted on:
// in both cases memory may be behind the ledger, and SnapshotRefusal must refuse the minute for
// every engine. It is "" while v3 is PAUSED: every refused write certainly rolled back, so memory
// equals the ledger, the book is true and the snapshots go on (plan 5.4).
//
// It waits on r.mu, which a step holds across its write. That is allowed here and nowhere on the
// pollers' or the price stream's path: this is called by the home page and the snapshot writer.
//
// A panic in here is stopped under the lock (the snapshot writer's goroutine has no recover of
// its own, and a panic there would stop v1 and v2 with it): v3 is suspended, and the book returned
// is empty and HALTED with the panic, so the minute is refused rather than written from a memory
// nobody can vouch for.
func (r *Runner3) Book() (out Book) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() {
		if p := recover(); p != nil {
			out = Book{Engine: "v3", Halted: r.panickedLocked("Book", p)}
		}
	}()
	out = Book{Engine: "v3"}
	if r.state == stateSuspended {
		out.Halted = r.reason
	}
	if note := r.panicNote.Load(); note != nil {
		out.Halted = *note
	}
	for _, b := range r.buckets {
		a := r.engine.Account(b.ID)
		if a == nil {
			// Before the first rebuild has succeeded: the book is halted, and its positions are not
			// known. The bucket is still listed, at the ledger cash NewRunner3 read for it, exactly as
			// Value counts a bucket no engine holds. Left out, Value would count it as unheld with the
			// cash the capital query zeroes for buckets held elsewhere, and the live page would show
			// the bucket as worth nothing. That cash is still the ledger's: v3 writes nothing before
			// a rebuild has succeeded. An open bet it may hold is not counted, as for an unheld bucket.
			out.Buckets = append(out.Buckets, BucketBook{BucketID: b.ID, Name: b.Name, Strategy: b.Strategy, Engine: "v3", World: "real",
				Version: version3, Life: store.LifeOf(b.Name), CashCents: b.CashCents})
			continue
		}
		bb := BucketBook{BucketID: b.ID, Name: b.Name, Strategy: b.Strategy, Engine: "v3", World: "real", Version: version3,
			Life: store.LifeOf(b.Name), CashCents: a.CashCents, Bets: a.Bets}
		for _, pos := range a.Open() {
			bid := 0.0
			if q, ok := r.quotes[pos.Ticker]; ok {
				bid = f(q.q.YesBid)
				if pos.Side != "yes" {
					bid = f(q.q.NoBid)
				}
			}
			p := Position{Strategy: b.Strategy, Engine: "v3", World: "real", Coin: pos.Coin, Side: upDown(pos.Side), Ticker: pos.Ticker,
				Contracts: pos.Contracts, CostCents: pos.CostCents, Placed: pos.FirstAt, Closes: pos.Close,
				Underlying: r.entryPrice[entryKey(b.ID, pos.Ticker, pos.Side)]}
			if pos.Contracts > 0 {
				p.EntryPrice = float64(pos.PremiumCents) / 100 / float64(pos.Contracts) // the average, fee left out, as the other engines show it
			}
			bb.AtRiskCents += p.CostCents
			if v, ok := markCents(pos.Contracts, bid); ok {
				p.ValueCents, bb.MarkedCents = &v, bb.MarkedCents+v
			} else {
				bb.Unmarked++
			}
			out.Positions = append(out.Positions, p)
		}
		out.Buckets = append(out.Buckets, bb)
	}
	return out
}

// Markers lists one coin's v3 bets and early sales made at or after `since`. They are kept in
// memory only (the engine keeps no bet log: the ledger is its record), so a restart empties them.
// A panic in here is stopped under the lock like one in Book, and returns no markers.
func (r *Runner3) Markers(coin string, since float64) (out []Marker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() {
		if p := recover(); p != nil {
			r.panickedLocked("Markers", p)
			out = nil
		}
	}()
	for _, m := range r.markers {
		if m.Coin == coin && m.T >= since {
			out = append(out, m)
		}
	}
	return out
}
