// Package gitconfig parses git's configuration file syntax.
//
// The same parser reads .git/config and a .gitmodules blob, which is what fsck
// needs: git checks a .gitmodules blob with its ordinary configuration parser
// and reports a parse failure as its own finding.
package gitconfig

import (
	"errors"
	"fmt"
	"strings"
)

// ErrParse marks any syntax error in a configuration source.
var ErrParse = errors.New("bad config line")

// Entry is one setting. Value is nil for a bare key, which git reads as true.
type Entry struct {
	Key   string // "section.subsection.name", section and name lower-cased
	Value *string
	Line  int
}

// Parse reads a whole configuration source.
func Parse(data []byte) ([]Entry, error) {
	p := &parser{data: data, line: 1}
	return p.run()
}

// ForEach calls fn for every setting, stopping at the first error.
func ForEach(data []byte, fn func(key string, value *string) error) error {
	entries, err := Parse(data)
	for i := range entries {
		if e := fn(entries[i].Key, entries[i].Value); e != nil {
			return e
		}
	}
	return err
}

type parser struct {
	data    []byte
	pos     int
	line    int
	section string
	out     []Entry
}

func (p *parser) run() ([]Entry, error) {
	comment := false
	for {
		c, ok := p.next()
		if !ok {
			break
		}
		if c == '\n' {
			comment = false
			continue
		}
		if comment || c == '#' || c == ';' {
			comment = true
			continue
		}
		if c == ' ' || c == '\t' {
			continue
		}
		if c == '[' {
			name, err := p.section_()
			if err != nil {
				return p.out, err
			}
			p.section = name
			continue
		}
		if !isAlpha(c) {
			return p.out, fmt.Errorf("%w: line %d: key does not start with a letter", ErrParse, p.line)
		}
		if p.section == "" {
			return p.out, fmt.Errorf("%w: line %d: key outside a section", ErrParse, p.line)
		}
		name, value, err := p.keyValue(c)
		if err != nil {
			return p.out, err
		}
		p.out = append(p.out, Entry{Key: p.section + "." + name, Value: value, Line: p.line})
	}
	return p.out, nil
}

func (p *parser) next() (byte, bool) {
	if p.pos >= len(p.data) {
		return 0, false
	}
	c := p.data[p.pos]
	p.pos++
	if c == '\r' && p.pos < len(p.data) && p.data[p.pos] == '\n' {
		c = p.data[p.pos]
		p.pos++
	}
	if c == '\n' {
		p.line++
	}
	return c, true
}

func (p *parser) peek() (byte, bool) {
	if p.pos >= len(p.data) {
		return 0, false
	}
	return p.data[p.pos], true
}

// section_ parses "[name]", "[name \"subsection\"]", or the dotted spelling.
func (p *parser) section_() (string, error) {
	var name strings.Builder
	for {
		c, ok := p.next()
		if !ok {
			return "", fmt.Errorf("%w: line %d: unterminated section header", ErrParse, p.line)
		}
		switch {
		case c == ']':
			if name.Len() == 0 {
				return "", fmt.Errorf("%w: line %d: empty section name", ErrParse, p.line)
			}
			return name.String(), nil
		case c == ' ' || c == '\t':
			sub, err := p.subsection()
			if err != nil {
				return "", err
			}
			if name.Len() == 0 {
				return "", fmt.Errorf("%w: line %d: empty section name", ErrParse, p.line)
			}
			return name.String() + "." + sub, nil
		case isKeyChar(c) || c == '.':
			name.WriteByte(toLower(c))
		default:
			return "", fmt.Errorf("%w: line %d: bad section header", ErrParse, p.line)
		}
	}
}

