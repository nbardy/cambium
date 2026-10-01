package gitx

import "strings"

// MatchPath supports *, ?, character classes within one path component and **
// across path separators. It intentionally follows slash-separated Git paths,
// not host-filesystem separators.
func MatchPath(pattern, path string) bool {
	pattern = strings.TrimPrefix(strings.ReplaceAll(pattern, "\\", "/"), "./")
	path = strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "./")
	return matchSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchSegments(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		if matchSegments(pattern[1:], path) {
			return true
		}
		for i := 0; i < len(path); i++ {
			if matchSegments(pattern[1:], path[i+1:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 || !matchComponent(pattern[0], path[0]) {
		return false
	}
	return matchSegments(pattern[1:], path[1:])
}

func matchComponent(pattern, value string) bool {
	// Small wildcard matcher. Git paths are byte strings; this implementation
	// operates on runes for sane Unicode behavior in configuration.
	p := []rune(pattern)
	v := []rune(value)
	var walk func(int, int) bool
	walk = func(pi, vi int) bool {
		for pi < len(p) {
			switch p[pi] {
			case '*':
				for pi+1 < len(p) && p[pi+1] == '*' {
					pi++
				}
				if pi+1 == len(p) {
					return true
				}
				for j := vi; j <= len(v); j++ {
					if walk(pi+1, j) {
						return true
					}
				}
				return false
			case '?':
				if vi >= len(v) {
					return false
				}
				pi++
				vi++
			case '[':
				if vi >= len(v) {
					return false
				}
				end := pi + 1
				for end < len(p) && p[end] != ']' {
					end++
				}
				if end == len(p) {
					if p[pi] != v[vi] {
						return false
					}
					pi++
					vi++
					continue
				}
				negate := false
				start := pi + 1
				if start < end && (p[start] == '!' || p[start] == '^') {
					negate = true
					start++
				}
				matched := false
				for k := start; k < end; k++ {
					if k+2 < end && p[k+1] == '-' {
						if v[vi] >= p[k] && v[vi] <= p[k+2] {
							matched = true
						}
						k += 2
					} else if v[vi] == p[k] {
						matched = true
					}
				}
				if matched == negate {
					return false
				}
				pi = end + 1
				vi++
			default:
				if vi >= len(v) || p[pi] != v[vi] {
					return false
				}
				pi++
				vi++
			}
		}
		return vi == len(v)
	}
	return walk(0, 0)
}
