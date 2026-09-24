package exercise

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// The protocol guard. docs/v3-measurement-protocol.md fixes TRAIN as the first 480 eligible
// 15-minute windows after t0, an embargo after them, and TEST after that, to be looked at ONCE by
// cmd/measure3. A shape's P&L over TEST windows is not one of the protocol's statistics, but a
// lambda chosen by reading it there would be a lambda chosen on TEST, which is the thing the
// protocol exists to prevent. So this tool refuses any 15-minute-round market that closes after
// TRAIN's last close until research/v3/test-result.json exists, which the Pi's MCP process
// cannot see: the owner says so by setting EnvPastTrain in the process's environment once the
// look has been taken and committed (the owner's decision, 2026-09-23). The ladders are under
// no protocol and are not guarded.
//
// TRAIN's end is found by the protocol's own existence query (section 1), which returns market
// ids and no probability, quote or result as a value, and which the protocol says is not a look.
// The constants below are the protocol's; cmd/measure3 has the same ones and is the authority.

const (
	// EnvPastTrain, non-empty in the MCP process's environment, lets a replay read windows past
	// TRAIN's end. Set it only after research/v3/test-result.json is committed.
	EnvPastTrain = "AC_EXERCISE_PAST_TRAIN"

	trainWindows  = 480
	completeAfter = 15 * time.Minute // a window is judged only once its close is this old
)

// t0 is the protocol's second amendment (section 1): from then on every BTC and ETH snapshot
// carries the forecast.
var t0 = time.Date(2026, 9, 22, 4, 17, 4, 0, time.UTC)

// protocolSymbols are the five series the protocol's queries name.
var protocolSymbols = []string{"KXBTC15M", "KXETH15M", "KXSOL15M", "KXXRP15M", "KXDOGE15M"}

