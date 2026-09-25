package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// The calibrator of "What is measured", with the coding and estimator of "Conventions fixed
// before TRAIN":
//
//	logit(p_cal) = a + b·logit(mid) + c·logit(p_model) + Σ_k d_k·1[τ ∈ bin_k]
//	             + e·logit(mid)·1[τ < 120 s] + g·vol_ratio + h·spread + coin effects
//
// An intercept; an indicator per τ bin but (600, 900] and per coin but BTC; every column but the
// intercept standardised with TRAIN's mean and population SD; the summed log-likelihood less
// ½·λ·Σβ² over the standardised columns, λ = 1, the intercept free (scikit-learn's C=1); Newton
// from zero until no coefficient moves by more than 1e-10, within 100 iterations.

const (
	lambdaRidge  = 1.0
	probHold     = 1e-6 // p_model's hold in its logit, and every probability's in a log loss
	newtonTol    = 1e-10
	newtonMaxIt  = 100
	refCoin      = "BTC"
	interactTau  = 120.0 // 1[τ < 120 s], as written
	colIntercept = "intercept"
	colMid       = "logit(mid)"
	colModel     = "logit(p_model)"
)

// column is one term of the design, before standardisation.
type column struct {
	Name string
	f    func(o observation) float64
}

// design is the column list for the coins present on TRAIN.
func design(coins []string) []column {
	cols := []column{
		{colMid, func(o observation) float64 { return logit(mid(o)) }},
		{colModel, func(o observation) float64 { return logit(hold(o.PModel)) }},
	}
	for k := 1; k < len(bins); k++ {
		k := k
		cols = append(cols, column{"tau in " + binLabels[k], func(o observation) float64 { return indicator(o.Bin == k) }})
	}
	cols = append(cols,
		column{"logit(mid) x [tau < 120 s]", func(o observation) float64 { return logit(mid(o)) * indicator(o.Tau < interactTau) }},
		column{"vol_ratio", func(o observation) float64 { return o.VolRatio }},
		column{"spread", func(o observation) float64 { return o.YesAsk - o.YesBid }},
	)
	for _, c := range coins {
		if c == refCoin {
			continue
		}
		c := c
		cols = append(cols, column{"coin " + c, func(o observation) float64 { return indicator(o.Coin == c) }})
	}
	return cols
}

func mid(o observation) float64 { return (o.YesBid + o.YesAsk) / 2 }

func hold(p float64) float64 { return math.Min(math.Max(p, probHold), 1-probHold) }

func logit(p float64) float64 { return math.Log(p / (1 - p)) }

