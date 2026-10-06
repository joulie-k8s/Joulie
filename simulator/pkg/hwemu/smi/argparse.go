package smi

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// rocm-smi and amd-smi are Python programs. This file holds what they share
// through Python itself: argparse parsing and errors, and the text Python
// prints for an int, a float and json.dumps.

// pyOneOrMore is argparse's nargs='+'.
const pyOneOrMore = -1

// pyOpt is one option of an argparse parser a fake tool mirrors.
type pyOpt struct {
	flags   []string // option strings in registration order, such as "-d", "--device"
	nargs   int      // 0 for a flag, n for exactly n values, pyOneOrMore for nargs='+'
	metavar []string // one per value; the last repeats for pyOneOrMore
	isInt   bool     // type=int: each value must be a Python int
	other   bool     // a real option fakesmi does not model
}

// key names the option in pyArgs: its last option string, the long form.
func (o *pyOpt) key() string { return o.flags[len(o.flags)-1] }

// pyOpts builds options from a compact spec, "-d/--device --json ...", in
// registration order. Options listed in modelled take their settings from
// it; the rest are real options fakesmi does not model.
func pyOpts(spec string, modelled map[string]pyOpt) []pyOpt {
	var opts []pyOpt
	for _, group := range strings.Fields(spec) {
		flags := strings.Split(group, "/")
		o, ok := modelled[flags[len(flags)-1]]
		if !ok {
			o.other = true
		}
		o.flags = flags
		opts = append(opts, o)
	}
	return opts
}

// pyParser is one argparse.ArgumentParser.
type pyParser struct {
	prog string
	opts []pyOpt
}

// pyArgs is what parsing produced.
type pyArgs struct {
	order      []string            // option keys in the order they first appeared
	values     map[string][]string // the last occurrence's values, as action 'store' keeps
	extras     []string            // arguments no option took: argparse's "unrecognized arguments"
	help       bool                // -h or --help, which argparse acts on at once
	unmodelled string              // the first real option fakesmi does not model
}

func (a *pyArgs) has(key string) bool { _, ok := a.values[key]; return ok }

func (a *pyArgs) value(key string) (string, bool) {
	v, ok := a.values[key]
	if !ok || len(v) == 0 {
		return "", false
	}
	return v[0], true
}

// pyUsageError is an argparse error: printed after the usage line, exit 2.
type pyUsageError struct{ msg string }

func (e *pyUsageError) Error() string { return e.msg }

// pyNegativeNumber is argparse's _negative_number_matcher. A parser without
// options that look like negative numbers takes such arguments as values.
var pyNegativeNumber = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

// pyOptionLike says whether argparse classifies arg as an option string ('O')
// rather than as a value ('A').
func pyOptionLike(arg string) bool {
	return len(arg) > 1 && arg[0] == '-' && !pyNegativeNumber.MatchString(arg)
}

// lookup resolves arg as argparse's _parse_optional does: an exact option
// string, then "--opt=value", then a unique prefix of a long option. A prefix
// shared by several options is an error that lists them. A single-dash
// argument longer than two characters is its first two characters plus an
// attached value (-d0) or more flags (-PM). opt is nil for an unknown option.
func (p *pyParser) lookup(arg string) (opt *pyOpt, flag, explicit string, hasExplicit bool, err error) {
	if o := p.find(arg); o != nil {
		return o, arg, "", false, nil
	}
	if strings.HasPrefix(arg, "--") {
		name, val, eq := strings.Cut(arg, "=")
		if o := p.find(name); o != nil && eq {
			return o, name, val, true, nil
		}
		var matches []string
		var match *pyOpt
		for i := range p.opts {
			for _, f := range p.opts[i].flags {
				if strings.HasPrefix(f, "--") && strings.HasPrefix(f, name) {
					matches = append(matches, f)
					match = &p.opts[i]
				}
			}
		}
		switch len(matches) {
		case 0:
			return nil, "", "", false, nil
		case 1:
			return match, matches[0], val, eq, nil
		}
		return nil, "", "", false, &pyUsageError{fmt.Sprintf("ambiguous option: %s could match %s", arg, strings.Join(matches, ", "))}
	}
	if len(arg) > 2 {
		if o := p.find(arg[:2]); o != nil {
			return o, arg[:2], arg[2:], true, nil
		}
	}
	return nil, "", "", false, nil
}

