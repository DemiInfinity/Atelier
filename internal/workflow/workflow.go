// Package workflow reads Actions workflow files (.atelier/workflows/*.yml,
// with .github/workflows as a fallback) into a typed form, matches them
// against events, expands matrices and pins actions to commits.
//
// It supports the commonly used subset of GitHub Actions. Keys it doesn't
// support become Warnings (shown on the run page); structural problems that
// make the workflow unusable become Errors, and such a workflow produces a
// failed run instead of a crash.
package workflow

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"atelier/internal/workflow/expr"
)

// Dirs are the folders workflows are read from, in order. The second is only
// used when the first has no workflows, so repositories imported from GitHub
// work unchanged.
var Dirs = []string{".atelier/workflows", ".github/workflows"}

// MaxFileSize bounds a workflow file.
const MaxFileSize = 512 * 1024

// MaxMatrix is the most jobs one matrix may expand to.
const MaxMatrix = 256

// DefaultTimeoutMinutes applies when a job sets none (GitHub's default).
const DefaultTimeoutMinutes = 360

// IsWorkflowFile reports whether name is a YAML workflow.
func IsWorkflowFile(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml")
}

// IsAgenticFile reports whether name is an agentic (Markdown) workflow.
func IsAgenticFile(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".md") }

type Workflow struct {
	Path        string
	Name        string
	Agentic     bool
	On          Triggers
	Env         map[string]string
	Permissions Permissions
	Concurrency *Concurrency
	Defaults    RunDefaults
	Jobs        []*Job
	Warnings    []string
	Errors      []string // the workflow can't run; a run shows these instead
	Source      []byte
}

// Valid reports whether the workflow can run.
func (w *Workflow) Valid() bool { return len(w.Errors) == 0 }

