package analysis

import (
	"math"
	"testing"
)

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-3 }

// Five clocks: two where the roster made a real choice, one whose owner would not have bought,
// one in warmup, one where only the owner would have bought.
func danceShadows() []Shadow {
	w := PickWindowName
	return []Shadow{
		// 900: A owned it and won, B lost. B's row was seen later under a moved election: A stays owner.
		{Member: "A", Close: 900, Seen: 500, Owner: "A", OwnerHow: w, Cost: 70, PnL: 30},
		{Member: "B", Close: 900, Seen: 600, Owner: "B", OwnerHow: w, Cost: 60, PnL: -60},
		// 1800: B owned it and lost; A and C won, C most.
		{Member: "A", Close: 1800, Seen: 1400, Owner: "B", OwnerHow: w, Cost: 80, PnL: 20},
		{Member: "B", Close: 1800, Seen: 1400, Owner: "B", OwnerHow: w, Cost: 50, PnL: -50},
		{Member: "C", Close: 1800, Seen: 1400, Owner: "B", OwnerHow: w, Cost: 40, PnL: 60},
		// 2700: C owned it but would not have bought.
		{Member: "A", Close: 2700, Seen: 2300, Owner: "C", OwnerHow: w, Cost: 70, PnL: 30},
		// 3600: warmup.
		{Member: "A", Close: 3600, Seen: 3200, Owner: "A", OwnerHow: "warmup", Cost: 70, PnL: -70},
		{Member: "B", Close: 3600, Seen: 3200, Owner: "A", OwnerHow: "warmup", Cost: 60, PnL: 40},
		// 4500: only the owner.
		{Member: "A", Close: 4500, Seen: 4100, Owner: "A", OwnerHow: w, Cost: 50, PnL: 50},
	}
}

func TestDanceMeasuresTheElection(t *testing.T) {
	d := Dance(danceShadows())
	if d.Clocks != 5 || d.Elected != 4 || d.Choices != 2 || d.Since != 900 {
		t.Fatalf("clocks %d elected %d choices %d since %d", d.Clocks, d.Elected, d.Choices, d.Since)
	}
	a1, b1 := 30.0/70, -1.0
	a2, b2, c2 := 20.0/80, -1.0, 60.0/40
	field := []float64{a1 - b1, b2 - (a2+c2)/2}
	split := []float64{a1 - (a1+b1)/2, b2 - (a2+b2+c2)/3}
	if s := d.OwnerVsField; s.N != 2 || !nearly(s.Mean, (field[0]+field[1])/2) || s.Verdict != "unresolved" {
		t.Errorf("owner v. field %+v", s)
	}
	if s := d.OwnerVsSplit; !nearly(s.Mean, (split[0]+split[1])/2) {
		t.Errorf("owner v. split %+v", s)
	}
	if !nearly(d.BestShare, 0.5) || !nearly(d.ChanceShare, (1.0/2+1.0/3)/2) {
		t.Errorf("best %v chance %v", d.BestShare, d.ChanceShare)
	}
	if len(d.Series) != 2 || !nearly(d.Series[1][1], a1+b2) || !nearly(d.Series[1][2], (a1+b1)/2+(a2+b2+c2)/3) {
		t.Errorf("series %v", d.Series)
	}
	byName := map[string]DanceMember{}
	for _, m := range d.Members {
		byName[m.Member] = m
	}
	a := byName["A"]
	if a.Clocks != 5 || a.Shadows != 5 || a.Owned != 2 || !nearly(a.OwnedPerDollar, (30.0+50)/(70+50)) || !nearly(a.PerDollar, (30.0+20+30-70+50)/(70+80+70+70+50)) {
		t.Errorf("A %+v", a)
	}
	if b := byName["B"]; b.Owned != 1 || b.OwnedPerDollar != -1 || b.Clocks != 3 {
		t.Errorf("B %+v", b)
	}
	if c := byName["C"]; c.Owned != 1 || c.OwnedPerDollar != 0 || c.Clocks != 1 {
		t.Errorf("C, which owned a clock it would not have bought in: %+v", c)
	}
	if len(d.MemberSeries["A"]) != 5 || !nearly(d.MemberSeries["A"][4][1], a1+a2+30.0/70-1+1) {
		t.Errorf("A's line %v", d.MemberSeries["A"])
	}
}

// Enough clocks where the owner wins and the rest lose: a verdict, in the owner's favour.
func TestDanceVerdict(t *testing.T) {
	var sh []Shadow
	for i := 0; i < 40; i++ {
		cl := int64(900 * (i + 1))
		win := int64(20 + 10*(i%2))
		sh = append(sh, Shadow{Member: "A", Close: cl, Seen: float64(cl - 300), Owner: "A", OwnerHow: PickWindowName, Cost: 70, PnL: win},
			Shadow{Member: "B", Close: cl, Seen: float64(cl - 300), Owner: "A", OwnerHow: PickWindowName, Cost: 60, PnL: -60})
	}
	d := Dance(sh)
	if d.OwnerVsField.N != 40 || d.OwnerVsField.Verdict != "owner better" || d.BestShare != 1 {
		t.Errorf("%+v best %v", d.OwnerVsField, d.BestShare)
	}
	if Dance(nil) != nil {
		t.Error("no shadows is no measurement")
	}
}
