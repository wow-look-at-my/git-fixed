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

// Bool is one boolean option.
type Bool struct {
	Short byte
	Long  string
	Help  string
	// Value receives true or false. A --no- prefix, or an explicit
	// --option=false, sets false.
	Value *int
}

// Str is one option that takes a value. git accepts four spellings for one,
// and so does this: "-C dir", "-Cdir", "--long value" and "--long=value".
//
// A Str has no --no- form, because there is no value to unset it to.
type Str struct {
	Short byte
	Long  string
	// Arg names the value in the usage text, the way "<directory>" does.
	Arg   string
	Help  string
	Value *string
}

// Set is a command's whole option table.
type Set struct {
	Usage []string
	Opts  []*Bool
	// Strs are the options that take a value. They print after the boolean
	// ones, which is why a command lists its most general option here.
	Strs []*Str
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
			used, err := s.long(arg[2:], args[i+1:])
			if err != nil {
				return nil, err
			}
			i += used
		case len(arg) > 1 && arg[0] == '-':
			used, err := s.short(arg[1:], args[i+1:])
			if err != nil {
				return nil, err
			}
			i += used
		default:
			// git's parse_options stops at the first non-option
			// unless KEEP_UNKNOWN is set; fsck does not set it, so
			// everything from here on is an argument.
			return append(rest, args[i:]...), nil
		}
	}
	return rest, nil
}

// long handles one "--" argument and reports how many of the arguments after it
// were consumed as its value.
func (s *Set) long(name string, next []string) (int, error) {
	arg := name
	if eq := strings.IndexByte(arg, '='); eq >= 0 {
		arg = arg[:eq]
	}
	if _, str := s.resolve(arg); str != nil {
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			*str.Value = name[eq+1:]
			return 0, nil
		}
		if len(next) == 0 {
			return 0, ErrUsage{Msg: fmt.Sprintf("option `%s' requires a value", arg)}
		}
		*str.Value = next[0]
		return 1, nil
	}
	return 0, s.longBool(name)
}

func (s *Set) longBool(name string) error {
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
	o, _ := s.resolve(name)
	return o
}

// resolve finds the one option a long name spells, exactly or as the only
// abbreviation of it. Both tables are searched together, so a prefix that fits
// an option of each kind is ambiguous and resolves to neither.
func (s *Set) resolve(name string) (*Bool, *Str) {
	var (
		abbrevBool *Bool
		abbrevStr  *Str
		abbrevs    int
	)
	for _, o := range s.Opts {
		if o.Long == name {
			return o, nil
		}
		if strings.HasPrefix(o.Long, name) {
			abbrevBool = o
			abbrevs++
		}
	}
	for _, o := range s.Strs {
		if o.Long == name {
			return nil, o
		}
		if o.Long != "" && strings.HasPrefix(o.Long, name) {
			abbrevStr = o
			abbrevs++
		}
	}
	if abbrevs != 1 {
		return nil, nil
	}
	return abbrevBool, abbrevStr
}

// short handles one "-" argument and reports how many of the arguments after it
// were consumed as a value.
func (s *Set) short(chars string, next []string) (int, error) {
	for i := 0; i < len(chars); i++ {
		c := chars[i]
		if str := s.shortStr(c); str != nil {
			// Whatever follows the letter is the value, as in "-Cdir".
			// A letter at the end of the bundle takes the next
			// argument instead, as in "-C dir".
			if rest := chars[i+1:]; rest != "" {
				*str.Value = rest
				return 0, nil
			}
			if len(next) == 0 {
				return 0, ErrUsage{Msg: fmt.Sprintf("switch `%c' requires a value", c)}
			}
			*str.Value = next[0]
			return 1, nil
		}
		var found *Bool
		for _, o := range s.Opts {
			if o.Short == c {
				found = o
				break
			}
		}
		if found == nil {
			return 0, ErrUsage{Msg: fmt.Sprintf("unknown switch `%c'", c)}
		}
		*found.Value = 1
	}
	return 0, nil
}

// shortStr finds the value option one letter names.
func (s *Set) shortStr(c byte) *Str {
	for _, o := range s.Strs {
		if o.Short == c {
			return o
		}
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
		names := ""
		if o.Long != "" {
			names = "--[no-]" + o.Long
		}
		writeOption(w, spell(o.Short, names), o.Help)
	}
	for _, o := range s.Strs {
		names := ""
		if o.Long != "" {
			names = "--" + o.Long
		}
		names = spell(o.Short, names)
		if o.Arg != "" {
			names += " " + o.Arg
		}
		writeOption(w, names, o.Help)
	}
	fmt.Fprintln(w)
}

// spell joins an option's two names the way git prints them.
func spell(short byte, long string) string {
	switch {
	case short == 0:
		return long
	case long == "":
		return "-" + string(short)
	}
	return "-" + string(short) + ", " + long
}

// writeOption lays one option out against the help column, wrapping onto the
// next line when the names are too wide for it.
func writeOption(w io.Writer, names, help string) {
	line := "    " + names
	if len(line) <= usageOptsWidth {
		line += strings.Repeat(" ", usageOptsWidth-len(line)+usageGap)
	} else {
		line += "\n" + strings.Repeat(" ", usageOptsWidth+usageGap)
	}
	fmt.Fprintf(w, "%s%s\n", line, help)
}
