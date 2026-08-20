package fsck

import (
	"bytes"
	"errors"
	"net/url"
	"strings"

	"github.com/wow-look-at-my/git-fixed/internal/gitconfig"
	"github.com/wow-look-at-my/git-fixed/internal/gitobj"
)

// Limits git puts on a .gitattributes file it is willing to parse.
const (
	attrMaxLineLength = 2048
	attrMaxFileSize   = 100 * 1024 * 1024
)

// Blob checks a blob. Only a blob a tree named as .gitmodules or
// .gitattributes has anything to check. A nil buf means the blob was too large
// to read into memory.
func (o *Options) Blob(ctx any, oid gitobj.OID, buf []byte) int {
	if o.Skipped(oid) {
		return 0
	}
	ret := 0
	if o.takeGitmodules(oid) {
		if buf == nil {
			return o.report(ctx, oid, gitobj.TypeBlob, MsgGitmodulesLarge, ".gitmodules too large to parse")
		}
		ret |= o.checkGitmodules(ctx, oid, buf)
	}
	if o.takeGitattributes(oid) {
		if buf == nil || len(buf) > attrMaxFileSize {
			return o.report(ctx, oid, gitobj.TypeBlob, MsgGitattributesLarge, ".gitattributes too large to parse")
		}
		ret |= o.checkGitattributes(ctx, oid, buf)
	}
	return ret
}

// checkGitattributes rejects a line no attribute parser would accept. git stops
// at the first NUL, so this does too.
func (o *Options) checkGitattributes(ctx any, oid gitobj.OID, buf []byte) int {
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	for len(buf) > 0 {
		eol := bytes.IndexByte(buf, '\n')
		length := len(buf)
		if eol >= 0 {
			length = eol
		}
		if length >= attrMaxLineLength {
			return o.report(ctx, oid, gitobj.TypeBlob, MsgGitattributesLineLength,
				".gitattributes has too long lines to parse")
		}
		if eol < 0 {
			break
		}
		buf = buf[eol+1:]
	}
	return 0
}

// checkGitmodules reads a .gitmodules blob and refuses the settings that have
// been used to attack a checkout.
func (o *Options) checkGitmodules(ctx any, oid gitobj.OID, buf []byte) int {
	ret := 0
	entries, err := gitconfig.Parse(buf)
	for _, e := range entries {
		section, name, key, hasSub := gitconfig.SplitKey(e.Key)
		if section != "submodule" || !hasSub {
			continue
		}
		if checkSubmoduleName(name) != nil {
			ret |= o.report(ctx, oid, gitobj.TypeBlob, MsgGitmodulesName,
				"disallowed submodule name: %s", name)
		}
		if e.Value == nil {
			continue
		}
		value := *e.Value
		switch key {
		case "url":
			if checkSubmoduleURL(value) != nil {
				ret |= o.report(ctx, oid, gitobj.TypeBlob, MsgGitmodulesUrl,
					"disallowed submodule url: %s", value)
			}
		case "path":
			if looksLikeCommandLineOption(value) {
				ret |= o.report(ctx, oid, gitobj.TypeBlob, MsgGitmodulesPath,
					"disallowed submodule path: %s", value)
			}
		case "update":
			if strings.HasPrefix(value, "!") {
				ret |= o.report(ctx, oid, gitobj.TypeBlob, MsgGitmodulesUpdate,
					"disallowed submodule update setting: %s", value)
			}
		}
	}
	if err != nil {
		ret |= o.report(ctx, oid, gitobj.TypeBlob, MsgGitmodulesParse, "could not parse gitmodules blob")
	}
	return ret
}

var errBadSubmodule = errors.New("disallowed submodule setting")

// checkSubmoduleName rejects an empty name and any ".." path component. Both
// separators count, so the rule reads the same on every platform.
func checkSubmoduleName(name string) error {
	if name == "" {
		return errBadSubmodule
	}
	for i := 0; ; {
		if strings.HasPrefix(name[i:], "..") {
			rest := name[i+2:]
			if rest == "" || isXPlatformSep(rest[0]) {
				return errBadSubmodule
			}
		}
		next := strings.IndexAny(name[i:], "/\\")
		if next < 0 {
			return nil
		}
		i += next + 1
		if i > len(name) {
			return nil
		}
	}
}

func isXPlatformSep(c byte) bool { return c == '/' || c == '\\' }

func looksLikeCommandLineOption(s string) bool { return strings.HasPrefix(s, "-") }

func startsWithDotSlash(s string) bool {
	return len(s) >= 2 && s[0] == '.' && isXPlatformSep(s[1])
}

func startsWithDotDotSlash(s string) bool {
	return len(s) >= 3 && s[0] == '.' && s[1] == '.' && isXPlatformSep(s[2])
}

// countLeadingDotdots counts the "../" components a relative submodule URL
// chops off the URL it resolves against.
func countLeadingDotdots(u string) (int, string) {
	n := 0
	for {
		switch {
		case startsWithDotDotSlash(u):
			n++
			u = u[3:]
		case startsWithDotSlash(u):
			u = u[2:]
		default:
			return n, u
		}
	}
}

