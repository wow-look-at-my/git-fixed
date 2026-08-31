package fsck

import (
	"bytes"
	"math"
	"strconv"

	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// Object runs the checks for an object of a known type. It is git's
// fsck_buffer(). A nil buf means the object was too large to hold in memory.
func (o *Options) Object(ctx any, oid gitobj.OID, typ gitobj.Type, buf []byte) int {
	switch typ {
	case gitobj.TypeBlob:
		return o.Blob(ctx, oid, buf)
	case gitobj.TypeTree:
		return o.Tree(ctx, oid, buf)
	case gitobj.TypeCommit:
		return o.Commit(ctx, oid, buf)
	case gitobj.TypeTag:
		return o.Tag(ctx, oid, buf)
	}
	return o.report(ctx, oid, typ, MsgUnknownType, "unknown type '%d' (internal fsck error)", int(typ))
}

// verifyHeaders confirms the header block of a commit or tag ends in a way the
// line-by-line parsing below can rely on.
func (o *Options) verifyHeaders(ctx any, buf []byte, oid gitobj.OID, typ gitobj.Type) int {
	for i := 0; i < len(buf); i++ {
		switch buf[i] {
		case 0:
			return o.report(ctx, oid, typ, MsgNulInHeader, "unterminated header: NUL at offset %d", i)
		case '\n':
			if i+1 < len(buf) && buf[i+1] == '\n' {
				return 0
			}
		}
	}
	// No blank line separates header from body. A missing body is fine, but
	// the last header line still needs its newline.
	if len(buf) > 0 && buf[len(buf)-1] == '\n' {
		return 0
	}
	return o.report(ctx, oid, typ, MsgUnterminatedHeader, "unterminated header")
}

// ident checks a "Name <email> timestamp timezone" line and advances buf past
// it. It is git's fsck_ident().
func (o *Options) ident(ctx any, buf []byte, oid gitobj.OID, typ gitobj.Type) (rest []byte, ret int) {
	p := buf
	// Advance the caller past this line up front, exactly as git does, so a
	// reported problem still leaves the caller pointing at the next line.
	if nl := bytes.IndexByte(buf, '\n'); nl >= 0 {
		rest = buf[nl+1:]
	} else {
		rest = buf[len(buf):]
	}

	if len(p) > 0 && p[0] == '<' {
		return rest, o.report(ctx, oid, typ, MsgMissingNameBeforeEmail,
			"invalid author/committer line - missing space before email")
	}
	i := cspn(p, "<>\n")
	if i < len(p) && p[i] == '>' {
		return rest, o.report(ctx, oid, typ, MsgBadName, "invalid author/committer line - bad name")
	}
	if i >= len(p) || p[i] != '<' {
		return rest, o.report(ctx, oid, typ, MsgMissingEmail, "invalid author/committer line - missing email")
	}
	if i == 0 || p[i-1] != ' ' {
		return rest, o.report(ctx, oid, typ, MsgMissingSpaceBeforeEmail,
			"invalid author/committer line - missing space before email")
	}
	p = p[i+1:]
	i = cspn(p, "<>\n")
	if i >= len(p) || p[i] != '>' {
		return rest, o.report(ctx, oid, typ, MsgBadEmail, "invalid author/committer line - bad email")
	}
	p = p[i+1:]
	if len(p) == 0 || p[0] != ' ' {
		return rest, o.report(ctx, oid, typ, MsgMissingSpaceBeforeDate,
			"invalid author/committer line - missing space before date")
	}
	p = p[1:]
	// git scans the blanks itself rather than letting strtoul eat the
	// newline that bounds the buffer.
	for len(p) > 0 && (p[0] == ' ' || p[0] == '\t') {
		p = p[1:]
	}
	if len(p) == 0 || !isDigit(p[0]) {
		return rest, o.report(ctx, oid, typ, MsgBadDate, "invalid author/committer line - bad date")
	}
	if p[0] == '0' && (len(p) < 2 || p[1] != ' ') {
		return rest, o.report(ctx, oid, typ, MsgZeroPaddedDate,
			"invalid author/committer line - zero-padded date")
	}
	ts, ndigits := parseTimestamp(p)
	if dateOverflows(ts) {
		return rest, o.report(ctx, oid, typ, MsgBadDateOverflow,
			"invalid author/committer line - date causes integer overflow")
	}
	if ndigits == 0 || len(p) <= ndigits || p[ndigits] != ' ' {
		return rest, o.report(ctx, oid, typ, MsgBadDate, "invalid author/committer line - bad date")
	}
	p = p[ndigits+1:]
	if len(p) < 6 ||
		(p[0] != '+' && p[0] != '-') ||
		!isDigit(p[1]) || !isDigit(p[2]) || !isDigit(p[3]) || !isDigit(p[4]) ||
		p[5] != '\n' {
		return rest, o.report(ctx, oid, typ, MsgBadTimezone, "invalid author/committer line - bad time zone")
	}
	return rest, 0
}

// Commit runs every check git makes on a commit object.
func (o *Options) Commit(ctx any, oid gitobj.OID, buf []byte) int {
	// Parsing below relies on the header ending well, so a failure here
	// must stop the whole check.
	if o.verifyHeaders(ctx, buf, oid, gitobj.TypeCommit) != 0 {
		return -1
	}
	whole := buf
	hexsz := o.Algo.HexSize

	rest, ok := cut(buf, "tree ")
	if !ok {
		return o.report(ctx, oid, gitobj.TypeCommit, MsgMissingTree, "invalid format - expected 'tree' line")
	}
	buf = rest
	if !validHexLine(buf, hexsz) {
		if err := o.report(ctx, oid, gitobj.TypeCommit, MsgBadTreeSha1, "invalid 'tree' line format - bad sha1"); err != 0 {
			return err
		}
	}
	buf = afterLine(buf, hexsz)
	for {
		next, ok := cut(buf, "parent ")
		if !ok {
			break
		}
		buf = next
		if !validHexLine(buf, hexsz) {
			if err := o.report(ctx, oid, gitobj.TypeCommit, MsgBadParentSha1, "invalid 'parent' line format - bad sha1"); err != 0 {
				return err
			}
		}
		buf = afterLine(buf, hexsz)
	}
	authorCount := 0
	err := 0
	for {
		next, ok := cut(buf, "author ")
		if !ok {
			break
		}
		authorCount++
		buf, err = o.ident(ctx, next, oid, gitobj.TypeCommit)
		if err != 0 {
			return err
		}
	}
	switch {
	case authorCount < 1:
		err = o.report(ctx, oid, gitobj.TypeCommit, MsgMissingAuthor, "invalid format - expected 'author' line")
	case authorCount > 1:
		err = o.report(ctx, oid, gitobj.TypeCommit, MsgMultipleAuthors, "invalid format - multiple 'author' lines")
	}
	if err != 0 {
		return err
	}
	next, ok := cut(buf, "committer ")
	if !ok {
		return o.report(ctx, oid, gitobj.TypeCommit, MsgMissingCommitter, "invalid format - expected 'committer' line")
	}
	if _, err = o.ident(ctx, next, oid, gitobj.TypeCommit); err != 0 {
		return err
	}
	if bytes.IndexByte(whole, 0) >= 0 {
		if err = o.report(ctx, oid, gitobj.TypeCommit, MsgNulInCommit, "NUL byte in the commit object body"); err != 0 {
			return err
		}
	}
	return 0
}

// TagInfo is what a tag names, for a caller that needs it after the check.
type TagInfo struct {
	Object     gitobj.OID
	TargetType gitobj.Type
	Name       string
}

// Tag runs every check git makes on a tag object.
func (o *Options) Tag(ctx any, oid gitobj.OID, buf []byte) int {
	ret, _ := o.TagWithInfo(ctx, oid, buf)
	return ret
}

// TagWithInfo runs the tag checks and also reports what the tag names. It is
// git's fsck_tag_standalone().
func (o *Options) TagWithInfo(ctx any, oid gitobj.OID, buf []byte) (int, TagInfo) {
	var info TagInfo
	info.TargetType = gitobj.TypeBad
	if ret := o.verifyHeaders(ctx, buf, oid, gitobj.TypeTag); ret != 0 {
		return ret, info
	}
	hexsz := o.Algo.HexSize

	rest, ok := cut(buf, "object ")
	if !ok {
		return o.report(ctx, oid, gitobj.TypeTag, MsgMissingObject, "invalid format - expected 'object' line"), info
	}
	buf = rest
	if !validHexLine(buf, hexsz) {
		if ret := o.report(ctx, oid, gitobj.TypeTag, MsgBadObjectSha1, "invalid 'object' line format - bad sha1"); ret != 0 {
			return ret, info
		}
	} else {
		info.Object, _ = o.Algo.ParseHexBytes(buf)
	}
	buf = afterLine(buf, hexsz)

	rest, ok = cut(buf, "type ")
	if !ok {
		return o.report(ctx, oid, gitobj.TypeTag, MsgMissingTypeEntry, "invalid format - expected 'type' line"), info
	}
	buf = rest
	eol := bytes.IndexByte(buf, '\n')
	if eol < 0 {
		return o.report(ctx, oid, gitobj.TypeTag, MsgMissingType, "invalid format - unexpected end after 'type' line"), info
	}
	info.TargetType = gitobj.TypeFromName(string(buf[:eol]))
	if info.TargetType == gitobj.TypeBad {
		if ret := o.report(ctx, oid, gitobj.TypeTag, MsgBadType, "invalid 'type' value"); ret != 0 {
			return ret, info
		}
	}
	buf = buf[eol+1:]

	rest, ok = cut(buf, "tag ")
	if !ok {
		return o.report(ctx, oid, gitobj.TypeTag, MsgMissingTagEntry, "invalid format - expected 'tag' line"), info
	}
	buf = rest
	eol = bytes.IndexByte(buf, '\n')
	if eol < 0 {
		return o.report(ctx, oid, gitobj.TypeTag, MsgMissingTag, "invalid format - unexpected end after 'type' line"), info
	}
	info.Name = string(buf[:eol])
	if !CheckRefnameFormat("refs/tags/"+info.Name, 0) {
		if ret := o.report(ctx, oid, gitobj.TypeTag, MsgBadTagName, "invalid 'tag' name: %s", info.Name); ret != 0 {
			return ret, info
		}
	}
	buf = buf[eol+1:]

	ret := 0
	if rest, ok = cut(buf, "tagger "); !ok {
		// Tags older than the 'tagger' line exist, so this is only a
		// warning by default.
		if ret = o.report(ctx, oid, gitobj.TypeTag, MsgMissingTaggerEntry, "invalid format - expected 'tagger' line"); ret != 0 {
			return ret, info
		}
	} else {
		buf, ret = o.ident(ctx, rest, oid, gitobj.TypeTag)
	}

	if len(buf) > 0 && buf[0] != '\n' {
		// verifyHeaders lets a line after 'tagger' pass as a custom
		// header. mktag wants no unknown headers at all.
		if ret = o.report(ctx, oid, gitobj.TypeTag, MsgExtraHeaderEntry, "invalid format - extra header(s) after 'tagger'"); ret != 0 {
			return ret, info
		}
	}
	return ret, info
}

// cut removes a literal prefix, reporting whether it was there.
func cut(buf []byte, prefix string) ([]byte, bool) {
	if len(buf) < len(prefix) || string(buf[:len(prefix)]) != prefix {
		return buf, false
	}
	return buf[len(prefix):], true
}

// validHexLine reports whether buf opens with a full hex object name followed by
// a newline.
func validHexLine(buf []byte, hexsz int) bool {
	if len(buf) < hexsz+1 || buf[hexsz] != '\n' {
		return false
	}
	for _, c := range buf[:hexsz] {
		if !isHex(c) {
			return false
		}
	}
	return true
}

// afterLine advances past an object name line, or past the next newline when the name was malformed.
func afterLine(buf []byte, hexsz int) []byte {
	if len(buf) >= hexsz+1 && buf[hexsz] == '\n' {
		return buf[hexsz+1:]
	}
	n := 0
	for n < len(buf) && isHex(buf[n]) {
		n++
	}
	if n < len(buf) {
		n++
	}
	return buf[n:]
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// cspn is C's strcspn over a byte slice.
func cspn(b []byte, reject string) int {
	for i := 0; i < len(b); i++ {
		if bytes.IndexByte([]byte(reject), b[i]) >= 0 {
			return i
		}
	}
	return len(b)
}

// parseTimestamp reads leading decimal digits, saturating the way strtoumax
// does, and reports how many digits it consumed.
func parseTimestamp(p []byte) (uint64, int) {
	n := 0
	for n < len(p) && isDigit(p[n]) {
		n++
	}
	if n == 0 {
		return 0, 0
	}
	v, err := strconv.ParseUint(string(p[:n]), 10, 64)
	if err != nil {
		v = math.MaxUint64
	}
	return v, n
}

// dateOverflows is git's date_overflows(): the value must fit a signed time_t.
func dateOverflows(t uint64) bool { return t > math.MaxInt64 }
