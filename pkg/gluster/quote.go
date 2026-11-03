package gluster

import "strings"

// shellQuote returns args as one POSIX shell command line in which every
// argument is a single-quoted word.
//
// Every argument is quoted, including the ones that would survive without
// quotes: a uniform rule is easier to audit and lets the wrapper of T-19 split
// the command line again without guessing. A single quote inside an argument is
// replaced by close-quote, backslash, quote, open-quote, which is the only way to
// put a quote inside a single quoted word. An empty argument becomes two single
// quotes.
//
// The remote login shell has to be POSIX compatible (bash, dash, zsh, sh) for the
// result to mean what it says.
func shellQuote(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
