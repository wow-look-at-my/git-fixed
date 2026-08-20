package fsck

import "strings"

// Flags for CheckRefnameFormat, matching git's REFNAME_* values.
const (
	// RefnameAllowOnelevel permits a name with a single component.
	RefnameAllowOnelevel = 1 << iota
	// RefnameRefspecPattern permits exactly one "*" component.
	RefnameRefspecPattern
)

// refnameDisposition classifies every byte, exactly as git's table does.
//
//	1 ends the component, 2 is '.', 3 is '{', 4 is forbidden, 5 is '*'.
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
			// Only one side of a refspec may hold the asterisk.
			*flags &^= RefnameRefspecPattern
		}
		last = ch
	}
out:
	if i == 0 {
		return 0, true // a zero-length component, which the caller rejects
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