// subsection parses the quoted part of a section header. Its case is kept.
func (p *parser) subsection() (string, error) {
	for {
		c, ok := p.next()
		if !ok {
			return "", fmt.Errorf("%w: line %d: unterminated section header", ErrParse, p.line)
		}
		if c == '"' {
			break
		}
		if c == ' ' || c == '\t' {
			continue
		}
		return "", fmt.Errorf("%w: line %d: bad section header", ErrParse, p.line)
	}
	var sub strings.Builder
	for {
		c, ok := p.next()
		if !ok || c == '\n' {
			return "", fmt.Errorf("%w: line %d: unterminated subsection name", ErrParse, p.line)
		}
		if c == '"' {
			break
		}
		if c == '\\' {
			c, ok = p.next()
			if !ok || c == '\n' {
				return "", fmt.Errorf("%w: line %d: unterminated subsection name", ErrParse, p.line)
			}
		}
		sub.WriteByte(c)
	}
	for {
		c, ok := p.next()
		if !ok {
			return "", fmt.Errorf("%w: line %d: unterminated section header", ErrParse, p.line)
		}
		if c == ']' {
			return sub.String(), nil
		}
		if c != ' ' && c != '\t' {
			return "", fmt.Errorf("%w: line %d: bad section header", ErrParse, p.line)
		}
	}
}

// keyValue parses "name" or "name = value" starting from its first letter.
func (p *parser) keyValue(first byte) (string, *string, error) {
	var name strings.Builder
	name.WriteByte(toLower(first))
	for {
		c, ok := p.peek()
		if !ok || !isKeyChar(c) {
			break
		}
		p.pos++
		name.WriteByte(toLower(c))
	}
	// Skip blanks between the key and '='.
	for {
		c, ok := p.peek()
		if !ok || (c != ' ' && c != '\t') {
			break
		}
		p.pos++
	}
	c, ok := p.peek()
	if !ok || c == '\n' || c == '#' || c == ';' {
		return name.String(), nil, nil
	}
	if c != '=' {
		return "", nil, fmt.Errorf("%w: line %d: expected '=' after key", ErrParse, p.line)
	}
	p.pos++
	value, err := p.value()
	if err != nil {
		return "", nil, err
	}
	return name.String(), &value, nil
}

// value parses everything to the end of a setting, honouring quotes, escapes,
// comments, and a backslash-newline continuation.
func (p *parser) value() (string, error) {
	var b strings.Builder
	quoted := false
	spaces := 0
	for {
		c, ok := p.next()
		if !ok {
			if quoted {
				return "", fmt.Errorf("%w: line %d: unterminated quoted value", ErrParse, p.line)
			}
			return b.String(), nil
		}
		if c == '\n' {
			if quoted {
				return "", fmt.Errorf("%w: line %d: unterminated quoted value", ErrParse, p.line)
			}
			p.pos--
			p.line--
			return b.String(), nil
		}
		if !quoted && (c == '#' || c == ';') {
			p.skipToEOL()
			return b.String(), nil
		}
		if c == ' ' || c == '\t' {
			if quoted {
				b.WriteByte(c)
			} else if b.Len() > 0 {
				spaces++
			}
			continue
		}
		if c == '"' {
			quoted = !quoted
			continue
		}
		if c == '\\' {
			e, ok := p.next()
			if !ok {
				return "", fmt.Errorf("%w: line %d: unfinished escape", ErrParse, p.line)
			}
			switch e {
			case '\n':
				continue // line continuation
			case 't':
				e = '\t'
			case 'b':
				e = '\b'
			case 'n':
				e = '\n'
			case '"', '\\':
			default:
				return "", fmt.Errorf("%w: line %d: bad escape \\%c", ErrParse, p.line, e)
			}
			for ; spaces > 0; spaces-- {
				b.WriteByte(' ')
			}
			b.WriteByte(e)
			continue
		}
		for ; spaces > 0; spaces-- {
			b.WriteByte(' ')
		}
		b.WriteByte(c)
	}
}

func (p *parser) skipToEOL() {
	for {
		c, ok := p.peek()
		if !ok || c == '\n' {
			return
		}
		p.pos++
	}
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

func toLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// SplitKey separates "section.subsection.name" into its parts. A key with no
// subsection reports ok for the subsection being absent.
func SplitKey(key string) (section, subsection, name string, hasSub bool) {
	first := strings.IndexByte(key, '.')
	if first < 0 {
		return key, "", "", false
	}
	last := strings.LastIndexByte(key, '.')
	if last == first {
		return key[:first], "", key[first+1:], false
	}
	return key[:first], key[first+1 : last], key[last+1:], true
}
