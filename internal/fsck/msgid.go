package fsck

// Code in this file mirrors git's FOREACH_FSCK_MSG_ID list in fsck.h. The
// spellings are load-bearing: the camel-cased form is printed in every
// message, and the lower-cased form is the fsck.<msgid> configuration key.

// MsgID identifies one fsck check.
type MsgID int

// The checks, in the order git declares them.
const (
	MsgNulInHeader MsgID = iota
	MsgUnterminatedHeader
	MsgBadDate
	MsgBadDateOverflow
	MsgBadEmail
	MsgBadName
	MsgBadObjectSha1
	MsgBadParentSha1
	MsgBadTimezone
	MsgBadTree
	MsgBadTreeSha1
	MsgBadType
	MsgDuplicateEntries
	MsgMissingAuthor
	MsgMissingCommitter
	MsgMissingEmail
	MsgMissingNameBeforeEmail
	MsgMissingObject
	MsgMissingSpaceBeforeDate
	MsgMissingSpaceBeforeEmail
	MsgMissingTag
	MsgMissingTagEntry
	MsgMissingTree
	MsgMissingType
	MsgMissingTypeEntry
	MsgMultipleAuthors
	MsgTreeNotSorted
	MsgUnknownType
	MsgZeroPaddedDate
	MsgGitmodulesMissing
	MsgGitmodulesBlob
	MsgGitmodulesLarge
	MsgGitmodulesName
	MsgGitmodulesSymlink
	MsgGitmodulesUrl
	MsgGitmodulesPath
	MsgGitmodulesUpdate
	MsgGitattributesMissing
	MsgGitattributesLarge
	MsgGitattributesLineLength
	MsgGitattributesBlob
	MsgEmptyName
	MsgFullPathname
	MsgHasDot
	MsgHasDotdot
	MsgHasDotgit
	MsgNullSha1
	MsgZeroPaddedFilemode
	MsgNulInCommit
	MsgLargePathname
	MsgBadFilemode
	MsgGitmodulesParse
	MsgGitignoreSymlink
	MsgGitattributesSymlink
	MsgMailmapSymlink
	MsgBadTagName
	MsgMissingTaggerEntry
	MsgExtraHeaderEntry
	msgIDCount
)

// msgInfo is the printed spelling, the configuration key, and the default
// severity of one check.
type msgInfo struct {
	Camel    string
	Lower    string
	Severity Severity
}

