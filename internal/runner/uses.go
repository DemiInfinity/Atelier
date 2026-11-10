package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"atelier/internal/actions"
)

// actionDef is action.yml / action.yaml.
type actionDef struct {
	Name    string                  `yaml:"name"`
	Inputs  map[string]actionInput  `yaml:"inputs"`
	Outputs map[string]actionOutput `yaml:"outputs"`
	Runs    actionRuns              `yaml:"runs"`
}

type actionInput struct {
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
}

type actionOutput struct {
	Description string `yaml:"description"`
	Value       string `yaml:"value"`
}

type actionRuns struct {
	Using          string            `yaml:"using"`
	Main           string            `yaml:"main"`
	Pre            string            `yaml:"pre"`
	PreIf          string            `yaml:"pre-if"`
	Post           string            `yaml:"post"`
	PostIf         string            `yaml:"post-if"`
	Steps          []compositeStep   `yaml:"steps"`
	Image          string            `yaml:"image"`
	Entrypoint     string            `yaml:"entrypoint"`
	PostEntrypoint string            `yaml:"post-entrypoint"`
	Args           []string          `yaml:"args"`
	Env            map[string]string `yaml:"env"`
}

type compositeStep struct {
	ID               string            `yaml:"id"`
	Name             string            `yaml:"name"`
	If               string            `yaml:"if"`
	Run              string            `yaml:"run"`
	Shell            string            `yaml:"shell"`
	Uses             string            `yaml:"uses"`
	With             map[string]string `yaml:"with"`
	Env              map[string]string `yaml:"env"`
	WorkingDirectory string            `yaml:"working-directory"`
	ContinueOnError  string            `yaml:"continue-on-error"`
	TimeoutMinutes   int               `yaml:"timeout-minutes"`
}

func (a *actionDef) kind() string {
	u := strings.ToLower(a.Runs.Using)
	switch {
	case strings.HasPrefix(u, "node"):
		return "node"
	case u == "composite", u == "docker":
		return u
	}
	return u
}

func loadActionDef(dir string) (*actionDef, error) {
	var b []byte
	var err error
	for _, n := range []string{"action.yml", "action.yaml"} {
		if b, err = os.ReadFile(filepath.Join(dir, n)); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("no action.yml or action.yaml in the action")
	}
	var a actionDef
	if err := yaml.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("parse action.yml: %w", err)
	}
	switch a.kind() {
	case "node", "composite", "docker":
	default:
		return nil, fmt.Errorf("unsupported action type runs.using=%q", a.Runs.Using)
	}
	return &a, nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// actionRef is owner/repo[/sub]@ref.
type actionRef struct {
	owner, repo, sub, ref string
	atelier               bool // atelier:owner/repo@sha — fetched from this Atelier server, not GitHub
}

func parseUses(u string) (actionRef, error) {
	fromAtelier := false
	if rest, ok := strings.CutPrefix(u, "atelier:"); ok {
		fromAtelier, u = true, rest
	}
	spec, ref, ok := strings.Cut(u, "@")
	parts := strings.Split(spec, "/")
	if !ok || len(parts) < 2 || parts[0] == "" || parts[1] == "" || ref == "" {
		return actionRef{}, fmt.Errorf("invalid uses %q: want owner/repo[/path]@sha", u)
	}
	for _, p := range parts {
		if p == ".." || p == "." || strings.ContainsAny(p, `\:`) {
			return actionRef{}, fmt.Errorf("invalid uses %q", u)
		}
	}
	return actionRef{owner: parts[0], repo: parts[1], sub: strings.Join(parts[2:], "/"), ref: ref, atelier: fromAtelier}, nil
}

func isCheckout(u string) bool {
	spec, _, _ := strings.Cut(strings.ToLower(u), "@")
	return spec == "actions/checkout"
}