// TrainEnd is TRAIN's last close, when TRAIN has its 480 eligible windows; complete is false while
// it has fewer, in which case every settled window so far is TRAIN's or not yet eligible, and
// nothing past TRAIN can be read anyway.
func TrainEnd(ctx context.Context, q store.Querier, now time.Time) (end time.Time, complete bool, err error) {
	// Coverage: s_c per symbol, the first snapshot carrying the forecast key.
	cov := map[string]time.Time{}
	rows, err := q.Query(ctx, `
		select i.symbol, min(e.at)
		  from evaluation e join market m on m.id = e.market_id join instrument i on i.id = m.instrument_id
		 where e.at >= $1 and i.symbol = any($2) and e.model ? 'v3'
		 group by i.symbol`, t0, protocolSymbols)
	if err != nil {
		return end, false, fmt.Errorf("coverage: %w", err)
	}
	for rows.Next() {
		var sym string
		var at time.Time
		if err := rows.Scan(&sym, &at); err != nil {
			rows.Close()
			return end, false, err
		}
		cov[sym] = at.UTC()
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return end, false, err
	}
	if len(cov) == 0 {
		return end, false, nil // no coin carries the forecast: TRAIN has not begun
	}

	// The existence query of section 1: markets with a scored row, ids only.
	scored := map[int64]bool{}
	rows, err = q.Query(ctx, `
		select distinct e.market_id
		  from evaluation e join market m on m.id = e.market_id join instrument i on i.id = m.instrument_id
		 where e.at >= $1 and i.symbol = any($2)
		   and m.result in ('yes','no')
		   and (e.model->'v3'->>'ok')::boolean and e.model->'v3'->>'p_model' is not null
		   and (e.quotes->>'yes_bid')::numeric > 0 and (e.quotes->>'no_bid')::numeric > 0`, t0, protocolSymbols)
	if err != nil {
		return end, false, fmt.Errorf("existence query: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return end, false, err
		}
		scored[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return end, false, err
	}

	// Every market of the five series closing after t0.
	var markets []guardMarket
	rows, err = q.Query(ctx, `
		select m.id, i.symbol, m.closes_at, coalesce(m.result, '')
		  from market m join instrument i on i.id = m.instrument_id
		 where i.symbol = any($1) and m.closes_at > $2
		 order by m.closes_at, i.symbol`, protocolSymbols, t0)
	if err != nil {
		return end, false, fmt.Errorf("markets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m guardMarket
		var id int64
		if err := rows.Scan(&id, &m.Symbol, &m.Closes, &m.Result); err != nil {
			return end, false, err
		}
		m.Closes, m.Scored = m.Closes.UTC(), scored[id]
		markets = append(markets, m)
	}
	if err := rows.Err(); err != nil {
		return end, false, err
	}
	end, complete = trainEndOf(cov, markets, now)
	return end, complete, nil
}

// guardMarket is one market as the walk judges it.
type guardMarket struct {
	Symbol string
	Closes time.Time
	Result string
	Scored bool
}

// trainEndOf is the protocol's walk (section 1) on rows already read: windows in close order,
// each eligible when every coin covered in it (s_c before the close) has a market with a yes/no
// result and a scored row, and at least one coin is covered; a window closed under 15 minutes
// ago is not judged. The 480th eligible window's close is TRAIN's end.
func trainEndOf(cov map[string]time.Time, markets []guardMarket, now time.Time) (end time.Time, complete bool) {
	windows := map[int64]map[string]guardMarket{}
	for _, m := range markets {
		if now.Sub(m.Closes) < completeAfter {
			continue
		}
		w := m.Closes.Unix()
		if windows[w] == nil {
			windows[w] = map[string]guardMarket{}
		}
		windows[w][m.Symbol] = m
	}
	keys := make([]int64, 0, len(windows))
	for w := range windows {
		keys = append(keys, w)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	eligible := 0
	for _, w := range keys {
		closes := time.Unix(w, 0).UTC()
		ok := false
		for _, sym := range protocolSymbols {
			sc, covered := cov[sym]
			if !covered || !sc.Before(closes) {
				continue
			}
			m, has := windows[w][sym]
			if !has || (m.Result != "yes" && m.Result != "no") || !m.Scored {
				ok = false
				break
			}
			ok = true
		}
		if !ok {
			continue
		}
		eligible++
		if eligible == trainWindows {
			return closes, true
		}
	}
	return end, false
}

// Guard refuses a replay of 15-minute rounds that reaches past TRAIN's end, unless the owner has
// said the look is taken. markets are the ones the window selected. getenv reads the process's
// environment (os.Getenv in the server; a stub in tests).
func Guard(ctx context.Context, q store.Querier, family string, markets []MarketRow, now time.Time, getenv func(string) string) error {
	if family != engine.FamilyRounds || len(markets) == 0 {
		return nil
	}
	end, complete, err := TrainEnd(ctx, q, now)
	if err != nil {
		return fmt.Errorf("the protocol guard could not be evaluated: %w", err)
	}
	return refuseIf(family, markets, end, complete, getenv)
}

// refuseIf is Guard's rule on figures already read.
func refuseIf(family string, markets []MarketRow, end time.Time, complete bool, getenv func(string) string) error {
	if family != engine.FamilyRounds || len(markets) == 0 || !complete {
		return nil
	}
	last := markets[len(markets)-1].Closes
	for _, m := range markets {
		if m.Closes.After(last) {
			last = m.Closes
		}
	}
	if !last.After(end) || getenv(EnvPastTrain) != "" {
		return nil
	}
	return fmt.Errorf("refused: the window reaches %s, after TRAIN's last close (%s); rounds closing after it are the protocol's embargo and TEST, "+
		"and a shape read on them is a shape chosen on TEST. Ask for a window ending at or before %s. Once research/v3/test-result.json is committed the owner "+
		"sets %s in the MCP process's environment on the Pi and this refusal lifts",
		last.Format(time.RFC3339), end.Format(time.RFC3339), end.Add(time.Second).Format(time.RFC3339), EnvPastTrain)
}
