package fsck

import (
	"strings"

	"github.com/wow-look-at-my/go-containers/set"
)

// Flags for CheckRefnameFormat, matching git's REFNAME_* values.
const (
	// RefnameAllowOnelevel permits a name with a single component.
	RefnameAllowOnelevel = 1 << iota
	// RefnameRefspecPattern permits a single "*" component.
	RefnameRefspecPattern
)

// refnameDisposition classifies every byte, exactly as git's table does.
//
//	Each entry selects a case in checkRefnameComponent below: end of
//	component, '.', '{', forbidden, or '*'.
var refnameDisposition = [256]byte{
	1, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
	4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
	4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 0, 0, 0, 2, 1,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4, 0, 0, 0, 0, 4,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4, 4, 0, 4, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 0, 0, 4, 4,
}

const lockSuffix = ".lock"

// CheckRefnameFormat reports whether a reference name is well formed. It is
// git's check_refname_format(), and fsck uses it to judge a tag's own name.
func CheckRefnameFormat(refname string, flags int) bool {
	if refname == "@" {
		return false
	}
	componentCount := 0
	componentLen := 0
	for {
		var ok bool
		componentLen, ok = checkRefnameComponent(refname, &flags)
		if !ok || componentLen == 0 {
			return false
		}
		componentCount++
		if componentLen >= len(refname) {
			break
		}
		refname = refname[componentLen+1:]
	}
	if componentLen > 0 && refname[componentLen-1] == '.' {
		return false
	}
	if flags&RefnameAllowOnelevel == 0 && componentCount < 2 {
		return false
	}
	return true
}

// checkRefnameComponent returns the length of the leading component, and
// reports whether that component is acceptable.
func checkRefnameComponent(refname string, flags *int) (int, bool) {
	var last byte
	i := 0
	for ; i < len(refname); i++ {
		ch := refname[i]
		switch refnameDisposition[ch] {
		case 1:
			goto out
		case 2:
			if last == '.' { // the component holds ".."
				return 0, false
			}
		case 3:
			if last == '@' { // the component holds "@{"
				return 0, false
			}
		case 4:
			return 0, false
		case 5:
			if *flags&RefnameRefspecPattern == 0 {
				return 0, false
			}
			// Only a single side of a refspec may hold the asterisk.
			*flags &^= RefnameRefspecPattern
		}
		last = ch
	}
out:
	if i == 0 {
		return 0, true // an empty component, which the caller rejects
	}
	if refname[0] == '.' {
		return 0, false
	}
	if i >= len(lockSuffix) && refname[i-len(lockSuffix):i] == lockSuffix {
		return 0, false
	}
	return i, true
}

// IsBranchRef reports whether a reference name lives under refs/heads/.
func IsBranchRef(refname string) bool { return strings.HasPrefix(refname, "refs/heads/") }

// irregularRootRefs are the root references whose names do not end in _HEAD.
var irregularRootRefs = set.Of(
	"HEAD", "AUTO_MERGE", "BISECT_EXPECTED_REV",
	"NOTES_MERGE_PARTIAL", "NOTES_MERGE_REF", "MERGE_AUTOSTASH",
)

// IsRootRef reports whether a name belongs to a reference that lives beside
// refs/ rather than under it. Such a name is a single component in capitals,
// so the ordinary refname rules do not apply to it.
func IsRootRef(refname string) bool {
	if !isRootRefSyntax(refname) || isPseudoRef(refname) {
		return false
	}
	if strings.HasSuffix(refname, "_HEAD") {
		return true
	}
	return irregularRootRefs.Contains(refname)
}

// isRootRefSyntax reports whether every byte belongs to the set a root reference may carry.
func isRootRefSyntax(refname string) bool {
	for i := 0; i < len(refname); i++ {
		c := refname[i]
		if (c < 'A' || c > 'Z') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// isPseudoRef names FETCH_HEAD and MERGE_HEAD, references git writes but does
// not treat as root references.
func isPseudoRef(refname string) bool {
	return refname == "FETCH_HEAD" || refname == "MERGE_HEAD"
}