// prepareActions downloads every remote action up front ("Set up job") and
// runs JavaScript `pre` scripts.
func (j *jobRun) prepareActions(sc *scope, steps []stepDef) error {
	for _, st := range steps {
		u := strings.TrimSpace(st.Uses)
		if u == "" || strings.HasPrefix(u, "docker://") || strings.HasPrefix(u, "./") || isCheckout(u) || strings.Contains(u, "${{") {
			continue
		}
		dir, err := j.actionDir(j.ctx, u)
		if err != nil {
			return err
		}
		act, err := loadActionDef(dir)
		if err != nil {
			return fmt.Errorf("%s: %w", u, err)
		}
		if act.kind() == "docker" && !j.r.Config.HasCap(actions.CapDocker) {
			return fmt.Errorf("%s is a docker action but the docker capability is off on this runner", u)
		}
		if act.kind() == "node" && act.Runs.Pre != "" {
			sc.logStep = st.Index
			cond := act.Runs.PreIf
			if cond == "" {
				cond = "always()"
			}
			if ok, err := j.eval(sc, nil).EvalCondition(cond); err != nil || !ok {
				continue
			}
			j.log.System(st.Index, "Pre "+st.displayName())
			inputs, err := j.actionInputs(sc, st, act)
			if err != nil {
				return err
			}
			code, err := j.runNodeScript(j.ctx, sc, st, act, dir, act.Runs.Pre, inputs, nil, newCmdState(), nil)
			if err != nil || code != 0 {
				return fmt.Errorf("pre step of %s failed (exit %d): %v", u, code, err)
			}
		}
	}
	return nil
}

// actionDir returns the local directory of a remote action, downloading it once.
func (j *jobRun) actionDir(ctx context.Context, u string) (string, error) {
	if d, ok := j.actDirs[u]; ok {
		return d, nil
	}
	ref, err := parseUses(u)
	if err != nil {
		return "", err
	}
	if !shaRe.MatchString(strings.ToLower(ref.ref)) {
		return "", fmt.Errorf("%s: actions must be pinned to a full commit SHA (see .atelier/actions.lock)", u)
	}
	j.log.System(-1, fmt.Sprintf("Download action repository '%s/%s@%s'", ref.owner, ref.repo, ref.ref))
	var root string
	if ref.atelier {
		root, err = j.fetchAtelierAction(ctx, ref.owner, ref.repo, strings.ToLower(ref.ref))
	} else {
		root, err = j.r.fetchAction(ctx, ref.owner, ref.repo, strings.ToLower(ref.ref))
	}
	if err != nil {
		return "", fmt.Errorf("download %s: %w", u, err)
	}
	dir := root
	if ref.sub != "" {
		dir = filepath.Join(root, filepath.FromSlash(path.Clean(ref.sub)))
	}
	j.actDirs[u] = dir
	return dir, nil
}

// runUses runs a `uses:` step and returns the action's outputs.
func (j *jobRun) runUses(ctx context.Context, sc *scope, st stepDef, stepEnv envMap, fc *fileCmds, cs *cmdState) (map[string]string, int, error) {
	u, err := j.eval(sc, stepEnv).Interpolate(strings.TrimSpace(st.Uses))
	if err != nil {
		return nil, 1, err
	}
	switch {
	case strings.HasPrefix(u, "docker://"):
		if !j.r.Config.HasCap(actions.CapDocker) {
			return nil, 1, errors.New("docker:// steps need the docker capability on this runner")
		}
		with, err := j.evalWith(sc, st, stepEnv)
		if err != nil {
			return nil, 1, err
		}
		args, err := splitArgs(with["args"])
		if err != nil {
			return nil, 1, err
		}
		code, err := j.dockerRun(ctx, sc, stepEnv, fc, cs, strings.TrimPrefix(u, "docker://"), with["entrypoint"], args, nil, "")
		return nil, code, err
	case isCheckout(u):
		with, err := j.evalWith(sc, st, stepEnv)
		if err != nil {
			return nil, 1, err
		}
		code, err := j.checkout(ctx, sc, with)
		return nil, code, err
	}
	var dir string
	if strings.HasPrefix(u, "./") || strings.HasPrefix(u, ".\\") {
		dir = filepath.Join(j.ws, filepath.FromSlash(u))
		if rel, err := filepath.Rel(j.ws, dir); err != nil || strings.HasPrefix(rel, "..") {
			return nil, 1, fmt.Errorf("local action %s is outside the workspace", u)
		}
	} else if dir, err = j.actionDir(ctx, u); err != nil {
		return nil, 1, err
	}
	act, err := loadActionDef(dir)
	if err != nil {
		return nil, 1, fmt.Errorf("%s: %w", u, err)
	}
	inputs, err := j.actionInputs(sc, st, act)
	if err != nil {
		return nil, 1, err
	}
	switch act.kind() {
	case "node":
		code, err := j.runNodeScript(ctx, sc, st, act, dir, act.Runs.Main, inputs, stepEnv, cs, fc)
		if act.Runs.Post != "" {
			j.mu.Lock()
			j.posts = append(j.posts, postAction{sc: sc, st: st, act: act, dir: dir, inputs: inputs, state: j.stateFrom(cs, fc)})
			j.mu.Unlock()
		}
		return nil, code, err
	case "composite":
		return j.runComposite(ctx, sc, st, act, dir, inputs, stepEnv)
	case "docker":
		if !j.r.Config.HasCap(actions.CapDocker) {
			return nil, 1, errors.New("docker actions need the docker capability on this runner")
		}
		return nil, 1, j.runDockerAction(ctx, sc, st, act, dir, inputs, stepEnv, fc, cs)
	}
	return nil, 1, fmt.Errorf("unsupported action type %q", act.Runs.Using)
}