// Job finds a job by id.
func (w *Workflow) Job(id string) *Job {
	for _, j := range w.Jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

type Triggers struct {
	Push        *PushTrigger
	PullRequest *PullRequestTrigger
	Dispatch    *DispatchTrigger
	Schedule    []Schedule
	Release     *ReleaseTrigger
}

// Events lists the trigger names, for display.
func (t Triggers) Events() []string {
	var out []string
	if t.Push != nil {
		out = append(out, "push")
	}
	if t.PullRequest != nil {
		out = append(out, "pull_request")
	}
	if t.Dispatch != nil {
		out = append(out, "workflow_dispatch")
	}
	if len(t.Schedule) > 0 {
		out = append(out, "schedule")
	}
	if t.Release != nil {
		out = append(out, "release")
	}
	return out
}

type PushTrigger struct {
	Branches, BranchesIgnore []string
	Tags, TagsIgnore         []string
	Paths, PathsIgnore       []string
}

type PullRequestTrigger struct {
	Types                    []string
	Branches, BranchesIgnore []string
	Paths, PathsIgnore       []string
}

type DispatchTrigger struct {
	Inputs []Input
}

type Input struct {
	Name        string
	Description string
	Required    bool
	Default     string
	Type        string // string, boolean, choice, number, environment
	Options     []string
}

type Schedule struct {
	Spec string
	Cron *Cron
}

type ReleaseTrigger struct {
	Types []string
}

type Permissions struct {
	Contents string // read, write or ""
	Packages string
}

// PackagesWrite reports whether the job token may publish packages.
func (p Permissions) PackagesWrite() bool { return p.Packages == "write" }

type Concurrency struct {
	Group            string
	CancelInProgress bool
}

type RunDefaults struct {
	Shell            string
	WorkingDirectory string
}

type Container struct {
	Image string
	Env   map[string]string
}

type Strategy struct {
	Matrix      *Matrix
	FailFast    bool
	MaxParallel int
}

type Matrix struct {
	Expr    string // the whole matrix is one expression, e.g. ${{ fromJSON(needs.a.outputs.m) }}
	Axes    []Axis
	Include []map[string]any
	Exclude []map[string]any
	// IncludeExpr / ExcludeExpr: include/exclude given as an expression.
	IncludeExpr, ExcludeExpr string
}

type Axis struct {
	Name   string
	Values []any
	Expr   string // the axis' values are one expression
}

// Dynamic reports whether expanding the matrix needs expressions evaluated.
func (m *Matrix) Dynamic() bool {
	if m == nil {
		return false
	}
	if m.Expr != "" || m.IncludeExpr != "" || m.ExcludeExpr != "" {
		return true
	}
	for _, a := range m.Axes {
		if a.Expr != "" || anyHasExpr(a.Values) {
			return true
		}
	}
	return anyHasExprMaps(m.Include) || anyHasExprMaps(m.Exclude)
}

type Job struct {
	ID              string
	Name            string
	RunsOn          []string
	Needs           []string
	If              string
	Environment     string
	Container       *Container
	Strategy        *Strategy
	TimeoutMinutes  int
	ContinueOnError bool
	Outputs         map[string]string
	Env             map[string]string
	Defaults        RunDefaults
	Permissions     Permissions
	Steps           []Step
	Unsupported     string // a reason this job can't run (e.g. reusable workflows)
}

type Step struct {
	ID               string
	Name             string
	If               string
	Run              string
	Shell            string
	Uses             string
	With             map[string]string
	Env              map[string]string
	WorkingDirectory string
	ContinueOnError  bool
	TimeoutMinutes   int
}

// NeedsDocker reports whether the job needs a runner with Docker.
func (j *Job) NeedsDocker() bool {
	if j.Container != nil && j.Container.Image != "" {
		return true
	}
	for _, s := range j.Steps {
		if strings.HasPrefix(s.Uses, "docker://") {
			return true
		}
	}
	return false
}

// ---------- parsing ----------

// Parse reads one workflow file. It returns an error only if src isn't a
// workflow at all; problems inside the workflow land in Errors and Warnings.
func Parse(filePath string, src []byte) *Workflow {
	w := &Workflow{Path: filePath, Name: path.Base(filePath), Source: src}
	if IsAgenticFile(filePath) {
		w.Agentic = true
		w.Name = strings.TrimSuffix(path.Base(filePath), path.Ext(filePath))
		return w
	}
	if len(src) > MaxFileSize {
		w.Errors = append(w.Errors, fmt.Sprintf("The workflow file is larger than %d KB.", MaxFileSize/1024))
		return w
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		w.Errors = append(w.Errors, "Invalid YAML: "+err.Error())
		return w
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		w.Errors = append(w.Errors, "The workflow file must be a YAML mapping (name:, on:, jobs:).")
		return w
	}
	p := &parser{w: w}
	p.root(doc.Content[0])
	return w
}

type parser struct {
	w *Workflow
}

func (p *parser) warn(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	for _, x := range p.w.Warnings {
		if x == msg {
			return
		}
	}
	p.w.Warnings = append(p.w.Warnings, msg)
}

func (p *parser) fail(format string, a ...any) {
	p.w.Errors = append(p.w.Errors, fmt.Sprintf(format, a...))
}

// pairs iterates a mapping node in order.
func pairs(n *yaml.Node) [][2]*yaml.Node {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var out [][2]*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], resolveAlias(n.Content[i+1])
		if k.Tag == "!!merge" || k.Value == "<<" { // YAML merge keys
			for _, m := range mergeSources(v) {
				out = append(out, pairs(m)...)
			}
			continue
		}
		out = append(out, [2]*yaml.Node{k, v})
	}
	return out
}

func mergeSources(v *yaml.Node) []*yaml.Node {
	if v.Kind == yaml.SequenceNode {
		var out []*yaml.Node
		for _, c := range v.Content {
			out = append(out, resolveAlias(c))
		}
		return out
	}
	return []*yaml.Node{v}
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && i < 10; i++ {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	n = resolveAlias(n)
	return n == nil || (n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "" && n.Style == 0))
}

func scalar(n *yaml.Node) (string, bool) {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", false
	}
	if n.Tag == "!!null" {
		return "", true
	}
	return n.Value, true
}

// stringList reads "x" or [x, y].
func (p *parser) stringList(n *yaml.Node, what string) []string {
	n = resolveAlias(n)
	if isNull(n) {
		return nil
	}
	if s, ok := scalar(n); ok {
		return []string{s}
	}
	if n.Kind != yaml.SequenceNode {
		p.fail("%s must be a string or a list of strings.", what)
		return nil
	}
	var out []string
	for _, c := range n.Content {
		s, ok := scalar(c)
		if !ok {
			p.fail("%s must be a list of strings.", what)
			return nil
		}
		out = append(out, s)
	}
	return out
}

