//go:build !faultinject

package runner

// FaultInjectionBuilt says whether this binary can inject database faults into the third engine.
// This is the normal build: it cannot, and the code that could is not compiled in.
const FaultInjectionBuilt = false

// WrapStore3 is the fault-injection seam of a normal build: it hands the store back untouched,
// whatever was asked for. main logs a warning when faults were asked of a binary that has none.
func WrapStore3(db Store3, _ Faults) Store3 { return db }
