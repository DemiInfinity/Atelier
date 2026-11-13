package web

import (
	"strings"
	"testing"
	"time"

	"atelier/internal/actions"
)

func TestANSIHTML(t *testing.T) {
	cases := []struct{ in, html, plain string }{
		{"plain <b>&", "plain &lt;b&gt;&amp;", "plain <b>&"},
		// vitest's "stderr | file" prefix: grey, then dim inside it
		{"\x1b[90mstderr\x1b[2m | a.ts\x1b[22m\x1b[39m", `<span class="a-f8">stderr</span><span class="a-f8 a-d"> | a.ts</span>`, "stderr | a.ts"},
		{" \x1b[32m✓\x1b[39m ok", ` <span class="a-f2">✓</span> ok`, " ✓ ok"},
		{"\x1b[1;31mfail\x1b[0m done", `<span class="a-f1 a-b">fail</span> done`, "fail done"},
		{"\x1b[38;5;196mred\x1b[m", `<span class="a-f9">red</span>`, "red"},
		{"\x1b[38;2;0;205;0mg\x1b[48;5;4mx", `<span class="a-f2">g</span><span class="a-f2 a-g4">x</span>`, "gx"},
		{"\x1b[7minv\x1b[27m", `<span class="a-inv">inv</span>`, "inv"},
		// cursor codes and hyperlinks go; the link text stays
		{"\x1b[2K\x1b[1Gdone \x1b]8;;https://x.test\x07link\x1b]8;;\x07!", "done link!", "done link!"},
		{"bell\x07 and\x00 nul", "bell and nul", "bell and nul"},
		{"\x1b[31m<script>", `<span class="a-f1">&lt;script&gt;</span>`, "<script>"},
	}
	for _, c := range cases {
		h, p := ansiHTML(c.in)
		if string(h) != c.html || p != c.plain {
			t.Errorf("ansiHTML(%q)\n got  %q / %q\n want %q / %q", c.in, h, p, c.html, c.plain)
		}
	}
	if lastCR("10%\r50%\r100%") != "100%" || lastCR("done\r") != "done" {
		t.Error("lastCR keeps the wrong part")
	}
}

func logLines(texts ...string) []actions.LogLine {
	var out []actions.LogLine
	for _, s := range texts {
		out = append(out, actions.LogLine{Time: time.Date(2026, 11, 13, 9, 30, 5, 0, time.UTC), Step: 0, Stream: "stdout", Text: s})
	}
	return out
}

func TestLogRowKinds(t *testing.T) {
	cases := []struct{ in, kind, label, where, body string }{
		{"::error file=src/main.ts,line=40,col=7::Build failed", logError, "Error", "src/main.ts:40:7", "Build failed"},
		{"::warning::50%25 done%0Anext", logWarning, "Warning", "", "50% done\nnext"},
		{"::notice title=Heads up::fine", logNotice, "Notice", "Heads up", "fine"},
		{"##[error]boom", logError, "Error", "", "boom"},
		{"::debug::x", logDebug, "", "", "x"},
		{"::set-output name=v::1", logCommand, "", "", ""},
		{"::add-mask::***", logCommand, "", "", ""},
		{"error TS2345: nope", logError, "", "", ""},
		{"\x1b[31merror\x1b[39m TS2345: nope", logError, "", "", ""},
		{"npm ERR! code 1", logError, "", "", ""},
		{"Error: missing", logError, "", "", ""},
		{"Process completed with exit code 2.", logError, "", "", ""},
		{"Process completed with exit code 0.", "", "", "", ""},
		{"Errors: 0", "", "", "", ""},
		{"npm \x1b[33mwarn\x1b[39m deprecated x", logWarning, "", "", ""},
		{"warning: unused", logWarning, "", "", ""},
		{"debconf: delaying package configuration, since apt-utils is not installed", logQuiet, "", "", ""},
		{"26538842dfbf: Pull complete", logPull, "", "", ""},
		{"Digest: sha256:2198e4c6", logPull, "", "", ""},
		{"Get:1 http://archive.ubuntu.com/ubuntu noble InRelease [256 kB]", logPull, "", "", ""},
		{"Waiting up to 180s for another pipeline's draft of v0.4.1-beta.5", "", "", "", ""},
		{":: not a command", "", "", "", ""},
	}
	for _, c := range cases {
		var p logParser
		r := p.row(logLines(c.in)[0])
		if r.Kind != c.kind || r.Label != c.label || r.Where != c.where || (c.body != "" && string(r.Body) != c.body) {
			t.Errorf("%q = kind %q label %q where %q body %q", c.in, r.Kind, r.Label, r.Where, r.Body)
		}
	}
}

func TestLogStopCommands(t *testing.T) {
	var p logParser
	kinds := []string{}
	for _, l := range logLines("::stop-commands::tok", "::error::not really", "::tok::", "::error::really") {
		kinds = append(kinds, p.row(l).Kind)
	}
	if strings.Join(kinds, ",") != "command,,command,error" {
		t.Errorf("kinds = %v", kinds)
	}
}

