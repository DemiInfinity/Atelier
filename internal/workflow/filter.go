package workflow

import (
	"regexp"
	"strings"
	"sync"
)

// Filter patterns follow GitHub's cheat sheet: * matches anything but "/",
// ** matches anything, ? and + repeat the previous character (zero-or-one,
// one-or-more), [...] is a character class, and a leading ! negates. In a
// list, later patterns override earlier ones.

var globCache sync.Map // pattern → *regexp.Regexp

func globRe(pat string) *regexp.Regexp {
	if re, ok := globCache.Load(pat); ok {
		return re.(*regexp.Regexp)
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch {
		case strings.HasPrefix(pat[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(pat[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?' || c == '+':
			if i == 0 {
				b.WriteString(regexp.QuoteMeta(string(c)))
			} else {
				b.WriteByte(c)
			}
		case c == '[':
			j := strings.IndexByte(pat[i:], ']')
			if j > 1 {
				class := pat[i+1 : i+j]
				b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
				i += j
			} else {
				b.WriteString(`\[`)
			}
		case c == '\\' && i+1 < len(pat):
			b.WriteString(regexp.QuoteMeta(pat[i+1 : i+2]))
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		re = regexp.MustCompile("^" + regexp.QuoteMeta(pat) + "$")
	}
	globCache.Store(pat, re)
	return re
}

// MatchGlob reports whether s matches one pattern (without negation).
func MatchGlob(pattern, s string) bool { return globRe(pattern).MatchString(s) }

// MatchList applies an ordered pattern list with ! negations.
func MatchList(patterns []string, s string) bool {
	matched := false
	for _, p := range patterns {
		if neg, ok := strings.CutPrefix(p, "!"); ok {
			if MatchGlob(neg, s) {
				matched = false
			}
			continue
		}
		if MatchGlob(p, s) {
			matched = true
		}
	}
	return matched
}

// filterNames applies include / ignore lists to one name. A nil include list
// with a nil ignore list matches everything.
func filterNames(include, ignore []string, name string) bool {
	switch {
	case include != nil:
		return MatchList(include, name)
	case ignore != nil:
		return !MatchList(ignore, name)
	}
	return true
}

// filterPaths: with include patterns, any changed file must match; with
// ignore patterns, at least one changed file must not be ignored. changed nil
// means the change set is unknown (e.g. a new branch without history in
// common), which counts as a match.
func filterPaths(include, ignore []string, changed []string) bool {
	if changed == nil || (include == nil && ignore == nil) {
		return true
	}
	for _, f := range changed {
		if include != nil && MatchList(include, f) {
			return true
		}
		if include == nil && !MatchList(ignore, f) {
			return true
		}
	}
	return false
}

// MatchPush reports whether a push to ref (refs/heads/x or refs/tags/x)
// changing these files triggers the workflow.
func (t *PushTrigger) MatchPush(ref string, changed []string) bool {
	if t == nil {
		return false
	}
	if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		hasBranchFilter := t.Branches != nil || t.BranchesIgnore != nil
		hasTagFilter := t.Tags != nil || t.TagsIgnore != nil
		if !hasBranchFilter && hasTagFilter {
			return false // only tags were asked for
		}
		return filterNames(t.Branches, t.BranchesIgnore, branch) && filterPaths(t.Paths, t.PathsIgnore, changed)
	}
	if tag, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		hasBranchFilter := t.Branches != nil || t.BranchesIgnore != nil
		hasTagFilter := t.Tags != nil || t.TagsIgnore != nil
		if hasBranchFilter && !hasTagFilter {
			return false // only branches were asked for
		}
		if !hasTagFilter && (t.Paths != nil || t.PathsIgnore != nil) {
			return false // path filters never match tag pushes
		}
		return filterNames(t.Tags, t.TagsIgnore, tag)
	}
	return false
}

// MatchPullRequest reports whether a pull request event triggers the workflow.
// action is opened, synchronize or reopened; base is the target branch.
func (t *PullRequestTrigger) MatchPullRequest(action, base string, changed []string) bool {
	if t == nil {
		return false
	}
	types := t.Types
	if types == nil {
		types = []string{"opened", "synchronize", "reopened"}
	}
	found := false
	for _, ty := range types {
		found = found || ty == action
	}
	return found && filterNames(t.Branches, t.BranchesIgnore, base) && filterPaths(t.Paths, t.PathsIgnore, changed)
}

// MatchRelease reports whether a release action triggers the workflow.
func (t *ReleaseTrigger) MatchRelease(action string) bool {
	if t == nil {
		return false
	}
	if t.Types == nil || len(t.Types) == 0 {
		return action == "published"
	}
	for _, ty := range t.Types {
		if ty == action {
			return true
		}
	}
	return false
}
