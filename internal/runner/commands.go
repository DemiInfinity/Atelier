package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// cmdState collects workflow commands (::set-output:: etc.) from one step.
type cmdState struct {
	mu        sync.Mutex
	outputs   map[string]string
	state     map[string]string
	stopToken string
}

func newCmdState() *cmdState {
	return &cmdState{outputs: map[string]string{}, state: map[string]string{}}
}

func (c *cmdState) setOutput(k, v string) {
	c.mu.Lock()
	c.outputs[k] = v
	c.mu.Unlock()
}

// unescapeData reverses the workflow-command escaping of values.
func unescapeData(s string) string {
	return strings.NewReplacer("%0D", "\r", "%0A", "\n", "%25", "%").Replace(s)
}

func unescapeProp(s string) string {
	return strings.NewReplacer("%0D", "\r", "%0A", "\n", "%3A", ":", "%2C", ",", "%25", "%").Replace(s)
}

// parseCommand parses "::name k=v,k2=v2::value".
func parseCommand(line string) (name string, props map[string]string, value string, ok bool) {
	t := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(t, "::") {
		return "", nil, "", false
	}
	rest := t[2:]
	i := strings.Index(rest, "::")
	if i < 0 {
		return "", nil, "", false
	}
	head, value := rest[:i], rest[i+2:]
	name, propStr, _ := strings.Cut(head, " ")
	if name == "" {
		return "", nil, "", false
	}
	props = map[string]string{}
	for _, p := range strings.Split(propStr, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok {
			props[k] = unescapeProp(v)
		}
	}
	return name, props, unescapeData(value), true
}

// handleLine processes one output line: workflow commands, then logging.
func (j *jobRun) handleLine(step int, stream, line string, cs *cmdState) {
	line = strings.TrimSuffix(line, "\r")
	cs.mu.Lock()
	stop := cs.stopToken
	cs.mu.Unlock()
	if stop != "" {
		if strings.TrimSpace(line) == "::"+stop+"::" {
			cs.mu.Lock()
			cs.stopToken = ""
			cs.mu.Unlock()
		}
		j.log.Line(step, stream, line)
		return
	}
	name, props, value, ok := parseCommand(line)
	if !ok {
		j.log.Line(step, stream, line)
		return
	}
	switch name {
	case "add-mask":
		j.mask.Add(value)
		j.log.Line(step, stream, "::add-mask::***")
		return
	case "set-output":
		cs.mu.Lock()
		cs.outputs[props["name"]] = value
		cs.mu.Unlock()
	case "save-state":
		cs.mu.Lock()
		cs.state[props["name"]] = value
		cs.mu.Unlock()
	case "stop-commands":
		cs.mu.Lock()
		cs.stopToken = value
		cs.mu.Unlock()
	case "set-env", "add-path":
		j.log.System(step, fmt.Sprintf("Warning: the %s command is disabled; use GITHUB_ENV / GITHUB_PATH files", name))
	}
	j.log.Line(step, stream, line)
}

// After a step's main process exits, its output is read until the pipes
// close; if something it started keeps them open, until they have been quiet
// for drainQuiet (at most drainMax). Then they are closed.
var (
	drainQuiet = time.Second
	drainMax   = 10 * time.Second
)

func drainAfterExit(wg *sync.WaitGroup, lastRead *atomic.Int64, closePipes func()) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	lastRead.Store(time.Now().UnixNano()) // give what the process wrote last time to arrive
	limit := time.Now().Add(drainMax)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			if now.After(limit) || now.Sub(time.Unix(0, lastRead.Load())) > drainQuiet {
				closePipes()
				<-done
				return
			}
		}
	}
}

// run starts a process and streams its output through handleLine.
func (j *jobRun) run(ctx context.Context, step int, argv []string, cmdLine, dir string, env envMap, cs *cmdState) (int, error) {
	if len(argv) == 0 {
		return 1, errors.New("empty command")
	}
	exe := argv[0]
	if p, err := lookPathIn(exe, env.Get(env.pathKey()), runtimeGOOS()); err == nil {
		exe = p
	} else {
		return 127, fmt.Errorf("%s: not found on PATH", argv[0])
	}
	cmd := exec.CommandContext(ctx, exe, argv[1:]...)
	cmd.Dir = dir
	if runtimeGOOS() != "windows" {
		env = env.Clone()
		env.Set("PWD", dir)
	}
	cmd.Env = env.Slice()
	cmd.WaitDelay = 10 * time.Second
	setProcGroup(cmd)
	if cmdLine != "" {
		setCmdLine(cmd, cmdLine)
	}
	// Our own pipes rather than StdoutPipe: a program the step leaves running
	// in the background (`server &`) keeps the pipe open, and the step must
	// still end when its main process does (see drainAfterExit).
	rOut, wOut, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		rOut.Close()
		wOut.Close()
		return 1, err
	}
	defer rOut.Close()
	defer rErr.Close()
	cmd.Stdout, cmd.Stderr = wOut, wErr
	err = cmd.Start()
	wOut.Close() // the child has its copies
	wErr.Close()
	if err != nil {
		return 1, err
	}
	var (
		wg       sync.WaitGroup
		lastRead atomic.Int64
	)
	lastRead.Store(time.Now().UnixNano())
	pump := func(r io.Reader, stream string) {
		defer wg.Done()
		br := bufio.NewReaderSize(r, 64*1024)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				lastRead.Store(time.Now().UnixNano())
				j.handleLine(step, stream, strings.TrimSuffix(line, "\n"), cs)
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(rOut, "stdout")
	go pump(rErr, "stderr")
	err = cmd.Wait()
	drainAfterExit(&wg, &lastRead, func() { rOut.Close(); rErr.Close() })
	if runtimeGOOS() != "windows" && groupAlive(cmd.Process.Pid) {
		j.addLeftover(cmd.Process.Pid)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		if ctx.Err() != nil {
			return -1, nil
		}
		return 1, err
	}
	return 0, nil
}