func TestBuildLogFolds(t *testing.T) {
	// The step the user found hard to read.
	blocks := buildLog(logLines(
		"Unable to find image 'ubuntu:24.04' locally",
		"24.04: Pulling from library/ubuntu",
		"26538842dfbf: Pulling fs layer",
		"26538842dfbf: Verifying Checksum",
		"26538842dfbf: Download complete",
		"26538842dfbf: Pull complete",
		"Digest: sha256:2198e4c6",
		"Status: Downloaded newer image for ubuntu:24.04",
		"debconf: delaying package configuration, since apt-utils is not installed",
		"Waiting up to 180s for another pipeline's draft of v0.4.1-beta.5",
	), 1)
	if len(blocks) != 2 || blocks[0].Fold != logPull || len(blocks[0].Rows) != 8 || blocks[1].Fold != "" || len(blocks[1].Rows) != 2 {
		t.Fatalf("blocks = %+v", blocks)
	}
	if blocks[1].Rows[0].N != 9 || blocks[1].Rows[0].Kind != logQuiet {
		t.Errorf("row after the pull = %+v", blocks[1].Rows[0])
	}
	h := string(renderLog(blocks))
	for _, want := range []string{
		`<details class="act-fold is-pull"><summary><span class="act-fold-t">Pulled image ubuntu:24.04</span><span class="act-fold-n" data-n="8">8 lines</span></summary>`,
		`<div class="act-ln is-pull" data-ln="1" data-title="ubuntu:24.04"><time class="act-ts" datetime="2026-11-13T09:30:05Z">09:30:05</time>`,
		`<div class="act-ln is-quiet" data-ln="9">`,
		`<div class="act-ln" data-ln="10">`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("rendered log lacks %s\n%s", want, h)
		}
	}

	// Groups: rows go inside, ::endgroup:: isn't shown, numbering carries on
	// from first, and a group with an error in it starts open.
	blocks = buildLog(logLines("::group::Install", "a", "error: b", "::endgroup::", "after", "x: one pull line? no", "26538842dfbf: Waiting", "tail"), 5)
	if len(blocks) != 2 || blocks[0].Fold != logGroup || blocks[0].Head.N != 5 || len(blocks[0].Rows) != 2 {
		t.Fatalf("group blocks = %+v", blocks)
	}
	if rows := blocks[1].Rows; len(rows) != 4 || rows[0].N != 9 || rows[2].Kind != logPull || rows[3].N != 12 {
		t.Errorf("after the group = %+v", rows)
	}
	h = string(renderLog(blocks))
	if !strings.Contains(h, `<details class="act-fold is-group" open><summary data-ln="5"><span class="act-fold-t">Install</span><span class="act-fold-n" data-n="2">2 lines</span>`) {
		t.Errorf("group render:\n%s", h)
	}
	if strings.Contains(h, "endgroup") || strings.Contains(h, "details class=\"act-fold is-pull") {
		t.Errorf("endgroup shown, or a single pull line folded:\n%s", h)
	}

	// A group still running is marked so live lines land in it.
	blocks = buildLog(logLines("::group::Build", "compiling"), 1)
	if !blocks[0].Unclosed || !strings.Contains(string(renderLog(blocks)), "data-act-open") {
		t.Errorf("open group not marked: %+v", blocks)
	}
}

func TestMarkFirstError(t *testing.T) {
	steps := []actStepView{{}, {}}
	steps[0].Index, steps[0].Status = 0, actions.StatusSuccess
	steps[1].Index, steps[1].Status = 1, actions.StatusFailure
	blocks := [][]logBlock{buildLog(logLines("error: harmless"), 1), buildLog(logLines("ok", "::error::real"), 1)}
	if id := markFirstError(7, steps, blocks, nil); id != "job-7-error" || blocks[1][0].Rows[1].ID != id || blocks[0][0].Rows[0].ID != "" {
		t.Errorf("first error = %q, rows %+v", id, blocks)
	}
	if !strings.Contains(string(renderLog(blocks[1])), `id="job-7-error"`) {
		t.Error("anchor not rendered")
	}
	// No error line in the failed step: an error elsewhere, else the step.
	blocks = [][]logBlock{buildLog(logLines("error: earlier"), 1), buildLog(logLines("quiet"), 1)}
	if id := markFirstError(7, steps, blocks, nil); id != "job-7-error" || blocks[0][0].Rows[0].ID != id {
		t.Errorf("fallback error = %q", id)
	}
	blocks = [][]logBlock{nil, buildLog(logLines("quiet"), 1)}
	if id := markFirstError(7, steps, blocks, nil); id != "step-7-1" {
		t.Errorf("fallback step = %q", id)
	}
}

func TestLogStreamRow(t *testing.T) {
	var p logParser
	g := streamRow(&p, logLines("::group::Install")[0])
	if g.Kind != logGroup || g.HTML != "" || !strings.HasPrefix(g.Fold, `<details class="act-fold is-group"><summary><span class="act-fold-t">Install</span>`) {
		t.Errorf("group row = %+v", g)
	}
	r := streamRow(&p, logLines("26538842dfbf: Pull complete")[0])
	if r.Kind != logPull || !strings.Contains(r.HTML, `class="act-ln is-pull"`) || strings.Contains(r.HTML, "data-ln") ||
		!strings.Contains(r.Fold, "Download progress") {
		t.Errorf("pull row = %+v", r)
	}
	if e := streamRow(&p, logLines("::endgroup::")[0]); e.Kind != logEndGroup || e.HTML != "" {
		t.Errorf("endgroup row = %+v", e)
	}
}
