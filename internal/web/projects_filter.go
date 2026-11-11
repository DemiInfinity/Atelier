package web

import (
	"strconv"
	"strings"

	"atelier/internal/store"
)

// The project filter language, kept in the URL (?q=…):
//
//	status:"In progress"     a field by name (spaces may be written as -: target-date:…)
//	status:Todo,Done         any of several values
//	repo:Marbl  repo:zoe/x   repository name or full name
//	is:open is:closed is:merged is:draft is:issue is:pr
//	label:bug                an issue label
//	no:status no:label       empty field / no labels
//	estimate:>3 target-date:<2026-12-01   comparisons for number and date fields
//	-label:wontfix           any qualifier can be negated
//	anything else            words that must appear in the title
//
// Terms are ANDed. Items the viewer can't read ("Private item") only match
// filters on the project's own fields, never on their title, repository,
// labels or state, so a filter can't be used to probe them.

type projTerm struct {
	Key  string // lower case; "" for free text
	Vals []string
	Neg  bool
}

type projFilter struct {
	Terms []projTerm
}

// projTokens splits a query on spaces, keeping "quoted phrases" whole (also
// after a qualifier, as in status:"In progress").
func projTokens(q string) []string {
	var out []string
	var cur strings.Builder
	inQuote, has := false, false
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
			has = true
		case !inQuote && (r == ' ' || r == '\t' || r == '\n'):
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		out = append(out, cur.String())
	}
	return out
}

func parseProjFilter(q string) projFilter {
	var f projFilter
	for _, tok := range projTokens(q) {
		if tok == "" {
			continue
		}
		neg := false
		body := tok
		if strings.HasPrefix(body, "-") && len(body) > 1 && strings.Contains(body, ":") {
			neg, body = true, body[1:]
		}
		key, val, ok := strings.Cut(body, ":")
		if !ok || key == "" || val == "" {
			f.Terms = append(f.Terms, projTerm{Vals: []string{strings.ToLower(tok)}})
			continue
		}
		var vals []string
		for _, v := range strings.Split(val, ",") {
			if v = strings.TrimSpace(v); v != "" {
				vals = append(vals, v)
			}
		}
		if len(vals) == 0 {
			continue
		}
		f.Terms = append(f.Terms, projTerm{Key: strings.ToLower(key), Vals: vals, Neg: neg})
	}
	return f
}

// projFieldKey is how a field is named in a filter: lower case, spaces as -.
func projFieldKey(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), "-"))
}

func projFindField(fields []store.ProjField, key string) (store.ProjField, bool) {
	for _, f := range fields {
		if projFieldKey(f.Name) == key || strings.EqualFold(f.Name, key) {
			return f, true
		}
	}
	return store.ProjField{}, false
}

// contentTerm reports whether a term looks at an item's content (title,
// repository, labels, state) rather than the project's own fields.
func (t projTerm) contentTerm(fields []store.ProjField) bool {
	switch t.Key {
	case "":
		return true
	case "repo", "repository", "label", "is", "state", "type":
		return true
	case "no":
		for _, v := range t.Vals {
			if strings.EqualFold(v, "label") || strings.EqualFold(v, "labels") {
				return true
			}
		}
		return false
	}
	_, ok := projFindField(fields, t.Key)
	return !ok // unknown qualifiers are free text
}

// match reports whether an item passes the filter. visible says whether the
// viewer may read the item's content.
func (f projFilter) match(it store.ProjItem, visible bool, fields []store.ProjField) bool {
	for _, t := range f.Terms {
		if !visible && !it.IsDraft() && t.contentTerm(fields) {
			return false
		}
		ok := t.match(it, fields)
		if t.Neg {
			ok = !ok
		}
		if !ok {
			return false
		}
	}
	return true
}

func (t projTerm) match(it store.ProjItem, fields []store.ProjField) bool {
	any := func(pred func(v string) bool) bool {
		for _, v := range t.Vals {
			if pred(v) {
				return true
			}
		}
		return false
	}
	switch t.Key {
	case "":
		return strings.Contains(strings.ToLower(it.Title), t.Vals[0])
	case "repo", "repository":
		return !it.IsDraft() && any(func(v string) bool {
			return strings.EqualFold(v, it.RepoName) || strings.EqualFold(v, it.RepoFullName())
		})
	case "label":
		return any(func(v string) bool {
			for _, l := range it.Labels {
				if strings.EqualFold(l.Name, v) {
					return true
				}
			}
			return false
		})
	case "is", "state", "type":
		return any(func(v string) bool {
			switch strings.ToLower(v) {
			case "open":
				return it.State == "open"
			case "closed":
				return it.State == "closed" || it.State == "merged"
			case "merged":
				return it.State == "merged"
			case "draft":
				return it.IsDraft()
			case "issue":
				return it.Kind == "issue"
			case "pr", "pull", "pullrequest", "pull-request":
				return it.Kind == "pull"
			}
			return false
		})
	case "no":
		return any(func(v string) bool {
			if strings.EqualFold(v, "label") || strings.EqualFold(v, "labels") {
				return len(it.Labels) == 0
			}
			if fd, ok := projFindField(fields, projFieldKey(v)); ok {
				return it.Values[fd.ID] == ""
			}
			return false
		})
	}
	fd, ok := projFindField(fields, t.Key)
	if !ok {
		// An unknown qualifier is just text.
		return strings.Contains(strings.ToLower(it.Title), strings.ToLower(t.Key+":"+strings.Join(t.Vals, ",")))
	}
	have := it.Values[fd.ID]
	return any(func(v string) bool { return projValueMatch(fd, have, v) })
}

// projValueMatch compares a stored value with one filter value.
func projValueMatch(f store.ProjField, have, want string) bool {
	switch f.Kind {
	case store.ProjFieldSelect:
		o, ok := f.Option(have)
		return ok && strings.EqualFold(o.Name, want)
	case store.ProjFieldText:
		return have != "" && strings.Contains(strings.ToLower(have), strings.ToLower(want))
	case store.ProjFieldNumber, store.ProjFieldDate:
		if have == "" {
			return false
		}
		op, rest := projCmpOp(want)
		var c int
		if f.Kind == store.ProjFieldNumber {
			h, err1 := strconv.ParseFloat(have, 64)
			w, err2 := strconv.ParseFloat(rest, 64)
			if err1 != nil || err2 != nil {
				return false
			}
			switch {
			case h < w:
				c = -1
			case h > w:
				c = 1
			}
		} else {
			c = strings.Compare(have, rest) // YYYY-MM-DD sorts as text
		}
		switch op {
		case ">":
			return c > 0
		case ">=":
			return c >= 0
		case "<":
			return c < 0
		case "<=":
			return c <= 0
		}
		return c == 0
	}
	return false
}

func projCmpOp(s string) (string, string) {
	for _, op := range []string{">=", "<=", ">", "<"} {
		if strings.HasPrefix(s, op) {
			return op, strings.TrimSpace(s[len(op):])
		}
	}
	return "=", s
}
