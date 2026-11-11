package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"

	"atelier/internal/actions"
	"atelier/internal/runner"
)

// atelier-runner claude-code enable|disable|show: lets this device run
// agentic workflows with engine: claude-code, using the Claude Code install
// the runner's user is signed in to (so usage counts against that Claude
// subscription). enable checks the install without using the model.

const claudeCodeUsage = `usage:
  atelier-runner claude-code enable [--path /path/to/claude]
  atelier-runner claude-code disable
  atelier-runner claude-code show
Restart the runner (or the service) afterwards.`

func claudeCodeCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(claudeCodeUsage)
	}
	sub := args[0]
	fs := flag.NewFlagSet("claude-code "+sub, flag.ContinueOnError)
	cfgPath := configFlag(fs)
	path := fs.String("path", "", "the claude binary (default: claude on PATH)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := runner.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	switch sub {
	case "enable":
		c, err := runner.CheckClaudeCode(context.Background(), *path)
		if err != nil {
			return fmt.Errorf("Claude Code wasn't switched on: %w", err)
		}
		cfg.ClaudeCodePath = c.Path
		if !slices.Contains(cfg.Capabilities, actions.CapClaudeCode) {
			cfg.Capabilities = append(cfg.Capabilities, actions.CapClaudeCode)
		}
		if err := cfg.Save(*cfgPath); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Found %s at %s.\n", c.Version, c.Path)
		if c.LoggedIn {
			sub := ""
			if c.Subscription != "" {
				sub = " (" + c.Subscription + ")"
			}
			fmt.Fprintf(stdout, "Signed in with %s%s.\n", c.AuthMethod, sub)
		}
		for _, w := range c.Warnings {
			fmt.Fprintln(stdout, "Warning: "+w)
		}
		if !cfg.HasCap(actions.CapJobs) {
			fmt.Fprintln(stdout, "Warning: the jobs capability is off on this runner, so it won't take agent jobs.")
		}
		fmt.Fprintln(stdout, "Claude Code is on for this device: agentic workflows with engine: claude-code can run here. Restart the runner to pick it up.")
		return nil
	case "disable":
		cfg.Capabilities = slices.DeleteFunc(cfg.Capabilities, func(c string) bool { return c == actions.CapClaudeCode })
		cfg.ClaudeCodePath = ""
		if err := cfg.Save(*cfgPath); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Claude Code is off for this device. Restart the runner to pick it up.")
		return nil
	case "show":
		state := "off"
		if cfg.HasCap(actions.CapClaudeCode) {
			state = "on"
		}
		p := cfg.ClaudeCodePath
		if p == "" {
			p = "(claude on PATH)"
		}
		fmt.Fprintf(stdout, "Claude Code: %s\nBinary: %s\n", state, p)
		return nil
	}
	return errors.New(claudeCodeUsage)
}