func (p *pyParser) find(flag string) *pyOpt {
	for i := range p.opts {
		for _, f := range p.opts[i].flags {
			if f == flag {
				return &p.opts[i]
			}
		}
	}
	return nil
}

// parse parses args as parse_known_args does. Errors about an option's
// values come back at once, as argparse raises them while it parses;
// unrecognized arguments come back in pyArgs.extras for the caller, because
// rocm-smi and amd-smi report them differently. Parsing stops at -h and at the
// first option fakesmi does not model.
func (p *pyParser) parse(args []string) (*pyArgs, error) {
	a := &pyArgs{values: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			a.extras = append(a.extras, args[i+1:]...)
			break
		}
		if !pyOptionLike(arg) {
			a.extras = append(a.extras, arg)
			continue
		}
		opt, flag, explicit, hasExplicit, err := p.lookup(arg)
		if err != nil {
			return nil, err
		}
		for opt != nil {
			if opt.other {
				a.unmodelled = flag
				return a, nil
			}
			if flag == "-h" || flag == "--help" {
				a.help = true
				return a, nil
			}
			name := strings.Join(opt.flags, "/")
			var vals []string
			next := (*pyOpt)(nil)
			switch {
			case opt.nargs == 0 && hasExplicit && !strings.HasPrefix(flag, "--"):
				// -PM: a flag followed by more single-character flags.
				short := flag[:1] + explicit[:1]
				if next = p.find(short); next == nil {
					return nil, &pyUsageError{fmt.Sprintf("argument %s: ignored explicit argument '%s'", name, explicit)}
				}
				flag, explicit = short, explicit[1:]
				hasExplicit = explicit != ""
			case opt.nargs == 0 && hasExplicit:
				return nil, &pyUsageError{fmt.Sprintf("argument %s: ignored explicit argument '%s'", name, explicit)}
			case opt.nargs != 0:
				if hasExplicit {
					vals = append(vals, explicit)
				}
				for (opt.nargs == pyOneOrMore || len(vals) < opt.nargs) && i+1 < len(args) && !pyOptionLike(args[i+1]) && args[i+1] != "--" {
					i++
					vals = append(vals, args[i])
				}
				switch {
				case opt.nargs == pyOneOrMore && len(vals) == 0:
					return nil, &pyUsageError{fmt.Sprintf("argument %s: expected at least one argument", name)}
				case opt.nargs == 1 && len(vals) == 0:
					return nil, &pyUsageError{fmt.Sprintf("argument %s: expected one argument", name)}
				case opt.nargs > 1 && len(vals) != opt.nargs:
					return nil, &pyUsageError{fmt.Sprintf("argument %s: expected %d arguments", name, opt.nargs)}
				}
				if opt.isInt {
					for _, v := range vals {
						if _, ok := pyInt(v); !ok {
							return nil, &pyUsageError{fmt.Sprintf("argument %s: invalid int value: '%s'", name, v)}
						}
					}
				}
			}
			if !a.has(opt.key()) {
				a.order = append(a.order, opt.key())
			}
			if vals == nil {
				vals = []string{}
			}
			a.values[opt.key()] = vals
			opt = next
		}
		if opt == nil && flag == "" {
			a.extras = append(a.extras, arg)
		}
	}
	return a, nil
}

// usage is the usage line: "usage: <prog> [-h] [-d DEVICE [DEVICE ...]] ...".
// It lists the modelled options only, so it is shorter than the real tool's.
func (p *pyParser) usage() string {
	var b strings.Builder
	b.WriteString("usage: " + p.prog)
	for _, o := range p.opts {
		if o.other {
			continue
		}
		b.WriteString(" [" + o.flags[0])
		switch {
		case o.nargs == pyOneOrMore:
			m := o.metavar[0]
			b.WriteString(" " + m + " [" + m + " ...]")
		case o.nargs > 0:
			b.WriteString(" " + strings.Join(o.metavar, " "))
		}
		b.WriteString("]")
	}
	return b.String()
}

// fail prints an argparse error as ArgumentParser.error does and returns its
// exit code.
func (p *pyParser) fail(w io.Writer, msg string) int {
	fmt.Fprintf(w, "%s\n%s: error: %s\n", p.usage(), p.prog, msg)
	return 2
}

