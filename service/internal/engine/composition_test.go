package engine

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func testMember(t *testing.T, name string, tauMin, tauMax, bandMin, lambda float64) Params {
	t.Helper()
	p, err := FromShape(Shape{Name: name, Exit: "hold", Lambda: lambda, StaleCost: 0.0012,
		TauMin: tauMin, TauMax: tauMax, BandMin: bandMin})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func rosterAB(t *testing.T, assign string, lookback int, structural bool) *Composition {
	t.Helper()
	a := testMember(t, "A", 300, 600, 0.4, 0.5)
	b := testMember(t, "B", 0, 150, 0.4, 0.5)
	return &Composition{Name: "Roster", Members: []Params{a, b}, Lookback: lookback, StructuralOnly: structural, Assign: assign, reserved: map[string]reservation{}}
}

func fillClocks(c *Composition, n int, aPnL, bPnL int64) {
	for i := 0; i < n; i++ {
		close := float64(1000 + i*900)
		c.settled = append(c.settled,
			ShadowSettled{Member: c.Members[0].Name, Close: close, Cost: 70, PnL: aPnL},
			ShadowSettled{Member: c.Members[1].Name, Close: close, Cost: 70, PnL: bPnL},
		)
	}
}

func TestAssignSitOutWarmupWindowReserveAndNoLookAhead(t *testing.T) {
	c := rosterAB(t, AssignBoth, 16, false)
	const now, tau, ticker = 20000.0, 400.0, "T-new"
	window := now + tau

	if idx, how := c.Pick(ticker, window, now, tau, nil); idx != -1 || how != PickSitOut {
		t.Fatalf("nobody eligible: %d %s", idx, how)
	}

	// Not yet K clocks: owner is roster[0] (warmup). A eligible → warmup claim.
	if idx, how := c.Pick("T-warm", window, now, tau, []int{0, 1}); idx != 0 || how != PickWarmup {
		t.Fatalf("warmup owner claims: %d %s", idx, how)
	}

	fillClocks(c, 16, -70, 30) // B has been winning
	// A future clock that would flip A into the lead must not be visible.
	c.settled = append(c.settled, ShadowSettled{Member: c.Members[0].Name, Close: now + 5000, Cost: 70, PnL: 30})

	owner, how := c.WindowOwner(window, now)
	if owner != 1 || how != PickWindow {
		t.Fatalf("adaptive owner should be B: %d %s", owner, how)
	}
	// The future settlement is invisible at now.
	if idx, _ := c.WindowOwner(window, now); idx != 1 {
		t.Fatalf("a settlement after now must not elect A: %d", idx)
	}

	// Structural-only ignores the shadows: owner is A.
	c.StructuralOnly = true
	if idx, how := c.WindowOwner(window, now); idx != 0 || how != PickWindow {
		t.Fatalf("structural owner: %d %s", idx, how)
	}
}

func TestLateOwnerBlocksFavourite(t *testing.T) {
	c := rosterAB(t, AssignBoth, 16, false)
	fillClocks(c, 16, -70, 30) // B (Late) wins → owns the next clock
	now, tau, window := 20000.0, 400.0, 20400.0
	if idx, _ := c.WindowOwner(window, now); idx != 1 {
		t.Fatalf("Late should own: %d", idx)
	}
	// Favourite (A) would buy at 400s; Late would not. A is not later than Late.
	if idx, how := c.Pick("BTC-late-clock", window, now, tau, []int{0}); idx != -1 || how != PickSitOut {
		t.Fatalf("Favourite must not steal a Late-owned clock: %d %s", idx, how)
	}
}

func TestFavouriteOwnerAllowsLateLeftover(t *testing.T) {
	c := rosterAB(t, AssignBoth, 16, true) // structural: owner is A
	now, tau, window := 20000.0, 100.0, 20100.0
	if idx, how := c.Pick("ETH-flat", window, now, tau, []int{1}); idx != 1 || how != PickReserve {
		t.Fatalf("Late should reserve the leftover: %d %s", idx, how)
	}
}

func TestReservationLock(t *testing.T) {
	c := rosterAB(t, AssignBoth, 16, true)
	now, window := 20000.0, 20400.0
	if idx, how := c.Pick("LOCK", window, now, 400, []int{0}); idx != 0 || how != PickWindow {
		t.Fatalf("owner claims: %d %s", idx, how)
	}
	if idx, how := c.Pick("LOCK", window, now, 100, []int{1}); idx != -1 || how != PickSitOut {
		t.Fatalf("locked ticker must not move to Late: %d %s", idx, how)
	}
	if idx, how := c.Pick("LOCK", window, now, 400, []int{0}); idx != 0 || how != PickWindow {
		t.Fatalf("owner still holds the lock: %d %s", idx, how)
	}
}

func TestAssignWindowHasNoLeftovers(t *testing.T) {
	c := rosterAB(t, AssignWindow, 16, true)
	if idx, how := c.Pick("X", 20100, 20000, 100, []int{1}); idx != -1 || how != PickSitOut {
		t.Fatalf("window-only must not reserve Late: %d %s", idx, how)
	}
}

func TestAssignReserveWaitsForLatest(t *testing.T) {
	c := rosterAB(t, AssignReserve, 0, false)
	// At 400s Late's window has not started: sit even if A would buy.
	if idx, how := c.Pick("R1", 20400, 20000, 400, []int{0}); idx != -1 || how != PickSitOut {
		t.Fatalf("reserve must wait: %d %s", idx, how)
	}
	if idx, how := c.Pick("R2", 20100, 20000, 100, []int{1}); idx != 1 || how != PickReserve {
		t.Fatalf("Late may enter in their clock: %d %s", idx, how)
	}
}

func TestFromShapeRoster(t *testing.T) {
	p, err := FromShape(Shape{
		Name: "Dance", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012,
		Members: []Member{
			{Name: "Favourite", Exit: "hold", Lambda: 1, StaleCost: 0.0012, Side: SideFavourite, BandMin: 0.70, BandMax: 0.90, TauMin: 300, TauMax: 600},
			{Name: "Value no-longshot", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, BandMin: 0.40, TauMax: 600},
			{Name: "Late model", Exit: "hold", Lambda: 0.8, StaleCost: 0.0012, BandMin: 0.40, TauMax: 150},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasComposition() || p.LookbackWindows != DefaultLookback || p.StructuralOnly || p.Assign != AssignBoth || len(p.Members) != 3 || p.Members[0].Side != SideFavourite {
		t.Fatalf("roster: assign %q lookback %d %+v", p.Assign, p.LookbackWindows, p.Members)
	}
	if p.Provenance["lookback_windows"].Kind != KindConvention || !strings.Contains(p.Blurb, "roster of 3") {
		t.Fatalf("labelled: %q %+v", p.Blurb, p.Provenance["lookback_windows"])
	}
	back := ToShape(p)
	if len(back.Members) != 3 || back.LookbackWindows != DefaultLookback || back.StructuralOnly || back.Assign != AssignBoth {
		t.Fatalf("ToShape: %+v", back)
	}
	q, err := FromShape(back)
	if err != nil || !q.HasComposition() || q.Assign != AssignBoth || q.Members[0].Side != SideFavourite || q.Members[2].TauMax != 150 {
		t.Fatalf("round-trip: %v %+v", err, q)
	}

	only, err := FromShape(Shape{
		Name: "Dance structural", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, StructuralOnly: true,
		Members: []Member{
			{Name: "Favourite", Exit: "hold", Lambda: 1, StaleCost: 0.0012, TauMin: 300, TauMax: 600},
			{Name: "Late", Exit: "hold", Lambda: 0.8, StaleCost: 0.0012, TauMax: 150},
		},
	})
	if err != nil || !only.StructuralOnly || only.LookbackWindows != 0 || only.Assign != AssignBoth {
		t.Fatalf("structural only: %v %+v", err, only)
	}

	res, err := FromShape(Shape{
		Name: "Dance reserve", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, Assign: AssignReserve,
		Members: []Member{
			{Name: "Favourite", Exit: "hold", Lambda: 1, StaleCost: 0.0012, TauMin: 300, TauMax: 600},
			{Name: "Late", Exit: "hold", Lambda: 0.8, StaleCost: 0.0012, TauMax: 150},
		},
	})
	if err != nil || res.Assign != AssignReserve || res.LookbackWindows != 0 {
		t.Fatalf("reserve: %v %+v", err, res)
	}

	for _, bad := range []Shape{
		{Name: "x", Exit: "hold", Lambda: 0.5, Members: []Member{{Name: "only", Exit: "hold", Lambda: 0.5}}},
		{Name: "x", Exit: "ev", Lambda: 0.5, StaleCost: 0.0012, StaleCostSell: 0.0012,
			Members: []Member{
				{Name: "a", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012},
				{Name: "b", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012},
			}},
		{Name: "x", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012,
			Members: []Member{
				{Name: "a", Exit: "ev", Lambda: 0.5, StaleCost: 0.0012, StaleCostSell: 0.0012},
				{Name: "b", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012},
			}},
		{Name: "x", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, Assign: "dance",
			Members: []Member{
				{Name: "a", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012},
				{Name: "b", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012},
			}},
		{Name: "x", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, Assign: AssignReserve, LookbackWindows: 16,
			Members: []Member{
				{Name: "a", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012},
				{Name: "b", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012},
			}},
	} {
		if _, err := FromShape(bad); err == nil {
			t.Fatalf("must refuse %+v", bad)
		}
	}
}

func TestShadowSettleIsCausal(t *testing.T) {
	c := &Composition{Lookback: 2, Members: []Params{{Name: "A"}, {Name: "B"}}}
	c.Observe("A", Market{Ticker: "T1", Close: 100}, "yes", 70)
	c.Observe("B", Market{Ticker: "T1", Close: 100}, "yes", 70)
	if _, n := c.windowScore("A", 200, 99); n != 0 {
		t.Fatal("pending is not a score")
	}
	c.Settle("T1", "yes")
	if r, n := c.windowScore("A", 200, 100); n != 1 || r <= 0 {
		t.Fatalf("A won at the close: r=%v n=%d", r, n)
	}
	if _, n := c.windowScore("A", 200, 99); n != 0 {
		t.Fatal("a settlement whose close is after now is invisible")
	}
	if _, n := c.windowScore("A", 100, 100); n != 0 {
		t.Fatal("this clock's own close must not elect it")
	}
}

// The memory survives being saved and read back: the same owner, the same claim on a ticker
// handed out before, and nothing that names a member the roster does not have.
func TestRosterMemoryRoundTrip(t *testing.T) {
	c := rosterAB(t, AssignBoth, 4, false)
	fillClocks(c, 6, -70, 30)
	c.Observe(c.Members[0].Name, Market{Ticker: "T-open", MarketID: 9, Close: 7000}, "yes", 60)
	idx, how := c.Pick("T-open", 7000, 6500, 500, []int{0, 1})
	if idx < 0 {
		t.Fatalf("nobody claimed: %s", how)
	}
	mem := c.Memory()
	blob, err := json.Marshal(mem)
	if err != nil {
		t.Fatal(err)
	}
	var back RosterMemory
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	back.Settled = append(back.Settled, ShadowSettled{Member: "Z", Close: 1000, Cost: 70, PnL: 30})
	back.Pending = append(back.Pending, ShadowPending{Member: "Z", Ticker: "T-z", Close: 7000, Cost: 50})
	back.Reserved["T-z"] = RosterClaim{Member: "Z", How: PickWindow, Close: 7000}

	d := rosterAB(t, AssignBoth, 4, false)
	d.Restore(back)
	if !reflect.DeepEqual(d.Memory(), mem) {
		t.Fatalf("restored\n %+v\nsaved\n %+v", d.Memory(), mem)
	}
	if !d.TakeDirty() || d.TakeDirty() {
		t.Fatal("a restore is a change to save, once")
	}
	o1, h1 := c.WindowOwner(7900, 7000)
	o2, h2 := d.WindowOwner(7900, 7000)
	if o1 != o2 || h1 != h2 || h2 != PickWindow {
		t.Fatalf("owner %d %s before, %d %s after", o1, h1, o2, h2)
	}
	if i, h := d.Pick("T-open", 7000, 6600, 400, []int{0, 1}); i != idx || h != how {
		t.Fatalf("the claim moved: %d %s, was %d %s", i, h, idx, how)
	}
}

// Prune keeps every clock a pick can still read, so no owner changes; it drops reservations on
// closed tickers and, after shadowGiveUp, shadows whose result never came, and counts those.
func TestPruneKeepsWhatAPickReads(t *testing.T) {
	c := rosterAB(t, AssignBoth, 4, false)
	nameA, nameB := c.Members[0].Name, c.Members[1].Name
	for i := 0; i < 10; i++ { // B leads early, A the last five clocks
		close := float64(1000 + i*900)
		a, b := int64(-70), int64(30)
		if i >= 5 {
			a, b = 30, -70
		}
		c.settled = append(c.settled, ShadowSettled{Member: nameA, Close: close, Cost: 70, PnL: a})
		if i%2 == 0 { // B sits out every other clock: its own latest four reach further back
			c.settled = append(c.settled, ShadowSettled{Member: nameB, Close: close, Cost: 70, PnL: b})
		}
	}
	now := 1000 + 9*900 + 60.0
	c.reserved["T-closed"] = reservation{idx: 1, how: PickReserve, close: now - 60}
	c.reserved["T-open"] = reservation{idx: 0, how: PickWindow, close: now + 840}
	c.pending = append(c.pending,
		ShadowPending{Member: nameA, Ticker: "T-waiting", Close: now - 60, Cost: 50},
		ShadowPending{Member: nameB, Ticker: "T-lost", Close: now - shadowGiveUp - 1, Cost: 50})
	type seen struct {
		idx int
		how string
	}
	owners := func() (out []seen) {
		for _, w := range []float64{now + 840, now + 1740} {
			for _, at := range []float64{now, now + 900} {
				i, h := c.WindowOwner(w, at)
				out = append(out, seen{i, h})
			}
		}
		return out
	}
	before := owners()
	if before[0] != (seen{0, PickWindow}) {
		t.Fatalf("A leads the last four clocks and owns the next: %v", before[0])
	}
	c.TakeDirty()
	if n := c.Prune(now); n != 1 {
		t.Fatalf("dropped %d unscored, want the one past shadowGiveUp", n)
	}
	if after := owners(); !reflect.DeepEqual(before, after) {
		t.Fatalf("owners %v before, %v after", before, after)
	}
	if _, ok := c.reserved["T-closed"]; ok {
		t.Fatal("a closed ticker's reservation was kept")
	}
	if _, ok := c.reserved["T-open"]; !ok {
		t.Fatal("an open ticker's reservation was dropped")
	}
	if len(c.pending) != 1 || c.pending[0].Ticker != "T-waiting" {
		t.Fatalf("pending %+v", c.pending)
	}
	per := map[string]int{}
	for _, s := range c.settled {
		per[s.Member]++
	}
	if per[nameA] != 4 || per[nameB] != 4 {
		t.Fatalf("kept per member %v, want each member's latest four", per)
	}
	if !c.TakeDirty() {
		t.Fatal("a prune that dropped something is a change to save")
	}
	if c.Prune(now); c.TakeDirty() {
		t.Fatal("a prune that dropped nothing is not a change")
	}
}

// Only yes or no scores a shadow; anything else leaves it waiting.
func TestShadowWaitsOnAResultThatIsNotYesOrNo(t *testing.T) {
	c := &Composition{Lookback: 2, Members: []Params{{Name: "A"}, {Name: "B"}}}
	c.Observe("A", Market{Ticker: "T1", Close: 100}, "yes", 70)
	for _, result := range []string{"scalar", ""} {
		c.Settle("T1", result)
		if len(c.pending) != 1 || len(c.settled) != 0 {
			t.Fatalf("%q scored the shadow", result)
		}
	}
	c.Settle("T1", "no")
	if len(c.pending) != 0 || len(c.settled) != 1 || c.settled[0].PnL != -70 {
		t.Fatalf("pending %+v settled %+v", c.pending, c.settled)
	}
}

// A shadow is queued for the record as it is scored, with when it was first seen and who owned
// the clock then; one dropped unscored is queued too, with no result. The queue rides in the
// memory until the runner takes it, and a record whose writes keep failing is given up.
func TestShadowsAreQueuedForTheRecord(t *testing.T) {
	c := rosterAB(t, AssignBoth, 1, false)
	a, b := c.Members[0].Name, c.Members[1].Name
	c.ObserveAt(a, Market{Ticker: "T1", MarketID: 7, Close: 900}, "yes", 72, b, PickWindow, 500)
	c.ObserveAt(b, Market{Ticker: "T1", MarketID: 7, Close: 900}, "no", 30, b, PickWindow, 510)
	c.ObserveAt(a, Market{Ticker: "T2", MarketID: 8, Close: 900}, "yes", 60, b, PickWindow, 520)
	c.Settle("T1", "yes")
	c.Prune(900 + shadowGiveUp + 1) // T2 never had a result
	want := []ShadowRecord{
		{Member: a, MarketID: 7, Ticker: "T1", Side: "yes", Close: 900, Seen: 500, Owner: b, OwnerHow: PickWindow, Cost: 72, Result: "yes", PnL: 28},
		{Member: b, MarketID: 7, Ticker: "T1", Side: "no", Close: 900, Seen: 510, Owner: b, OwnerHow: PickWindow, Cost: 30, Result: "yes", PnL: -30},
		{Member: a, MarketID: 8, Ticker: "T2", Side: "yes", Close: 900, Seen: 520, Owner: b, OwnerHow: PickWindow, Cost: 60},
	}
	mem := c.Memory()
	if !reflect.DeepEqual(mem.Unrecorded, want) {
		t.Fatalf("queued\n %+v\nwant\n %+v", mem.Unrecorded, want)
	}
	// Saved and restored, the queue comes back; a record naming no member of this roster does not.
	blob, _ := json.Marshal(mem)
	var back RosterMemory
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	back.Unrecorded = append(back.Unrecorded, ShadowRecord{Member: "Z", MarketID: 9, Side: "yes", Cost: 50})
	d := rosterAB(t, AssignBoth, 1, false)
	d.Restore(back)
	if !reflect.DeepEqual(d.Memory().Unrecorded, want) {
		t.Fatalf("restored %+v", d.Memory().Unrecorded)
	}
	// Taken, the queue is empty and the memory is marked changed, so the save drops them too.
	c.TakeDirty()
	recs := c.TakeRecords()
	if len(recs) != 3 || len(c.TakeRecords()) != 0 || !c.TakeDirty() {
		t.Fatalf("took %d", len(recs))
	}
	// Given back after each failed write, then given up.
	for try := 1; try < maxRecordTries; try++ {
		if dropped := c.GiveBack(recs); dropped != 0 {
			t.Fatalf("try %d dropped %d", try, dropped)
		}
		recs = c.TakeRecords()
		if len(recs) != 3 || recs[0].Tries != try {
			t.Fatalf("try %d: %+v", try, recs)
		}
	}
	if dropped := c.GiveBack(recs); dropped != 3 || len(c.TakeRecords()) != 0 {
		t.Fatalf("after %d tries dropped %d", maxRecordTries, dropped)
	}
}

// Memory saved before the shadows were recorded has no time seen and no owner: it still restores,
// and its shadows are recorded without them.
func TestOldRosterMemoryStillRecords(t *testing.T) {
	c := rosterAB(t, AssignBoth, 1, false)
	var old RosterMemory
	if err := json.Unmarshal([]byte(`{"pending":[{"member":"`+c.Members[0].Name+`","ticker":"T","market_id":3,"side":"yes","close":900,"cost":50}]}`), &old); err != nil {
		t.Fatal(err)
	}
	c.Restore(old)
	c.Settle("T", "no")
	recs := c.TakeRecords()
	if len(recs) != 1 || recs[0].Seen != 0 || recs[0].Owner != "" || recs[0].Result != "no" || recs[0].PnL != -50 || recs[0].MarketID != 3 {
		t.Fatalf("%+v", recs)
	}
}

// A roster's members trade the parent's markets: app/builder stamps the bucket with the parent's
// family and that family's runner holds it, so a member of another family would run on the
// wrong tape. Every member's family must be the parent's, through both doors (FromShape and a
// stored Params re-validated); a blank member family reads as the rounds, like the parent's.
func TestRosterMembersShareParentFamily(t *testing.T) {
	rounds := func(name string) Member {
		return Member{Name: name, Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, TauMax: 600}
	}
	ladder := func(name string) Member {
		return Member{Name: name, Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, Family: FamilyLadders, TauMin: 10800, TauMax: 108000}
	}
	spot := func(name string) Member {
		return Member{Name: name, Exit: "hold", Family: FamilySpot}
	}
	refuse := func(s Shape, member, family, parent string) {
		t.Helper()
		_, err := FromShape(s)
		if err == nil {
			t.Fatalf("must refuse %+v", s)
		}
		for _, want := range []string{member, family, parent} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal names the member, its family and the roster's; %q is missing from: %v", want, err)
			}
		}
	}
	roundsParent := Shape{Name: "Mixed", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012}
	ladderParent := Shape{Name: "Mixed", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, Family: FamilyLadders, TauMin: 10800, TauMax: 108000}

	// A rounds parent with spot members. Each member alone passes FromShape (spot skips R1, so
	// its lambda is 0); without this check the bucket would go to the rounds runner carrying two
	// lambda-0 members, which R1 forbids.
	s := roundsParent
	s.Members = []Member{spot("Coin A"), spot("Coin B")}
	refuse(s, "Coin A", FamilySpot, FamilyRounds)
	// A rounds parent with ladder members.
	s = roundsParent
	s.Members = []Member{ladder("Day A"), ladder("Day B")}
	refuse(s, "Day A", FamilyLadders, FamilyRounds)
	// One stray member is enough, in either direction.
	s = roundsParent
	s.Members = []Member{rounds("Round A"), ladder("Day B")}
	refuse(s, "Day B", FamilyLadders, FamilyRounds)
	s = ladderParent
	s.Members = []Member{ladder("Day A"), rounds("Round B")}
	refuse(s, "Round B", FamilyRounds, FamilyLadders)

	// A ladder parent with ladder members is a roster, and it round-trips through the builder.
	s = ladderParent
	s.Name = "Day dance"
	s.Members = []Member{ladder("Day A"), func() Member { m := ladder("Day B"); m.TauMax = 43200; return m }()}
	lad, err := FromShape(s)
	if err != nil {
		t.Fatal(err)
	}
	if !lad.HasComposition() || lad.FamilyOf() != FamilyLadders || lad.Members[1].Family != FamilyLadders {
		t.Fatalf("ladder roster: %+v", lad)
	}
	back := ToShape(lad)
	if back.Family != FamilyLadders || len(back.Members) != 2 || back.Members[0].Family != FamilyLadders || back.Members[1].TauMax != 43200 {
		t.Fatalf("ToShape: %+v", back)
	}
	if q, err := FromShape(back); err != nil || q.FamilyOf() != FamilyLadders || q.Members[1].TauMax != 43200 {
		t.Fatalf("round-trip: %v %+v", err, q)
	}

	// An all-rounds roster is still one whether a member says kalshi15m or leaves it blank.
	explicit := rounds("Round A")
	explicit.Family = FamilyRounds
	s = roundsParent
	s.Name = "Dance"
	s.Members = []Member{explicit, rounds("Round B")}
	rnd, err := FromShape(s)
	if err != nil || rnd.FamilyOf() != FamilyRounds || !rnd.HasComposition() {
		t.Fatalf("rounds roster: %v %+v", err, rnd)
	}
	// The store's door: a Params re-validated on its own, not through FromShape. A stored
	// roster whose member has drifted to another family is refused the same way.
	if err := rnd.Validate(); err != nil {
		t.Fatal(err)
	}
	drift := rnd
	drift.Members = append([]Member(nil), rnd.Members...)
	drift.Members[1].Family, drift.Members[1].TauMin, drift.Members[1].TauMax = FamilyLadders, 10800, 108000
	err = drift.Validate()
	if err == nil || !strings.Contains(err.Error(), "Round B") || !strings.Contains(err.Error(), FamilyLadders) || !strings.Contains(err.Error(), FamilyRounds) {
		t.Fatalf("a stored roster with a stray member must be refused, naming it: %v", err)
	}
}

// A member the bucket's limits keep from owning (no_owner) is passed over by the election even with
// the best score, and in warmup; it still takes a seat the owner leaves. A name fits with or
// without " (conventions)"; one that fits nobody is reported; a list leaving nobody to own is refused.
func TestPassOverKeepsAMemberFromOwning(t *testing.T) {
	c := rosterAB(t, AssignBoth, 2, false) // A plays 300-600 s out; B, the later specialist, the last 150 s
	fillClocks(c, 3, -10, 30)              // B has the better score
	now := float64(1000 + 3*900)
	if idx, how := c.WindowOwner(now+900, now); idx != 1 || how != PickWindow {
		t.Fatalf("B's score elects it: %d %s", idx, how)
	}
	unknown, applied := c.PassOver([]string{"B", "Nobody"})
	if !applied || len(unknown) != 1 || unknown[0] != "Nobody" {
		t.Fatalf("applied %v unknown %v", applied, unknown)
	}
	if idx, how := c.WindowOwner(now+900, now); idx != 0 || how != PickWindow {
		t.Fatalf("B passed over, A is elected: %d %s", idx, how)
	}
	// A does not claim the ticker; B, later than A, takes the seat.
	if idx, how := c.Pick("T9", now+900, now+800, 100, []int{1}); idx != 1 || how != PickReserve {
		t.Fatalf("B lost its leftovers: %d %s", idx, how)
	}
	// Warmup goes to the first member the limits let own.
	w := rosterAB(t, AssignBoth, 5, false)
	if _, applied := w.PassOver([]string{c.Members[0].Name}); !applied {
		t.Fatal("A by its full name was refused")
	}
	if idx, how := w.WindowOwner(5000, 4000); idx != 1 || how != PickWarmup {
		t.Fatalf("warmup with A passed over: %d %s", idx, how)
	}
	// Nobody left to own: refused, and what was set stays.
	if _, applied := c.PassOver([]string{"A", "B"}); applied {
		t.Fatal("passing over every member was applied")
	}
	if idx, _ := c.WindowOwner(now+900, now); idx != 0 {
		t.Fatalf("a refused list changed the election: %d", idx)
	}
}
