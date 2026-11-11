package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"unicode"
	"unicode/utf8"

	"atelier/internal/store"
)

// Friendly messages: what the clients say after a push, a green run, a
// merge… Stored per person so every device says the same things. "ours" and
// "plain" are fixed (docs/app-api.md §9); a person adds their own lines,
// picks a tone and sets the rules.

const (
	appMessagesPerMoment = 20
	appMessageMaxRunes   = 140
)

// appMoments are the moments, in the contract's order.
var appMoments = []string{"push", "clean", "pass", "fail", "merge", "first", "late", "mile", "agent"}

var appMessagesOurs = map[string][]string{
	"push":  {"Home and dry. {branch} is safe in your studio.", "Off it goes. {count} commits landed on {repo}.", "Pushed. Checks are warming up on {device}."},
	"clean": {"All tidy. The kettle is free.", "Nothing to commit. Go and make something."},
	"pass":  {"Green across the board on {device}.", "Checks passed. A little smugness is allowed."},
	"fail":  {"One test disagrees with you on {branch}.", "Not this time. The log knows why."},
	"merge": {"Merged into main. Nicely done.", "In it goes. {repo} is {count} commits better."},
	"first": {"Good morning. First one of the day.", "And we are off."},
	"late":  {"Pushed at {time}. It will still be here tomorrow.", "Saved. Now go to bed."},
	"mile":  {"That was commit {count} on {repo}."},
	"agent": {"{workflow} left you a note.", "{workflow} finished while you were busy."},
}

var appMessagesPlain = map[string]string{
	"push": "Pushed {branch}.", "clean": "No changes.", "pass": "Checks passed.", "fail": "Checks failed.",
	"merge": "Merged into {base}.", "first": "Committed.", "late": "Pushed {branch}.", "mile": "Committed.",
	"agent": "{workflow} finished.",
}

var appMessageRules = []string{"quiet_fullscreen", "terminal", "milestones"}

var appPlaceholders = map[string]bool{"repo": true, "branch": true, "base": true, "count": true, "device": true, "time": true, "workflow": true}

var appPlaceholderRe = regexp.MustCompile(`\{([^{}]*)\}`)

// appMessagesDoc is what's stored for a person.
type appMessagesDoc struct {
	Tone  string              `json:"tone"`
	Yours map[string][]string `json:"yours"`
	Rules map[string]bool     `json:"rules"`
}

func appMessagesDefault() appMessagesDoc {
	d := appMessagesDoc{Tone: "friendly", Yours: map[string][]string{}, Rules: map[string]bool{}}
	for _, r := range appMessageRules {
		d.Rules[r] = true
	}
	return d
}

func (a *App) appMessagesLoad(r *http.Request, personID int64) (appMessagesDoc, error) {
	d := appMessagesDefault()
	raw, err := a.db.AppMessages(r.Context(), personID)
	if errors.Is(err, store.ErrNotFound) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	var saved appMessagesDoc
	if json.Unmarshal([]byte(raw), &saved) != nil {
		return d, nil
	}
	if saved.Tone == "friendly" || saved.Tone == "plain" || saved.Tone == "off" {
		d.Tone = saved.Tone
	}
	for _, m := range appMoments {
		if ls := saved.Yours[m]; len(ls) > 0 {
			d.Yours[m] = ls
		}
	}
	for _, k := range appMessageRules {
		if v, ok := saved.Rules[k]; ok {
			d.Rules[k] = v
		}
	}
	return d, nil
}

// json is the full object of §9.
func (d appMessagesDoc) json() map[string]any {
	moments := map[string]any{}
	for _, m := range appMoments {
		yours := d.Yours[m]
		if yours == nil {
			yours = []string{}
		}
		moments[m] = map[string]any{"ours": appMessagesOurs[m], "yours": yours}
	}
	return map[string]any{"tone": d.Tone, "moments": moments, "plain": appMessagesPlain, "rules": d.Rules}
}

func (a *App) appMessagesGet(w http.ResponseWriter, r *http.Request, c *appCaller) {
	d, err := a.appMessagesLoad(r, c.Who.PersonID)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d.json())
}

// appCheckMessage validates one of a person's own lines. It returns the
// machine code and sentence of the problem, or "" if it's fine.
func appCheckMessage(s string) (code, msg string) {
	switch {
	case s == "":
		return "empty_message", "A message can't be empty."
	case !utf8.ValidString(s):
		return "invalid_message", "Messages must be plain text."
	case utf8.RuneCountInString(s) > appMessageMaxRunes:
		return "message_too_long", "Keep each message to 140 characters."
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == '<' || r == '>' {
			return "invalid_message", "Messages are plain text on one line: no line breaks, tabs or markup."
		}
	}
	for _, m := range appPlaceholderRe.FindAllStringSubmatch(s, -1) {
		if !appPlaceholders[m[1]] {
			return "invalid_placeholder", "Unknown placeholder {" + m[1] + "}. Use {repo}, {branch}, {base}, {count}, {device}, {time} or {workflow}."
		}
	}
	return "", ""
}

func (a *App) appMessagesPut(w http.ResponseWriter, r *http.Request, c *appCaller) {
	var in struct {
		Tone  *string             `json:"tone"`
		Yours map[string][]string `json:"yours"`
		Rules map[string]*bool    `json:"rules"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	d, err := a.appMessagesLoad(r, c.Who.PersonID)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	invalid := func(code, msg string) { appError(w, http.StatusUnprocessableEntity, code, msg) }
	if in.Tone != nil {
		if *in.Tone != "friendly" && *in.Tone != "plain" && *in.Tone != "off" {
			invalid("invalid_tone", "tone must be friendly, plain or off.")
			return
		}
		d.Tone = *in.Tone
	}
	for moment, lines := range in.Yours {
		if _, ok := appMessagesOurs[moment]; !ok {
			invalid("invalid_moment", "There's no moment called "+appCleanText(moment, 40)+".")
			return
		}
		if len(lines) > appMessagesPerMoment {
			invalid("too_many_messages", "Keep it to 20 messages per moment.")
			return
		}
		clean := []string{}
		for _, l := range lines {
			if code, msg := appCheckMessage(l); code != "" {
				invalid(code, msg)
				return
			}
			clean = append(clean, l)
		}
		if len(clean) == 0 {
			delete(d.Yours, moment)
		} else {
			d.Yours[moment] = clean
		}
	}
	for k, v := range in.Rules {
		if _, ok := d.Rules[k]; !ok || v == nil {
			invalid("invalid_rule", "rules are quiet_fullscreen, terminal and milestones, each true or false.")
			return
		}
		d.Rules[k] = *v
	}
	b, _ := json.Marshal(d)
	if err := a.db.SetAppMessages(r.Context(), c.Who.PersonID, string(b)); err != nil {
		a.appFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d.json())
}