var msgInfos = [msgIDCount]msgInfo{
	MsgNulInHeader:             {Camel: "nulInHeader", Lower: "nulinheader", Severity: SevFatal},
	MsgUnterminatedHeader:      {Camel: "unterminatedHeader", Lower: "unterminatedheader", Severity: SevFatal},
	MsgBadDate:                 {Camel: "badDate", Lower: "baddate", Severity: SevError},
	MsgBadDateOverflow:         {Camel: "badDateOverflow", Lower: "baddateoverflow", Severity: SevError},
	MsgBadEmail:                {Camel: "badEmail", Lower: "bademail", Severity: SevError},
	MsgBadName:                 {Camel: "badName", Lower: "badname", Severity: SevError},
	MsgBadObjectSha1:           {Camel: "badObjectSha1", Lower: "badobjectsha1", Severity: SevError},
	MsgBadParentSha1:           {Camel: "badParentSha1", Lower: "badparentsha1", Severity: SevError},
	MsgBadTimezone:             {Camel: "badTimezone", Lower: "badtimezone", Severity: SevError},
	MsgBadTree:                 {Camel: "badTree", Lower: "badtree", Severity: SevError},
	MsgBadTreeSha1:             {Camel: "badTreeSha1", Lower: "badtreesha1", Severity: SevError},
	MsgBadType:                 {Camel: "badType", Lower: "badtype", Severity: SevError},
	MsgDuplicateEntries:        {Camel: "duplicateEntries", Lower: "duplicateentries", Severity: SevError},
	MsgMissingAuthor:           {Camel: "missingAuthor", Lower: "missingauthor", Severity: SevError},
	MsgMissingCommitter:        {Camel: "missingCommitter", Lower: "missingcommitter", Severity: SevError},
	MsgMissingEmail:            {Camel: "missingEmail", Lower: "missingemail", Severity: SevError},
	MsgMissingNameBeforeEmail:  {Camel: "missingNameBeforeEmail", Lower: "missingnamebeforeemail", Severity: SevError},
	MsgMissingObject:           {Camel: "missingObject", Lower: "missingobject", Severity: SevError},
	MsgMissingSpaceBeforeDate:  {Camel: "missingSpaceBeforeDate", Lower: "missingspacebeforedate", Severity: SevError},
	MsgMissingSpaceBeforeEmail: {Camel: "missingSpaceBeforeEmail", Lower: "missingspacebeforeemail", Severity: SevError},
	MsgMissingTag:              {Camel: "missingTag", Lower: "missingtag", Severity: SevError},
	MsgMissingTagEntry:         {Camel: "missingTagEntry", Lower: "missingtagentry", Severity: SevError},
	MsgMissingTree:             {Camel: "missingTree", Lower: "missingtree", Severity: SevError},
	MsgMissingType:             {Camel: "missingType", Lower: "missingtype", Severity: SevError},
	MsgMissingTypeEntry:        {Camel: "missingTypeEntry", Lower: "missingtypeentry", Severity: SevError},
	MsgMultipleAuthors:         {Camel: "multipleAuthors", Lower: "multipleauthors", Severity: SevError},
	MsgTreeNotSorted:           {Camel: "treeNotSorted", Lower: "treenotsorted", Severity: SevError},
	MsgUnknownType:             {Camel: "unknownType", Lower: "unknowntype", Severity: SevError},
	MsgZeroPaddedDate:          {Camel: "zeroPaddedDate", Lower: "zeropaddeddate", Severity: SevError},
	MsgGitmodulesMissing:       {Camel: "gitmodulesMissing", Lower: "gitmodulesmissing", Severity: SevError},
	MsgGitmodulesBlob:          {Camel: "gitmodulesBlob", Lower: "gitmodulesblob", Severity: SevError},
	MsgGitmodulesLarge:         {Camel: "gitmodulesLarge", Lower: "gitmoduleslarge", Severity: SevError},
	MsgGitmodulesName:          {Camel: "gitmodulesName", Lower: "gitmodulesname", Severity: SevError},
	MsgGitmodulesSymlink:       {Camel: "gitmodulesSymlink", Lower: "gitmodulessymlink", Severity: SevError},
	MsgGitmodulesUrl:           {Camel: "gitmodulesUrl", Lower: "gitmodulesurl", Severity: SevError},
	MsgGitmodulesPath:          {Camel: "gitmodulesPath", Lower: "gitmodulespath", Severity: SevError},
	MsgGitmodulesUpdate:        {Camel: "gitmodulesUpdate", Lower: "gitmodulesupdate", Severity: SevError},
	MsgGitattributesMissing:    {Camel: "gitattributesMissing", Lower: "gitattributesmissing", Severity: SevError},
	MsgGitattributesLarge:      {Camel: "gitattributesLarge", Lower: "gitattributeslarge", Severity: SevError},
	MsgGitattributesLineLength: {Camel: "gitattributesLineLength", Lower: "gitattributeslinelength", Severity: SevError},
	MsgGitattributesBlob:       {Camel: "gitattributesBlob", Lower: "gitattributesblob", Severity: SevError},
	MsgEmptyName:               {Camel: "emptyName", Lower: "emptyname", Severity: SevWarn},
	MsgFullPathname:            {Camel: "fullPathname", Lower: "fullpathname", Severity: SevWarn},
	MsgHasDot:                  {Camel: "hasDot", Lower: "hasdot", Severity: SevWarn},
	MsgHasDotdot:               {Camel: "hasDotdot", Lower: "hasdotdot", Severity: SevWarn},
	MsgHasDotgit:               {Camel: "hasDotgit", Lower: "hasdotgit", Severity: SevWarn},
	MsgNullSha1:                {Camel: "nullSha1", Lower: "nullsha1", Severity: SevWarn},
	MsgZeroPaddedFilemode:      {Camel: "zeroPaddedFilemode", Lower: "zeropaddedfilemode", Severity: SevWarn},
	MsgNulInCommit:             {Camel: "nulInCommit", Lower: "nulincommit", Severity: SevWarn},
	MsgLargePathname:           {Camel: "largePathname", Lower: "largepathname", Severity: SevWarn},
	MsgBadFilemode:             {Camel: "badFilemode", Lower: "badfilemode", Severity: SevInfo},
	MsgGitmodulesParse:         {Camel: "gitmodulesParse", Lower: "gitmodulesparse", Severity: SevInfo},
	MsgGitignoreSymlink:        {Camel: "gitignoreSymlink", Lower: "gitignoresymlink", Severity: SevInfo},
	MsgGitattributesSymlink:    {Camel: "gitattributesSymlink", Lower: "gitattributessymlink", Severity: SevInfo},
	MsgMailmapSymlink:          {Camel: "mailmapSymlink", Lower: "mailmapsymlink", Severity: SevInfo},
	MsgBadTagName:              {Camel: "badTagName", Lower: "badtagname", Severity: SevInfo},
	MsgMissingTaggerEntry:      {Camel: "missingTaggerEntry", Lower: "missingtaggerentry", Severity: SevInfo},
	MsgExtraHeaderEntry:        {Camel: "extraHeaderEntry", Lower: "extraheaderentry", Severity: SevIgnore},
}