func (j *jobRun) stateFrom(cs *cmdState, fc *fileCmds) map[string]string {
	st := map[string]string{}
	cs.mu.Lock()
	for k, v := range cs.state {
		st[k] = v
	}
	cs.mu.Unlock()
	if kv, err := parseKVFile(fc.state); err == nil {
		for _, p := range kv {
			st[p[0]] = p[1]
		}
	}
	return st
}

func (j *jobRun) evalWith(sc *scope, st stepDef, stepEnv envMap) (map[string]string, error) {
	out := map[string]string{}
	ev := j.eval(sc, stepEnv)
	for k, v := range st.With {
		r, err := ev.Interpolate(v)
		if err != nil {
			return nil, fmt.Errorf("with.%s: %w", k, err)
		}
		out[strings.ToLower(k)] = r
	}
	return out, nil
}

// actionInputs merges `with:` and action.yml defaults (keys lower-case).
func (j *jobRun) actionInputs(sc *scope, st stepDef, act *actionDef) (map[string]string, error) {
	with, err := j.evalWith(sc, st, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	ev := j.eval(sc, nil)
	for name, in := range act.Inputs {
		k := strings.ToLower(name)
		if v, ok := with[k]; ok {
			out[k] = v
			continue
		}
		v, err := ev.Interpolate(in.Default)
		if err != nil {
			return nil, fmt.Errorf("input %s default: %w", name, err)
		}
		if in.Required && v == "" {
			j.log.System(sc.logStep, fmt.Sprintf("Warning: input required and not supplied: %s", name))
		}
		out[k] = v
	}
	for k, v := range with {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out, nil
}

func inputEnvName(k string) string {
	return "INPUT_" + strings.ToUpper(strings.ReplaceAll(k, " ", "_"))
}

// runNodeScript runs a JavaScript action file (main, pre or post) with node.
func (j *jobRun) runNodeScript(ctx context.Context, sc *scope, st stepDef, act *actionDef, dir, file string, inputs map[string]string, stepEnv envMap, cs *cmdState, fc *fileCmds) (int, error) {
	if stepEnv == nil {
		var err error
		if stepEnv, err = j.stepEnv(sc, st); err != nil {
			return 1, err
		}
	}
	if fc == nil {
		var err error
		if fc, err = j.newFileCmds(); err != nil {
			return 1, err
		}
		defer func() {
			for k, v := range j.applyFileCmds(sc, fc, cs).outputs {
				cs.setOutput(k, v)
			}
			fc.remove()
		}()
	}
	env := j.processEnv(sc, stepEnv, fc, hostPaths, false, dir)
	for k, v := range inputs {
		env.Set(inputEnvName(k), v)
	}
	for k, v := range act.Runs.Env {
		env.Set(k, v)
	}
	if st.ID != "" {
		env.Set("GITHUB_ACTION", st.ID)
	}
	node, err := lookPathIn("node", env.Get(env.pathKey()), runtime.GOOS)
	if err != nil {
		return 1, fmt.Errorf("this is a JavaScript action (%s) and needs Node.js: install node 20 or newer and make sure it is on the runner's PATH", act.Runs.Using)
	}
	script := filepath.Join(dir, filepath.FromSlash(path.Clean("/" + file))[1:])
	if _, err := os.Stat(script); err != nil {
		return 1, fmt.Errorf("action entry point %s not found", file)
	}
	return j.run(ctx, sc.logStep, []string{node, script}, "", j.ws, env, cs)
}

// runPosts runs JavaScript `post` scripts in reverse order.
func (j *jobRun) runPosts(top *scope) {
	j.mu.Lock()
	posts := j.posts
	j.mu.Unlock()
	for i := len(posts) - 1; i >= 0; i-- {
		p := posts[i]
		if errors.Is(j.cause(), errShutdown) {
			return
		}
		cond := p.act.Runs.PostIf
		if cond == "" {
			cond = "always()"
		}
		ok, err := j.eval(top, nil).EvalCondition(cond)
		if err != nil || !ok {
			continue
		}
		j.log.System(p.sc.logStep, "Post "+p.st.displayName())
		ctx := j.ctx
		if j.interrupted() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(context.WithoutCancel(j.ctx), alwaysStepTimeout)
			defer cancel()
		}
		stepEnv, _ := j.stepEnv(p.sc, p.st)
		if stepEnv == nil {
			stepEnv = envMap{}
		}
		for k, v := range p.state {
			stepEnv.Set("STATE_"+k, v)
		}
		code, err := j.runNodeScript(ctx, p.sc, p.st, p.act, p.dir, p.act.Runs.Post, p.inputs, stepEnv, newCmdState(), nil)
		if err != nil || code != 0 {
			j.log.System(p.sc.logStep, fmt.Sprintf("Error: post step failed (exit %d) %v", code, errString(err)))
			top.status = actions.StatusFailure
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// runComposite runs a composite action's steps in their own scope.
func (j *jobRun) runComposite(ctx context.Context, sc *scope, st stepDef, act *actionDef, dir string, inputs map[string]string, stepEnv envMap) (map[string]string, int, error) {
	if sc.depth >= maxCompositeDepth {
		return nil, 1, errors.New("composite actions nested too deeply")
	}
	in := map[string]any{}
	for k, v := range inputs {
		in[k] = v
	}
	parentEnv := map[string]string{}
	for k, v := range stepEnv {
		parentEnv[k] = v
	}
	csc := &scope{steps: map[string]any{}, inputs: in, status: actions.StatusSuccess, logStep: sc.logStep,
		actionPath: dir, parentEnv: parentEnv, depth: sc.depth + 1}
	steps := make([]stepDef, len(act.Runs.Steps))
	for i, s := range act.Runs.Steps {
		steps[i] = stepDef{Index: sc.logStep, ID: s.ID, Name: s.Name, If: s.If, Run: s.Run, Shell: s.Shell, Uses: s.Uses,
			With: s.With, Env: s.Env, WorkingDirectory: s.WorkingDirectory, ContinueOnError: s.ContinueOnError, TimeoutMinutes: s.TimeoutMinutes}
	}
	// the job's cancellation is seen through j.ctx; ctx here bounds this step
	saved := j.ctx
	j.ctx = ctx
	j.runSteps(csc, steps, false)
	j.ctx = saved

	outs := map[string]string{}
	ev := j.eval(csc, nil)
	names := make([]string, 0, len(act.Outputs))
	for k := range act.Outputs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v, err := ev.Interpolate(act.Outputs[k].Value)
		if err != nil {
			return outs, 1, fmt.Errorf("output %s: %w", k, err)
		}
		outs[k] = v
	}
	if csc.status == actions.StatusFailure {
		return outs, 1, nil
	}
	return outs, 0, nil
}

// splitArgs splits a docker `args` string on whitespace, honouring quotes. No
// shell is involved.
func splitArgs(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in := rune(0)
	has := false
	for _, r := range s {
		switch {
		case in != 0:
			if r == in {
				in = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			in, has = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
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
	if in != 0 {
		return nil, fmt.Errorf("unterminated quote in args")
	}
	if has {
		out = append(out, cur.String())
	}
	return out, nil
}