// urlToCurlURL reports whether git-remote-curl would handle this URL, and gives
// back the URL it would be handed.
func urlToCurlURL(u string) (string, bool) {
	for _, p := range []string{"http::", "https::", "ftp::", "ftps::"} {
		if strings.HasPrefix(u, p) {
			return u[len(p):], true
		}
	}
	for _, p := range []string{"http://", "https://", "ftp://", "ftps://"} {
		if strings.HasPrefix(u, p) {
			return u, true
		}
	}
	return "", false
}

// checkSubmoduleURL refuses the submodule URLs that were exploitable, which is
// git's check_submodule_url().
func checkSubmoduleURL(u string) error {
	if looksLikeCommandLineOption(u) {
		return errBadSubmodule
	}
	if startsWithDotSlash(u) || startsWithDotDotSlash(u) || strings.HasPrefix(u, "git://") {
		// This can be appended to an http URL and then url-decoded, so
		// it must not smuggle a newline through.
		decoded, err := url.QueryUnescape(u)
		if err != nil {
			decoded = u
		}
		if strings.ContainsRune(decoded, '\n') {
			return errBadSubmodule
		}
		// A URL that escapes its own root with "../" can overwrite the
		// host, which is CVE-2020-11008.
		if n, next := countLeadingDotdots(u); n > 0 && next != "" && (next[0] == ':' || next[0] == '/') {
			return errBadSubmodule
		}
		return nil
	}
	if curlURL, ok := urlToCurlURL(u); ok {
		host, err := credentialHostFromURL(curlURL)
		if err != nil || host == "" {
			return errBadSubmodule
		}
	}
	return nil
}

// credentialHostFromURL is git's credential_from_url_gently(): it splits the
// URL the same way, and refuses any component holding a newline.
func credentialHostFromURL(u string) (string, error) {
	protoEnd := strings.Index(u, "://")
	if protoEnd <= 0 {
		return "", errBadSubmodule
	}
	proto := u[:protoEnd]
	cp := u[protoEnd+3:]
	at := strings.IndexByte(cp, '@')
	colon := strings.IndexByte(cp, ':')
	slash := strings.IndexAny(cp, "/?#")
	if slash < 0 {
		slash = len(cp)
	}
	var username, password, host string
	switch {
	case at < 0 || slash <= at:
		host = cp[:slash]
	case colon < 0 || at <= colon:
		username = urlDecode(cp[:at])
		host = cp[at+1 : slash]
	default:
		username = urlDecode(cp[:colon])
		password = urlDecode(cp[colon+1 : at])
		host = cp[at+1 : slash]
	}
	host = urlDecode(host)
	path := strings.TrimLeft(cp[slash:], "/")
	path = urlDecode(strings.TrimRight(path, "/"))
	for _, part := range []string{username, password, proto, host, path} {
		if strings.ContainsRune(part, '\n') {
			return "", errBadSubmodule
		}
	}
	return host, nil
}

// urlDecode undoes percent-encoding, leaving an invalid sequence in place the
// way git's url_decode() does.
func urlDecode(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func (o *Options) takeGitmodules(oid gitobj.OID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.gitmodulesFound.Contains(oid) {
		return false
	}
	o.gitmodulesDone.Add(oid)
	return true
}

func (o *Options) takeGitattributes(oid gitobj.OID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.gitattributesFound.Contains(oid) {
		return false
	}
	o.gitattributesDone.Add(oid)
	return true
}

// PendingBlobs lists the .gitmodules and .gitattributes blobs a tree named but
// whose content the run has not checked yet.
func (o *Options) PendingBlobs() (gitmodules, gitattributes []gitobj.OID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for oid := range o.gitmodulesFound.All() {
		if !o.gitmodulesDone.Contains(oid) {
			gitmodules = append(gitmodules, oid)
		}
	}
	for oid := range o.gitattributesFound.All() {
		if !o.gitattributesDone.Contains(oid) {
			gitattributes = append(gitattributes, oid)
		}
	}
	return gitmodules, gitattributes
}

// ReportMissingBlob reports a named .gitmodules or .gitattributes blob that
// could not be read.
func (o *Options) ReportMissingBlob(ctx any, oid gitobj.OID, kind string) int {
	id := MsgGitmodulesMissing
	if kind == ".gitattributes" {
		id = MsgGitattributesMissing
	}
	return o.report(ctx, oid, gitobj.TypeBlob, id, "unable to read %s blob", kind)
}

// ReportNonBlob reports that a name that must be a blob is some other type.
func (o *Options) ReportNonBlob(ctx any, oid gitobj.OID, typ gitobj.Type, kind string) int {
	id := MsgGitmodulesBlob
	if kind == ".gitattributes" {
		id = MsgGitattributesBlob
	}
	return o.report(ctx, oid, typ, id, "non-blob found at %s", kind)
}