// help prints the usage line and the modelled options, then exits 0 as -h does.
func (p *pyParser) help(w io.Writer) int {
	fmt.Fprintf(w, "%s\n\nOptions emulated by fakesmi:\n", p.usage())
	for _, o := range p.opts {
		if !o.other {
			fmt.Fprintf(w, "  %s\n", strings.Join(o.flags, ", "))
		}
	}
	return 0
}

// pyInt parses s as Python's int(str) does: surrounding whitespace, an
// optional sign, and decimal digits with single underscores between them.
func pyInt(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	sign := int64(1)
	switch {
	case strings.HasPrefix(s, "-"):
		sign, s = -1, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64)
	if err != nil {
		return 0, false
	}
	return sign * v, true
}

// pyIsDigit is Python's str.isdigit for ASCII input: non-empty, digits only.
func pyIsDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// pyFloat is repr(float): the shortest digits that round-trip, a ".0" on
// whole numbers, and an exponent below 1e-4 and from 1e16 on. So 750000000/1e6
// prints "750.0", which the agent parses (extractFloatAny in cmd/agent).
func pyFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if f != 0 && (exp < -4 || exp >= 16) {
		return e
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// pyHex is hex() of a C short, as rocm-smi formats device IDs: 0x74a1, and a
// negative value for an ID with the top bit set.
func pyHex(v int64) string {
	s := int64(int16(v))
	if s < 0 {
		return "-0x" + strconv.FormatInt(-s, 16)
	}
	return "0x" + strconv.FormatInt(s, 16)
}

// pyDict is a Python dict: keys keep their insertion order, and setting an
// existing key keeps its place.
type pyDict struct {
	keys []string
	vals map[string]any
}

// newPyDict builds a dict from alternating keys and values.
func newPyDict(kv ...any) *pyDict {
	d := &pyDict{vals: map[string]any{}}
	for i := 0; i+1 < len(kv); i += 2 {
		d.set(kv[i].(string), kv[i+1])
	}
	return d
}

func (d *pyDict) set(k string, v any) {
	if _, ok := d.vals[k]; !ok {
		d.keys = append(d.keys, k)
	}
	d.vals[k] = v
}

// pyJSON is json.dumps(v) with indent 0 (separators ", " and ": ", one line)
// or json.dumps(v, indent=n), with ensure_ascii as Python defaults.
func pyJSON(v any, indent int) string {
	var b strings.Builder
	pyJSONValue(&b, v, indent, 0)
	return b.String()
}

func pyJSONValue(b *strings.Builder, v any, indent, depth int) {
	open := func(start string) {
		b.WriteString(start)
		if indent > 0 {
			b.WriteString("\n" + strings.Repeat(" ", indent*(depth+1)))
		}
	}
	sep := func() {
		if indent > 0 {
			b.WriteString(",\n" + strings.Repeat(" ", indent*(depth+1)))
		} else {
			b.WriteString(", ")
		}
	}
	end := func(s string) {
		if indent > 0 {
			b.WriteString("\n" + strings.Repeat(" ", indent*depth))
		}
		b.WriteString(s)
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		b.WriteString(pyJSONString(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		switch {
		case math.IsInf(x, 1):
			b.WriteString("Infinity")
		case math.IsInf(x, -1):
			b.WriteString("-Infinity")
		case math.IsNaN(x):
			b.WriteString("NaN")
		default:
			b.WriteString(pyFloat(x))
		}
	case *pyDict:
		if len(x.keys) == 0 {
			b.WriteString("{}")
			return
		}
		open("{")
		for i, k := range x.keys {
			if i > 0 {
				sep()
			}
			b.WriteString(pyJSONString(k) + ": ")
			pyJSONValue(b, x.vals[k], indent, depth+1)
		}
		end("}")
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		open("[")
		for i, e := range x {
			if i > 0 {
				sep()
			}
			pyJSONValue(b, e, indent, depth+1)
		}
		end("]")
	default:
		panic(fmt.Sprintf("pyJSON: unsupported type %T", v))
	}
}

// pyJSONString quotes s as json.dumps does with ensure_ascii: the short
// escapes, \u00XX for other control characters, and \uXXXX (surrogate pairs
// above the BMP) for everything outside printable ASCII.
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= 0x20 && r <= 0x7e:
				b.WriteRune(r)
			case r > 0xffff:
				r1, r2 := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
			default:
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