func (p *parser) stringMap(n *yaml.Node, what string) map[string]string {
	if isNull(n) {
		return nil
	}
	n = resolveAlias(n)
	if n.Kind == yaml.ScalarNode && expr.HasExpr(n.Value) {
		p.warn("%s given as one expression isn't supported; set each key separately.", what)
		return nil
	}
	if n.Kind != yaml.MappingNode {
		p.fail("%s must be a mapping of names to values.", what)
		return nil
	}
	out := map[string]string{}
	for _, kv := range pairs(n) {
		s, ok := scalar(kv[1])
		if !ok {
			p.fail("%s.%s must be a string, number or boolean.", what, kv[0].Value)
			continue
		}
		out[kv[0].Value] = s
	}
	return out
}

func (p *parser) boolVal(n *yaml.Node, what string, def bool) bool {
	s, ok := scalar(n)
	if !ok {
		p.fail("%s must be true or false.", what)
		return def
	}
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	case "":
		return def
	}
	if expr.HasExpr(s) {
		p.warn("%s as an expression isn't supported; using %t.", what, def)
		return def
	}
	p.fail("%s must be true or false.", what)
	return def
}

func (p *parser) intVal(n *yaml.Node, what string, def int) int {
	s, ok := scalar(n)
	if !ok {
		p.fail("%s must be a number.", what)
		return def
	}
	if expr.HasExpr(s) {
		p.warn("%s as an expression isn't supported; using %d.", what, def)
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 {
		p.fail("%s must be a positive number.", what)
		return def
	}
	return int(f)
}

func (p *parser) root(n *yaml.Node) {
	w := p.w
	sawOn, sawJobs := false, false
	for _, kv := range pairs(n) {
		key, v := kv[0].Value, kv[1]
		switch key {
		case "name":
			if s, ok := scalar(v); ok && s != "" {
				w.Name = s
			}
		case "run-name":
			p.warn("run-name isn't supported yet; runs are named after the commit.")
		case "on", "true": // YAML 1.1 parsers read a bare `on` as true
			sawOn = true
			p.triggers(v)
		case "env":
			w.Env = p.stringMap(v, "env")
		case "permissions":
			w.Permissions = p.permissions(v, "permissions", Permissions{Contents: "read"})
		case "concurrency":
			w.Concurrency = p.concurrency(v)
		case "defaults":
			w.Defaults = p.defaults(v, "defaults", RunDefaults{})
		case "jobs":
			sawJobs = true
			p.jobs(v)
		default:
			p.warn("Unsupported key %q at the top of the workflow (ignored).", key)
		}
	}
	if w.Permissions == (Permissions{}) {
		w.Permissions = Permissions{Contents: "read"}
	}
	if !sawOn {
		p.fail("The workflow has no `on:` triggers.")
	}
	if !sawJobs || len(w.Jobs) == 0 {
		p.fail("The workflow has no jobs.")
	}
	p.checkNeeds()
}

// ---------- triggers ----------

var supportedEvents = map[string]bool{"push": true, "pull_request": true, "workflow_dispatch": true, "schedule": true, "release": true}

func (p *parser) triggers(n *yaml.Node) {
	n = resolveAlias(n)
	switch n.Kind {
	case yaml.ScalarNode:
		p.trigger(n.Value, nil)
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if s, ok := scalar(c); ok {
				p.trigger(s, nil)
			}
		}
	case yaml.MappingNode:
		for _, kv := range pairs(n) {
			p.trigger(kv[0].Value, kv[1])
		}
	default:
		p.fail("`on:` must be an event name, a list of events or a mapping.")
	}
}

