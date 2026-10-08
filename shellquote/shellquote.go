// Package shellquote has one job: wrap a string in single quotes so it
// survives being interpolated into a POSIX shell command literally. Not a
// general shell parser — just the one safe escaping move every module
// (and run.Become) needs when building a command string.
package shellquote

import "strings"

// Quote wraps s in single quotes, escaping any single quote already in s.
// Good enough for paths, usernames, and base64 payloads (which by
// construction never contain a single quote); not a general-purpose
// shell escaper.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
