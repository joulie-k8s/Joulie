package scenario

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	comparatorRE = regexp.MustCompile(ComparatorPattern)
	// comparatorNumberRE is the number of a comparator, the first match
	// after the operator.
	comparatorNumberRE = regexp.MustCompile(`-?[0-9]+(\.[0-9]+)?(e[+-]?[0-9]+)?`)
)

// Dimension is what a comparator's unit measures. A comparator without a
// unit is DimensionNone and compares the observed number as it is.
type Dimension string

const (
	DimensionNone   Dimension = ""
	DimensionTime   Dimension = "time"
	DimensionEnergy Dimension = "energy"
	DimensionPower  Dimension = "power"
)

// unitScale converts a comparator unit to seconds, joules or watts.
var unitScale = map[string]struct {
	scale float64
	dim   Dimension
}{
	"ms":  {1e-3, DimensionTime},
	"s":   {1, DimensionTime},
	"min": {60, DimensionTime},
	"h":   {3600, DimensionTime},
	"J":   {1, DimensionEnergy},
	"kJ":  {1e3, DimensionEnergy},
	"MJ":  {1e6, DimensionEnergy},
	"W":   {1, DimensionPower},
}

// Expectation is a parsed Comparator: Op applied to the observed value and
// Value, which is in seconds, joules or watts when the comparator has a unit.
type Expectation struct {
	Op    string
	Value float64
	Unit  string
	Dim   Dimension
}

// Parse returns the Expectation c states, or an error naming the grammar.
func (c Comparator) Parse() (Expectation, error) {
	text := strings.TrimSpace(string(c))
	m := comparatorRE.FindStringSubmatch(text)
	if m == nil {
		return Expectation{}, fmt.Errorf("comparator %q does not match %s", string(c), ComparatorPattern)
	}
	op, unit := m[1], m[len(m)-1]
	v, err := strconv.ParseFloat(comparatorNumberRE.FindString(text[len(op):]), 64)
	if err != nil {
		return Expectation{}, fmt.Errorf("comparator %q: %w", string(c), err)
	}
	e := Expectation{Op: op, Value: v, Unit: unit}
	if u, ok := unitScale[unit]; ok {
		e.Value *= u.scale
		e.Dim = u.dim
	}
	return e, nil
}

// Holds reports whether observed meets the expectation. == and != allow a
// relative difference of 1e-9, so a ratio computed two ways still compares
// equal.
func (e Expectation) Holds(observed float64) bool {
	if math.IsNaN(observed) {
		return false
	}
	eq := math.Abs(observed-e.Value) <= 1e-9*math.Max(1, math.Abs(e.Value))
	switch e.Op {
	case "<=":
		return observed <= e.Value || eq
	case ">=":
		return observed >= e.Value || eq
	case "<":
		return observed < e.Value && !eq
	case ">":
		return observed > e.Value && !eq
	case "==":
		return eq
	case "!=":
		return !eq
	}
	return false
}

// String is the expectation in base units, such as ">= 90 s".
func (e Expectation) String() string {
	s := e.Op + " " + strconv.FormatFloat(e.Value, 'g', -1, 64)
	switch e.Dim {
	case DimensionTime:
		s += " s"
	case DimensionEnergy:
		s += " J"
	case DimensionPower:
		s += " W"
	}
	return s
}