// fileCmds are the per-step GITHUB_OUTPUT/ENV/PATH/STEP_SUMMARY/STATE files.
type fileCmds struct{ output, env, path, summary, state string }

func (j *jobRun) newFileCmds() (*fileCmds, error) {
	j.mu.Lock()
	j.seq++
	n := j.seq
	j.mu.Unlock()
	f := &fileCmds{}
	for _, p := range []struct {
		dst  *string
		name string
	}{{&f.output, "output"}, {&f.env, "env"}, {&f.path, "path"}, {&f.summary, "step_summary"}, {&f.state, "state"}} {
		*p.dst = filepath.Join(j.files, fmt.Sprintf("%s_%d", p.name, n))
		if err := os.WriteFile(*p.dst, nil, 0o666); err != nil {
			return nil, err
		}
		_ = os.Chmod(*p.dst, 0o666) // container users may not be us
	}
	return f, nil
}

func (f *fileCmds) remove() {
	for _, p := range []string{f.output, f.env, f.path, f.summary, f.state} {
		_ = os.Remove(p)
	}
}

// parseKVFile parses name=value and name<<DELIM heredoc entries, in order.
func parseKVFile(path string) ([][2]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	var out [][2]string
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			continue
		}
		eq := strings.Index(l, "=")
		hd := strings.Index(l, "<<")
		if hd > 0 && (eq < 0 || hd < eq) {
			name, delim := l[:hd], l[hd+2:]
			if name == "" || delim == "" {
				return out, fmt.Errorf("invalid line %q", l)
			}
			var val []string
			closed := false
			for i++; i < len(lines); i++ {
				if lines[i] == delim {
					closed = true
					break
				}
				val = append(val, lines[i])
			}
			if !closed {
				return out, fmt.Errorf("matching delimiter %q not found", delim)
			}
			out = append(out, [2]string{name, strings.Join(val, "\n")})
			continue
		}
		if eq <= 0 {
			return out, fmt.Errorf("invalid line %q", l)
		}
		out = append(out, [2]string{l[:eq], l[eq+1:]})
	}
	return out, nil
}

// applyFileCmds reads the step's files after it ran.
func (j *jobRun) applyFileCmds(sc *scope, fc *fileCmds, cs *cmdState) stepOut {
	out := stepOut{outputs: map[string]string{}, state: map[string]string{}}
	cs.mu.Lock()
	for k, v := range cs.outputs {
		out.outputs[k] = v
	}
	for k, v := range cs.state {
		out.state[k] = v
	}
	cs.mu.Unlock()
	if kv, err := parseKVFile(fc.output); err != nil {
		j.log.System(sc.logStep, "Warning: GITHUB_OUTPUT: "+err.Error())
	} else {
		for _, p := range kv {
			out.outputs[p[0]] = p[1]
		}
	}
	if kv, err := parseKVFile(fc.state); err == nil {
		for _, p := range kv {
			out.state[p[0]] = p[1]
		}
	}
	if kv, err := parseKVFile(fc.env); err != nil {
		j.log.System(sc.logStep, "Warning: GITHUB_ENV: "+err.Error())
	} else {
		j.mu.Lock()
		for _, p := range kv {
			if strings.EqualFold(p[0], "NODE_OPTIONS") && strings.Contains(p[1], "--require") {
				// GitHub blocks this too: it would inject code into every later node step
				continue
			}
			j.env.Set(p[0], p[1])
		}
		j.mu.Unlock()
	}
	if b, err := os.ReadFile(fc.path); err == nil {
		j.mu.Lock()
		for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				j.paths = append([]string{l}, j.paths...)
			}
		}
		j.mu.Unlock()
	}
	if b, err := os.ReadFile(fc.summary); err == nil {
		const maxSummary = 1 << 20
		if len(b) > maxSummary {
			b = b[:maxSummary]
		}
		out.summary = string(b)
	}
	return out
}