func (p *parser) trigger(event string, cfg *yaml.Node) {
	t := &p.w.On
	if !supportedEvents[event] {
		p.warn("The %q trigger isn't supported (ignored).", event)
		return
	}
	cfg = resolveAlias(cfg)
	empty := isNull(cfg)
	if !empty && event != "schedule" && cfg.Kind != yaml.MappingNode {
		p.fail("on.%s must be a mapping.", event)
		return
	}
	switch event {
	case "push":
		pt := &PushTrigger{}
		for _, kv := range pairs(cfg) {
			key := kv[0].Value
			list := p.stringList(kv[1], "on.push."+key)
			switch key {
			case "branches":
				pt.Branches = nonNil(list)
			case "branches-ignore":
				pt.BranchesIgnore = nonNil(list)
			case "tags":
				pt.Tags = nonNil(list)
			case "tags-ignore":
				pt.TagsIgnore = nonNil(list)
			case "paths":
				pt.Paths = nonNil(list)
			case "paths-ignore":
				pt.PathsIgnore = nonNil(list)
			default:
				p.warn("on.push.%s isn't supported (ignored).", key)
			}
		}
		if pt.Branches != nil && pt.BranchesIgnore != nil {
			p.fail("on.push can't use both branches and branches-ignore.")
		}
		if pt.Paths != nil && pt.PathsIgnore != nil {
			p.fail("on.push can't use both paths and paths-ignore.")
		}
		t.Push = pt
	case "pull_request":
		pr := &PullRequestTrigger{}
		for _, kv := range pairs(cfg) {
			key := kv[0].Value
			list := p.stringList(kv[1], "on.pull_request."+key)
			switch key {
			case "types":
				for _, ty := range list {
					switch ty {
					case "opened", "synchronize", "reopened":
						pr.Types = append(pr.Types, ty)
					default:
						p.warn("pull_request type %q isn't supported yet (ignored).", ty)
					}
				}
				if pr.Types == nil {
					pr.Types = []string{}
				}
			case "branches":
				pr.Branches = nonNil(list)
			case "branches-ignore":
				pr.BranchesIgnore = nonNil(list)
			case "paths":
				pr.Paths = nonNil(list)
			case "paths-ignore":
				pr.PathsIgnore = nonNil(list)
			default:
				p.warn("on.pull_request.%s isn't supported (ignored).", key)
			}
		}
		t.PullRequest = pr
	case "workflow_dispatch":
		d := &DispatchTrigger{}
		for _, kv := range pairs(cfg) {
			switch kv[0].Value {
			case "inputs":
				for _, in := range pairs(kv[1]) {
					d.Inputs = append(d.Inputs, p.input(in[0].Value, in[1]))
				}
			default:
				p.warn("on.workflow_dispatch.%s isn't supported (ignored).", kv[0].Value)
			}
		}
		t.Dispatch = d
	case "schedule":
		if empty || cfg.Kind != yaml.SequenceNode {
			p.fail("on.schedule must be a list of `- cron: \"…\"` entries.")
			return
		}
		for _, c := range cfg.Content {
			for _, kv := range pairs(c) {
				if kv[0].Value != "cron" {
					p.warn("on.schedule.%s isn't supported (ignored).", kv[0].Value)
					continue
				}
				spec, _ := scalar(kv[1])
				cr, err := ParseCron(spec)
				if err != nil {
					p.fail("on.schedule: %v", err)
					continue
				}
				t.Schedule = append(t.Schedule, Schedule{Spec: spec, Cron: cr})
			}
		}
	case "release":
		rt := &ReleaseTrigger{}
		for _, kv := range pairs(cfg) {
			if kv[0].Value != "types" {
				p.warn("on.release.%s isn't supported (ignored).", kv[0].Value)
				continue
			}
			rt.Types = nonNil(p.stringList(kv[1], "on.release.types"))
			for _, ty := range rt.Types {
				if ty != "published" {
					p.warn("release type %q isn't supported; only published releases trigger runs.", ty)
				}
			}
		}
		t.Release = rt
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (p *parser) input(name string, n *yaml.Node) Input {
	in := Input{Name: name, Type: "string"}
	for _, kv := range pairs(n) {
		switch kv[0].Value {
		case "description":
			in.Description, _ = scalar(kv[1])
		case "required":
			in.Required = p.boolVal(kv[1], "inputs."+name+".required", false)
		case "default":
			in.Default, _ = scalar(kv[1])
		case "type":
			ty, _ := scalar(kv[1])
			switch ty {
			case "string", "boolean", "choice", "number", "environment":
				in.Type = ty
			default:
				p.warn("Input %s has unsupported type %q; treating it as a string.", name, ty)
			}
		case "options":
			in.Options = p.stringList(kv[1], "inputs."+name+".options")
		case "deprecationMessage":
		default:
			p.warn("inputs.%s.%s isn't supported (ignored).", name, kv[0].Value)
		}
	}
	if in.Type == "choice" && len(in.Options) == 0 {
		p.fail("Input %s is a choice but has no options.", name)
	}
	return in
}

func (p *parser) permissions(n *yaml.Node, what string, def Permissions) Permissions {
	n = resolveAlias(n)
	if s, ok := scalar(n); ok {
		switch s {
		case "read-all":
			return Permissions{Contents: "read", Packages: "read"}
		case "write-all":
			return Permissions{Contents: "write", Packages: "write"}
		case "", "{}":
			return Permissions{Contents: "none"}
		}
		p.fail("%s must be read-all, write-all or a mapping.", what)
		return def
	}
	out := Permissions{Contents: "none", Packages: "none"}
	if n.Kind != yaml.MappingNode {
		p.fail("%s must be read-all, write-all or a mapping.", what)
		return def
	}
	for _, kv := range pairs(n) {
		lvl, _ := scalar(kv[1])
		if lvl != "read" && lvl != "write" && lvl != "none" {
			p.fail("%s.%s must be read, write or none.", what, kv[0].Value)
			continue
		}
		switch kv[0].Value {
		case "contents":
			out.Contents = lvl
		case "packages":
			out.Packages = lvl
		default:
			if lvl != "none" {
				p.warn("Permission %q isn't supported by Atelier job tokens (ignored).", kv[0].Value)
			}
		}
	}
	if out.Contents == "write" {
		p.warn("contents: write isn't granted to job tokens yet; jobs can read the repository only.")
	}
	return out
}

func (p *parser) concurrency(n *yaml.Node) *Concurrency {
	n = resolveAlias(n)
	if s, ok := scalar(n); ok {
		if s == "" {
			return nil
		}
		return &Concurrency{Group: s}
	}
	c := &Concurrency{}
	for _, kv := range pairs(n) {
		switch kv[0].Value {
		case "group":
			c.Group, _ = scalar(kv[1])
		case "cancel-in-progress":
			c.CancelInProgress = p.boolVal(kv[1], "concurrency.cancel-in-progress", false)
		default:
			p.warn("concurrency.%s isn't supported (ignored).", kv[0].Value)
		}
	}
	if c.Group == "" {
		p.fail("concurrency needs a group.")
		return nil
	}
	return c
}

func (p *parser) defaults(n *yaml.Node, what string, base RunDefaults) RunDefaults {
	out := base
	for _, kv := range pairs(n) {
		if kv[0].Value != "run" {
			p.warn("%s.%s isn't supported (ignored).", what, kv[0].Value)
			continue
		}
		for _, r := range pairs(kv[1]) {
			switch r[0].Value {
			case "shell":
				out.Shell, _ = scalar(r[1])
			case "working-directory":
				out.WorkingDirectory, _ = scalar(r[1])
			default:
				p.warn("%s.run.%s isn't supported (ignored).", what, r[0].Value)
			}
		}
	}
	return out
}

// ---------- jobs ----------

func validID(s string) bool {
	if s == "" || len(s) > 100 || !(s[0] == '_' || s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') {
		return false
	}
	for _, c := range s {
		if !(c == '_' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (p *parser) jobs(n *yaml.Node) {
	n = resolveAlias(n)
	if n.Kind != yaml.MappingNode {
		p.fail("jobs must be a mapping of job ids to jobs.")
		return
	}
	for _, kv := range pairs(n) {
		id := kv[0].Value
		if !validID(id) {
			p.fail("Job id %q must start with a letter or _ and contain only letters, digits, - and _.", id)
			continue
		}
		if p.w.Job(id) != nil {
			p.fail("Job id %q is used twice.", id)
			continue
		}
		if len(p.w.Jobs) >= 256 {
			p.fail("A workflow can have at most 256 jobs.")
			return
		}
		p.w.Jobs = append(p.w.Jobs, p.job(id, kv[1]))
	}
}

func (p *parser) job(id string, n *yaml.Node) *Job {
	j := &Job{ID: id, TimeoutMinutes: DefaultTimeoutMinutes, Defaults: p.w.Defaults, Permissions: p.w.Permissions}
	where := "jobs." + id
	if resolveAlias(n).Kind != yaml.MappingNode {
		p.fail("%s must be a mapping.", where)
		return j
	}
	sawSteps := false
	for _, kv := range pairs(n) {
		key, v := kv[0].Value, kv[1]
		switch key {
		case "name":
			j.Name, _ = scalar(v)
		case "runs-on":
			j.RunsOn = p.runsOn(v, where)
		case "needs":
			j.Needs = p.stringList(v, where+".needs")
		case "if":
			j.If, _ = scalar(v)
			if err := checkCond(j.If); err != nil {
				p.fail("%s.if: %v", where, err)
			}
		case "environment":
			j.Environment = p.environment(v, where)
		case "container":
			j.Container = p.container(v, where)
		case "strategy":
			j.Strategy = p.strategy(v, where)
		case "timeout-minutes":
			j.TimeoutMinutes = p.intVal(v, where+".timeout-minutes", DefaultTimeoutMinutes)
		case "continue-on-error":
			j.ContinueOnError = p.boolVal(v, where+".continue-on-error", false)
		case "outputs":
			j.Outputs = p.stringMap(v, where+".outputs")
		case "env":
			j.Env = p.stringMap(v, where+".env")
		case "defaults":
			j.Defaults = p.defaults(v, where+".defaults", j.Defaults)
		case "permissions":
			j.Permissions = p.permissions(v, where+".permissions", p.w.Permissions)
		case "steps":
			sawSteps = true
			p.steps(j, v, where)
		case "uses":
			j.Unsupported = "Reusable workflows (jobs." + id + ".uses) aren't supported yet."
			p.warn("%s", j.Unsupported)
		case "services":
			p.warn("%s.services isn't supported yet (ignored).", where)
		case "concurrency":
			p.warn("%s.concurrency isn't supported; use workflow-level concurrency (ignored).", where)
		case "with", "secrets":
			if j.Unsupported == "" {
				p.warn("%s.%s only applies to reusable workflows (ignored).", where, key)
			}
		default:
			p.warn("Unsupported key %s.%s (ignored).", where, key)
		}
	}
	if j.Unsupported == "" {
		if len(j.RunsOn) == 0 {
			p.fail("%s needs runs-on.", where)
		}
		if !sawSteps || len(j.Steps) == 0 {
			p.fail("%s has no steps.", where)
		}
	}
	if j.TimeoutMinutes <= 0 || j.TimeoutMinutes > 7*24*60 {
		j.TimeoutMinutes = DefaultTimeoutMinutes
	}
	return j
}

func checkCond(c string) error {
	c = strings.TrimSpace(c)
	if c == "" {
		return nil
	}
	if expr.HasExpr(c) && expr.Strip(c) == c {
		// "${{ a }} == b" style: interpolated text, which GitHub also allows.
		_, err := expr.Expressions(c)
		return err
	}
	return expr.Check(expr.Strip(c))
}

func (p *parser) runsOn(n *yaml.Node, where string) []string {
	n = resolveAlias(n)
	if n.Kind == yaml.MappingNode {
		var labels []string
		for _, kv := range pairs(n) {
			switch kv[0].Value {
			case "labels":
				labels = p.stringList(kv[1], where+".runs-on.labels")
			case "group":
				p.warn("%s.runs-on.group isn't supported; Atelier routes by labels (ignored).", where)
			}
		}
		return labels
	}
	return p.stringList(n, where+".runs-on")
}

func (p *parser) environment(n *yaml.Node, where string) string {
	n = resolveAlias(n)
	if s, ok := scalar(n); ok {
		return s
	}
	name := ""
	for _, kv := range pairs(n) {
		switch kv[0].Value {
		case "name":
			name, _ = scalar(kv[1])
		case "url":
			p.warn("%s.environment.url isn't shown yet (ignored).", where)
		default:
			p.warn("%s.environment.%s isn't supported (ignored).", where, kv[0].Value)
		}
	}
	return name
}

func (p *parser) container(n *yaml.Node, where string) *Container {
	n = resolveAlias(n)
	if s, ok := scalar(n); ok {
		if s == "" {
			return nil
		}
		return &Container{Image: s}
	}
	c := &Container{}
	for _, kv := range pairs(n) {
		switch kv[0].Value {
		case "image":
			c.Image, _ = scalar(kv[1])
		case "env":
			c.Env = p.stringMap(kv[1], where+".container.env")
		default:
			p.warn("%s.container.%s isn't supported yet (ignored).", where, kv[0].Value)
		}
	}
	if c.Image == "" {
		p.fail("%s.container needs an image.", where)
		return nil
	}
	return c
}

func (p *parser) strategy(n *yaml.Node, where string) *Strategy {
	s := &Strategy{FailFast: true}
	for _, kv := range pairs(n) {
		switch kv[0].Value {
		case "matrix":
			s.Matrix = p.matrix(kv[1], where)
		case "fail-fast":
			s.FailFast = p.boolVal(kv[1], where+".strategy.fail-fast", true)
		case "max-parallel":
			s.MaxParallel = p.intVal(kv[1], where+".strategy.max-parallel", 0)
		default:
			p.warn("%s.strategy.%s isn't supported (ignored).", where, kv[0].Value)
		}
	}
	return s
}

func (p *parser) matrix(n *yaml.Node, where string) *Matrix {
	n = resolveAlias(n)
	m := &Matrix{}
	if s, ok := scalar(n); ok {
		if !expr.HasExpr(s) {
			p.fail("%s.strategy.matrix must be a mapping or an expression.", where)
			return nil
		}
		m.Expr = s
		return m
	}
	if n.Kind != yaml.MappingNode {
		p.fail("%s.strategy.matrix must be a mapping.", where)
		return nil
	}
	for _, kv := range pairs(n) {
		key, v := kv[0].Value, resolveAlias(kv[1])
		switch key {
		case "include", "exclude":
			var list []map[string]any
			exprStr := ""
			if s, ok := scalar(v); ok && expr.HasExpr(s) {
				exprStr = s
			} else {
				var raw []map[string]any
				if err := v.Decode(&raw); err != nil {
					p.fail("%s.strategy.matrix.%s must be a list of mappings.", where, key)
					continue
				}
				list = raw
			}
			if key == "include" {
				m.Include, m.IncludeExpr = list, exprStr
			} else {
				m.Exclude, m.ExcludeExpr = list, exprStr
			}
		default:
			ax := Axis{Name: key}
			if s, ok := scalar(v); ok && expr.HasExpr(s) {
				ax.Expr = s
			} else if v.Kind == yaml.SequenceNode {
				var vals []any
				if err := v.Decode(&vals); err != nil {
					p.fail("%s.strategy.matrix.%s: %v", where, key, err)
					continue
				}
				ax.Values = vals
				if len(vals) == 0 {
					p.fail("%s.strategy.matrix.%s has no values.", where, key)
				}
			} else {
				p.fail("%s.strategy.matrix.%s must be a list.", where, key)
				continue
			}
			m.Axes = append(m.Axes, ax)
		}
	}
	return m
}

func (p *parser) steps(j *Job, n *yaml.Node, where string) {
	n = resolveAlias(n)
	if n.Kind != yaml.SequenceNode {
		p.fail("%s.steps must be a list.", where)
		return
	}
	ids := map[string]bool{}
	for i, c := range n.Content {
		sw := fmt.Sprintf("%s.steps[%d]", where, i)
		if len(j.Steps) >= 1000 {
			p.fail("%s has more than 1000 steps.", where)
			return
		}
		if resolveAlias(c).Kind != yaml.MappingNode {
			p.fail("%s must be a mapping.", sw)
			continue
		}
		s := Step{}
		for _, kv := range pairs(c) {
			key, v := kv[0].Value, kv[1]
			switch key {
			case "id":
				s.ID, _ = scalar(v)
				if !validID(s.ID) {
					p.fail("%s.id %q must start with a letter or _ and contain only letters, digits, - and _.", sw, s.ID)
				} else if ids[s.ID] {
					p.fail("%s.id %q is used twice.", sw, s.ID)
				}
				ids[s.ID] = true
			case "name":
				s.Name, _ = scalar(v)
			case "if":
				s.If, _ = scalar(v)
				if err := checkCond(s.If); err != nil {
					p.fail("%s.if: %v", sw, err)
				}
			case "run":
				s.Run, _ = scalar(v)
			case "shell":
				s.Shell, _ = scalar(v)
			case "uses":
				s.Uses, _ = scalar(v)
			case "with":
				s.With = p.stringMap(v, sw+".with")
			case "env":
				s.Env = p.stringMap(v, sw+".env")
			case "working-directory":
				s.WorkingDirectory, _ = scalar(v)
			case "continue-on-error":
				s.ContinueOnError = p.boolVal(v, sw+".continue-on-error", false)
			case "timeout-minutes":
				s.TimeoutMinutes = p.intVal(v, sw+".timeout-minutes", 0)
			default:
				p.warn("Unsupported key %s.%s (ignored).", sw, key)
			}
		}
		switch {
		case s.Run != "" && s.Uses != "":
			p.fail("%s has both run and uses; a step does one or the other.", sw)
		case s.Run == "" && s.Uses == "":
			p.fail("%s needs run or uses.", sw)
		case s.Uses != "":
			if _, err := ParseUses(s.Uses); err != nil {
				p.fail("%s.uses: %v", sw, err)
			}
		}
		if s.Run != "" {
			if s.Shell == "" {
				s.Shell = j.Defaults.Shell
			}
			if s.WorkingDirectory == "" {
				s.WorkingDirectory = j.Defaults.WorkingDirectory
			}
		}
		for _, str := range s.strings() {
			if _, err := expr.Expressions(str); err != nil {
				p.fail("%s: %v", sw, err)
				break
			}
		}
		j.Steps = append(j.Steps, s)
	}
}

func (s Step) strings() []string {
	out := []string{s.Name, s.Run, s.Uses, s.WorkingDirectory}
	for _, m := range []map[string]string{s.With, s.Env} {
		for _, v := range m {
			out = append(out, v)
		}
	}
	return out
}

// checkNeeds validates needs references and rejects cycles.
func (p *parser) checkNeeds() {
	w := p.w
	for _, j := range w.Jobs {
		for _, n := range j.Needs {
			if n == j.ID {
				p.fail("Job %s needs itself.", j.ID)
			} else if w.Job(n) == nil {
				p.fail("Job %s needs %q, which doesn't exist.", j.ID, n)
			}
		}
	}
	if len(w.Errors) > 0 {
		return
	}
	if _, err := Order(w.Jobs); err != nil {
		p.fail("%v", err)
	}
}

// Order returns job ids in dependency order (needs first), or an error naming a cycle.
func Order(jobs []*Job) ([]string, error) {
	state := map[string]int{}
	byID := map[string]*Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	var out []string
	var visit func(id string, trail []string) error
	visit = func(id string, trail []string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("Jobs depend on each other in a loop: %s.", strings.Join(append(trail, id), " → "))
		case 2:
			return nil
		}
		state[id] = 1
		if j := byID[id]; j != nil {
			for _, n := range j.Needs {
				if err := visit(n, append(trail, id)); err != nil {
					return err
				}
			}
		}
		state[id] = 2
		out = append(out, id)
		return nil
	}
	for _, j := range jobs {
		if err := visit(j.ID, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Stages groups job ids by depth in the needs graph, for drawing it.
func Stages(jobs []*Job) [][]string {
	depth := map[string]int{}
	order, err := Order(jobs)
	if err != nil {
		return nil
	}
	byID := map[string]*Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	maxD := 0
	for _, id := range order {
		d := 0
		for _, n := range byID[id].Needs {
			if depth[n]+1 > d {
				d = depth[n] + 1
			}
		}
		depth[id] = d
		if d > maxD {
			maxD = d
		}
	}
	out := make([][]string, maxD+1)
	for _, j := range jobs {
		out[depth[j.ID]] = append(out[depth[j.ID]], j.ID)
	}
	return out
}

// Uses lists every distinct `uses:` in the workflow, sorted.
func (w *Workflow) Uses() []string {
	seen := map[string]bool{}
	var out []string
	for _, j := range w.Jobs {
		for _, s := range j.Steps {
			if s.Uses != "" && !seen[s.Uses] {
				seen[s.Uses] = true
				out = append(out, s.Uses)
			}
		}
	}
	sort.Strings(out)
	return out
}

func anyHasExpr(vals []any) bool {
	for _, v := range vals {
		if s, ok := v.(string); ok && expr.HasExpr(s) {
			return true
		}
	}
	return false
}

func anyHasExprMaps(ms []map[string]any) bool {
	for _, m := range ms {
		for _, v := range m {
			if s, ok := v.(string); ok && expr.HasExpr(s) {
				return true
			}
		}
	}
	return false
}
