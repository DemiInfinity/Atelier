package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Docker is driven through the docker CLI. Values (which may be secrets) are
// never put in argv: env vars are passed as "-e NAME" and the CLI reads the
// value from its own environment.

type jobContainer struct {
	id   string
	path string // PATH inside the container
}

// cliEnvKeys are host variables the docker CLI itself needs; when a container
// also sets one, it is passed explicitly as -e K=V (these are never secrets).
var cliEnvKeys = []string{"PATH", "HOME", "USERPROFILE", "SYSTEMROOT", "TEMP", "TMP", "TMPDIR", "APPDATA", "LOCALAPPDATA",
	"DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"}

func isCLIEnvKey(k string) bool {
	for _, c := range cliEnvKeys {
		if strings.EqualFold(c, k) {
			return true
		}
	}
	return false
}

// dockerEnvArgs returns the -e flags for a container env and the environment
// for the docker CLI process that carries the values.
func dockerEnvArgs(env envMap) ([]string, envMap) {
	proc := osEnv()
	var args []string
	for _, k := range sortedKeys(env) {
		if k == "" || strings.ContainsAny(k, "= ") {
			continue
		}
		if isCLIEnvKey(k) {
			args = append(args, "-e", k+"="+env[k])
			continue
		}
		args = append(args, "-e", k)
		proc.Set(k, env[k])
	}
	return args, proc
}

func (j *jobRun) dockerOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(j.mask.Mask(errb.String())))
	}
	return strings.TrimSpace(out.String()), nil
}

// startContainer starts the job container (container:) with the job
// directory mounted; run steps then use docker exec.
func (j *jobRun) startContainer(sc *scope) (*jobContainer, error) {
	ev := j.eval(sc, nil)
	image, err := ev.Interpolate(j.job.Container.Image)
	if err != nil {
		return nil, err
	}
	cenv := envMap{}
	for _, k := range sortedKeys(j.job.Container.Env) {
		v, err := ev.Interpolate(j.job.Container.Env[k])
		if err != nil {
			return nil, err
		}
		cenv.Set(k, v)
	}
	name := fmt.Sprintf("atelier-job-%d-%s", j.job.ID, randHex(3))
	j.log.System(-1, "Starting job container "+image)
	envArgs, proc := dockerEnvArgs(cenv)
	args := []string{"run", "-d", "--name", name, "--label", containerLabelJobID + "=" + strconv.FormatInt(j.job.ID, 10),
		"-v", j.root + ":" + containerMountRoot, "-v", j.ws + ":" + containerWorkspace, "-w", containerWorkspace}
	args = append(args, envArgs...)
	args = append(args, "--entrypoint", "tail", image, "-f", "/dev/null")
	code, err := j.run(j.ctx, -1, append([]string{"docker"}, args...), "", j.root, proc, newCmdState())
	if err != nil || code != 0 {
		return nil, fmt.Errorf("docker run failed (exit %d) %v", code, errString(err))
	}
	ctr := &jobContainer{id: name}
	ctx, cancel := context.WithTimeout(j.ctx, time.Minute)
	defer cancel()
	if p, err := j.dockerOutput(ctx, "exec", name, "sh", "-c", `printf %s "$PATH"`); err == nil && p != "" {
		ctr.path = p
	} else {
		ctr.path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	return ctr, nil
}

func (c *jobContainer) exec(ctx context.Context, j *jobRun, step int, wd string, env envMap, argv []string, cs *cmdState) (int, error) {
	envArgs, proc := dockerEnvArgs(env)
	args := append([]string{"docker", "exec", "-w", wd}, envArgs...)
	args = append(args, c.id)
	args = append(args, argv...)
	return j.run(ctx, step, args, "", j.root, proc, cs)
}

func (c *jobContainer) stop(j *jobRun) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := j.dockerOutput(ctx, "rm", "-f", c.id); err != nil {
		j.slog.Warn("could not remove job container", "container", c.id, "err", err)
	}
}

// dockerRun runs one container for a docker action or a docker:// step, with
// the workspace and file-command directory mounted.
func (j *jobRun) dockerRun(ctx context.Context, sc *scope, stepEnv envMap, fc *fileCmds, cs *cmdState, image, entrypoint string, args []string, extraEnv map[string]string, actionPath string) (int, error) {
	env := j.processEnv(sc, stepEnv, fc, j.containerPaths, true, "")
	env.Set("HOME", "/github/home")
	for k, v := range extraEnv {
		env.Set(k, v)
	}
	if actionPath != "" {
		env.Set("GITHUB_ACTION_PATH", "")
	}
	envArgs, proc := dockerEnvArgs(env)
	name := fmt.Sprintf("atelier-step-%d-%s", j.job.ID, randHex(3))
	argv := []string{"docker", "run", "--rm", "--name", name, "--label", containerLabelJobID + "=" + strconv.FormatInt(j.job.ID, 10),
		"-v", j.root + ":" + containerMountRoot, "-v", j.ws + ":" + containerWorkspace,
		"-v", filepath.Join(j.root, "home") + ":/github/home", "-w", containerWorkspace}
	argv = append(argv, envArgs...)
	if entrypoint != "" {
		argv = append(argv, "--entrypoint", entrypoint)
	}
	argv = append(argv, image)
	argv = append(argv, args...)
	return j.run(ctx, sc.logStep, argv, "", j.root, proc, cs)
}

// runDockerAction builds (or pulls) a docker action's image and runs it.
func (j *jobRun) runDockerAction(ctx context.Context, sc *scope, st stepDef, act *actionDef, dir string, inputs map[string]string, stepEnv envMap, fc *fileCmds, cs *cmdState) error {
	image := act.Runs.Image
	if strings.HasPrefix(image, "docker://") {
		image = strings.TrimPrefix(image, "docker://")
	} else {
		df := filepath.Join(dir, filepath.FromSlash(image))
		sum := sha256.Sum256([]byte(dir + "|" + image))
		tag := "atelier-action:" + hex.EncodeToString(sum[:8])
		if _, err := j.dockerOutput(ctx, "image", "inspect", tag); err != nil {
			j.log.System(sc.logStep, "Building docker action image")
			_, proc := dockerEnvArgs(envMap{})
			code, err := j.run(ctx, sc.logStep, []string{"docker", "build", "-t", tag, "-f", df, filepath.Dir(df)}, "", dir, proc, newCmdState())
			if err != nil || code != 0 {
				return fmt.Errorf("docker build failed (exit %d) %v", code, errString(err))
			}
		}
		image = tag
	}
	in := map[string]any{}
	extra := map[string]string{}
	for k, v := range inputs {
		in[k] = v
		extra[inputEnvName(k)] = v
	}
	asc := *sc
	asc.inputs = in
	ev := j.eval(&asc, stepEnv)
	for k, v := range act.Runs.Env {
		r, err := ev.Interpolate(v)
		if err != nil {
			return err
		}
		extra[k] = r
	}
	var args []string
	for _, a := range act.Runs.Args {
		r, err := ev.Interpolate(a)
		if err != nil {
			return err
		}
		args = append(args, r)
	}
	code, err := j.dockerRun(ctx, sc, stepEnv, fc, cs, image, act.Runs.Entrypoint, args, extra, dir)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("docker action exited with code %d", code)
	}
	return nil
}