func sigmoid(z float64) float64 {
	if z >= 0 {
		return 1 / (1 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1 + e)
}

func indicator(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// coinsOf is the coins present, sorted.
func coinsOf(obs []observation) []string {
	seen := map[string]bool{}
	for _, o := range obs {
		seen[o.Coin] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// scaler is the frozen standardisation: kept columns with TRAIN's mean and population SD.
type scaler struct {
	Coins   []string  `json:"coins"`
	Columns []string  `json:"columns"` // kept, in order, the intercept not among them
	Mean    []float64 `json:"mean"`
	SD      []float64 `json:"sd"`
	Dropped []string  `json:"dropped_constant_on_train"`
}

func fitScaler(obs []observation) scaler {
	sc := scaler{Coins: coinsOf(obs), Dropped: []string{}}
	for _, c := range design(sc.Coins) {
		var mean float64
		for _, o := range obs {
			mean += c.f(o)
		}
		mean /= float64(len(obs))
		var ss float64
		for _, o := range obs {
			d := c.f(o) - mean
			ss += d * d
		}
		sd := math.Sqrt(ss / float64(len(obs)))
		if !(sd > 0) {
			sc.Dropped = append(sc.Dropped, c.Name)
			continue
		}
		sc.Columns, sc.Mean, sc.SD = append(sc.Columns, c.Name), append(sc.Mean, mean), append(sc.SD, sd)
	}
	return sc
}

// resolve is the kept columns' functions, in order: an error for a name the design does not
// make (a result file that is not this tool's).
func (sc scaler) resolve() ([]column, error) {
	byName := map[string]column{}
	for _, c := range design(sc.Coins) {
		byName[c.Name] = c
	}
	out := make([]column, len(sc.Columns))
	for j, name := range sc.Columns {
		c, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("the standardisation names a column the design does not make: %q", name)
		}
		out[j] = c
	}
	if len(sc.Mean) != len(out) || len(sc.SD) != len(out) {
		return nil, fmt.Errorf("the standardisation has %d columns, %d means and %d SDs", len(out), len(sc.Mean), len(sc.SD))
	}
	return out, nil
}

// matrix is the standardised design, the intercept first.
func (sc scaler) matrix(obs []observation) ([][]float64, error) {
	cols, err := sc.resolve()
	if err != nil {
		return nil, err
	}
	X := make([][]float64, len(obs))
	for i, o := range obs {
		x := make([]float64, 1+len(cols))
		x[0] = 1
		for j, c := range cols {
			x[1+j] = (c.f(o) - sc.Mean[j]) / sc.SD[j]
		}
		X[i] = x
	}
	return X, nil
}

// model is a fitted calibrator.
type model struct {
	Scaler     scaler    `json:"standardisation"`
	Beta       []float64 `json:"beta_standardised"` // intercept first, then Scaler.Columns
	Iterations int       `json:"newton_iterations"`
}

// predict is p_cal for every observation.
func (m model) predict(obs []observation) ([]float64, error) {
	X, err := m.Scaler.matrix(obs)
	if err != nil {
		return nil, err
	}
	if len(m.Beta) != 1+len(m.Scaler.Columns) {
		return nil, fmt.Errorf("%d coefficients for %d columns and the intercept", len(m.Beta), len(m.Scaler.Columns))
	}
	out := make([]float64, len(X))
	for i, x := range X {
		out[i] = sigmoid(dot(x, m.Beta))
	}
	return out, nil
}

var errNoConvergence = errors.New("the fit did not converge within 100 Newton iterations")

// fitRidge maximises Σ[y·η − log(1 + e^η)] − ½·λ·Σ_{j≥1} β_j² by Newton's method from zero.
func fitRidge(X [][]float64, y []float64) ([]float64, int, error) {
	p := len(X[0])
	beta := make([]float64, p)
	for it := 1; it <= newtonMaxIt; it++ {
		g := make([]float64, p)
		H := square(p)
		for i, x := range X {
			mu := sigmoid(dot(x, beta))
			w := mu * (1 - mu)
			for j := 0; j < p; j++ {
				g[j] += x[j] * (y[i] - mu)
				for k := 0; k <= j; k++ {
					H[j][k] += w * x[j] * x[k]
				}
			}
		}
		for j := 0; j < p; j++ {
			for k := j + 1; k < p; k++ {
				H[j][k] = H[k][j]
			}
			if j > 0 { // the penalty: never on the intercept
				g[j] -= lambdaRidge * beta[j]
				H[j][j] += lambdaRidge
			}
		}
		step, err := solveSPD(H, g)
		if err != nil {
			return nil, it, err
		}
		var most float64
		for j := range beta {
			beta[j] += step[j]
			most = math.Max(most, math.Abs(step[j]))
		}
		if most <= newtonTol {
			return beta, it, nil
		}
	}
	return nil, newtonMaxIt, errNoConvergence
}

// clusteredSE is the sandwich A⁻¹ B A⁻¹, A the penalised information at the fit and B the sum
// over close times of each cluster's score outer product. Reported only.
func clusteredSE(X [][]float64, y []float64, beta []float64, cluster []time.Time) ([]float64, error) {
	p := len(beta)
	A := square(p)
	scores := map[time.Time][]float64{}
	for i, x := range X {
		mu := sigmoid(dot(x, beta))
		w := mu * (1 - mu)
		s := scores[cluster[i]]
		if s == nil {
			s = make([]float64, p)
			scores[cluster[i]] = s
		}
		for j := 0; j < p; j++ {
			s[j] += x[j] * (y[i] - mu)
			for k := 0; k < p; k++ {
				A[j][k] += w * x[j] * x[k]
			}
		}
	}
	for j := 1; j < p; j++ {
		A[j][j] += lambdaRidge
	}
	B := square(p)
	for _, s := range scores {
		for j := 0; j < p; j++ {
			for k := 0; k < p; k++ {
				B[j][k] += s[j] * s[k]
			}
		}
	}
	Ainv, err := invertSPD(A)
	if err != nil {
		return nil, err
	}
	V := mulMat(mulMat(Ainv, B), Ainv)
	se := make([]float64, p)
	for j := range se {
		se[j] = math.Sqrt(math.Max(V[j][j], 0))
	}
	return se, nil
}

// unstandardised is a kept column's coefficient on its own scale: β_j / SD_j. ok is false for a
// column that was dropped.
func (m model) unstandardised(name string) (coef float64, ok bool) {
	for j, c := range m.Scaler.Columns {
		if c == name {
			return m.Beta[1+j] / m.Scaler.SD[j], true
		}
	}
	return 0, false
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func square(p int) [][]float64 {
	m := make([][]float64, p)
	for i := range m {
		m[i] = make([]float64, p)
	}
	return m
}

func mulMat(a, b [][]float64) [][]float64 {
	n, p, q := len(a), len(b), len(b[0])
	out := make([][]float64, n)
	for i := 0; i < n; i++ {
		out[i] = make([]float64, q)
		for k := 0; k < p; k++ {
			for j := 0; j < q; j++ {
				out[i][j] += a[i][k] * b[k][j]
			}
		}
	}
	return out
}

// cholesky is L with L·Lᵀ = A, for a symmetric positive definite A.
func cholesky(A [][]float64) ([][]float64, error) {
	n := len(A)
	L := square(n)
	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			s := A[i][j]
			for k := 0; k < j; k++ {
				s -= L[i][k] * L[j][k]
			}
			if i == j {
				if !(s > 0) {
					return nil, fmt.Errorf("the information matrix is not positive definite at column %d", i)
				}
				L[i][i] = math.Sqrt(s)
			} else {
				L[i][j] = s / L[j][j]
			}
		}
	}
	return L, nil
}

func solveSPD(A [][]float64, b []float64) ([]float64, error) {
	L, err := cholesky(A)
	if err != nil {
		return nil, err
	}
	n := len(b)
	z := make([]float64, n)
	for i := 0; i < n; i++ {
		s := b[i]
		for k := 0; k < i; k++ {
			s -= L[i][k] * z[k]
		}
		z[i] = s / L[i][i]
	}
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		s := z[i]
		for k := i + 1; k < n; k++ {
			s -= L[k][i] * x[k]
		}
		x[i] = s / L[i][i]
	}
	return x, nil
}

func invertSPD(A [][]float64) ([][]float64, error) {
	n := len(A)
	inv := square(n)
	for j := 0; j < n; j++ {
		e := make([]float64, n)
		e[j] = 1
		col, err := solveSPD(A, e)
		if err != nil {
			return nil, err
		}
		for i := 0; i < n; i++ {
			inv[i][j] = col[i]
		}
	}
	return inv, nil
}
