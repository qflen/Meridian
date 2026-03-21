package storage

import "regexp"

// regexpCompiled wraps the standard regexp for matching.
type regexpCompiled = *regexp.Regexp

// compileRegexp compiles a regular expression pattern.
func compileRegexp(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(pattern)
}
