// Package parseopt implements git's parse-options behaviour: long options with
// a --no- form, unique-prefix abbreviation, a "--" terminator, and the usage
// text git prints for -h or a bad option.
//
// git's own conventions are the specification here, not any Go flag package's,
// because a drop-in replacement for a git command has to accept exactly what
// the original accepts and refuse exactly what it refuses.
package parseopt

import (
	"fmt"
	"io"
	"strings"
)

// Bool is one boolean option. git's fsck takes no other kind.
type Bool struct {
	Short byte
	Long  string
	Help  string
	// Value receives true or false. A --no- prefix, or an explicit
	// --option=false, sets false.
	Value *int
}

// Set is a command's whole option table.
type Set struct {
	Usage []string
	Opts  []*Bool
}

// ErrHelp is returned when the user asked for the usage text.
type ErrHelp struct{}

func (ErrHelp) Error() string { return "usage requested" }

// ErrUsage is returned for a bad option. git exits 129 for both.
type ErrUsage struct{ Msg string }

func (e ErrUsage) Error() string { return e.Msg }

// Parse consumes the option arguments and returns whatever follows them.
func (s *Set) Parse(args []string) ([]string, error) {
	var rest []string
	i := 0
	for ; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			i++
			return append(rest, args[i:]...), nil
		case arg == "-h" || arg == "--help":
			return nil, ErrHelp{}
		case strings.HasPrefix(arg, "--"):
			if err := s.long(arg[2:]); err != nil {
				return nil, err
			}
		case len(arg) > 1 && arg[0] == '-':
			if err := s.short(arg[1:]); err != nil {
				return nil, err
			}
		default:
			// git's parse_options stops at the first non-option
			// unless KEEP_UNKNOWN is set; fsck does not set it, so
			// everything from here on is an argument.
			return append(rest, args[i:]...), nil
		}
	}
	return rest, nil
}

func (s *Set) long(name string) error {
	value := 1
	arg := name
	if eq := strings.IndexByte(arg, '='); eq >= 0 {
		v := arg[eq+1:]
		arg = arg[:eq]
		switch v {
		case "true", "yes", "on", "1":
			value = 1
		case "false", "no", "off", "0":
			value = 0
		default:
			return ErrUsage{Msg: fmt.Sprintf("option `%s' takes no value", arg)}
		}
	}
	if stripped, ok := strings.CutPrefix(arg, "no-"); ok {
		if o := s.find(stripped); o != nil {
			*o.Value = 1 - value
			return nil
		}
	}
	o := s.find(arg)
	if o == nil {
		return ErrUsage{Msg: fmt.Sprintf("unknown option `%s'", name)}
	}
	*o.Value = value
	return nil
}

// find resolves an exact long name, or the one option it abbreviates.
func (s *Set) find(name string) *Bool {
	var abbrev *Bool
	ambiguous := false
	for _, o := range s.Opts {
		if o.Long == name {
			return o
		}
		if strings.HasPrefix(o.Long, name) {
			if abbrev != nil {
				ambiguous = true
			}
			abbrev = o
		}
	}
	if ambiguous {
		return nil
	}
	return abbrev
}

func (s *Set) short(chars string) error {
	for i := 0; i < len(chars); i++ {
		c := chars[i]
		var found *Bool
		for _, o := range s.Opts {
			if o.Short == c {
				found = o
				break
			}
		}
		if found == nil {
			return ErrUsage{Msg: fmt.Sprintf("unknown switch `%c'", c)}
		}
		*found.Value = 1
	}
	return nil
}

// Width settings git uses when it lays out the option list.
const (
	usageOptsWidth = 24
	usageGap       = 2
)

// PrintUsage writes the usage block exactly as git's usage_with_options() does.
func (s *Set) PrintUsage(w io.Writer) {
	for i, u := range s.Usage {
		prefix := "usage: "
		if i > 0 {
			prefix = "   or: "
		}
		fmt.Fprintf(w, "%s%s\n", prefix, u)
	}
	fmt.Fprintln(w)
	for _, o := range s.Opts {
		pos := 4
		line := "    "
		if o.Short != 0 {
			line += "-" + string(o.Short)
			pos += 2
			if o.Long != "" {
				line += ", "
				pos += 2
			}
		}
		if o.Long != "" {
			line += "--[no-]" + o.Long
			pos += 7 + len(o.Long)
		}
		if pos <= usageOptsWidth {
			line += strings.Repeat(" ", usageOptsWidth-pos+usageGap)
		} else {
			line += "\n" + strings.Repeat(" ", usageOptsWidth+usageGap)
		}
		fmt.Fprintf(w, "%s%s\n", line, o.Help)
	}
	fmt.Fprintln(w)
}
