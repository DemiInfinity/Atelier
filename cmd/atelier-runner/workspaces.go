package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"atelier/internal/actions"
	"atelier/internal/runner"
)

// atelier-runner workspaces enable|disable|show: device workspaces ("Open on
// <device>" in Atelier). Off until switched on here; the open presets are
// this device's own commands, kept in workspaces.json next to the config.

const workspacesUsage = `usage:
  atelier-runner workspaces enable [--root DIR] [--remote https|ssh] [--preset name=command]...
  atelier-runner workspaces disable
  atelier-runner workspaces show
Restart the runner (or the service) afterwards.`

// workspaceConfigPath is workspaces.json next to the runner config.
func workspaceConfigPath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "workspaces.json")
}

type presetFlags map[string]string

func (p presetFlags) String() string { return "" }
func (p presetFlags) Set(s string) error {
	name, cmd, ok := strings.Cut(s, "=")
	if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(cmd) == "" {
		return errors.New("use --preset name=command, e.g. --preset vscode='code {path}'")
	}
	p[strings.TrimSpace(name)] = strings.TrimSpace(cmd)
	return nil
}

func workspacesCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(workspacesUsage)
	}
	sub := args[0]
	fs := flag.NewFlagSet("workspaces "+sub, flag.ContinueOnError)
	cfgPath := configFlag(fs)
	root := fs.String("root", "", "workspaces folder (default ~/Atelier)")
	remote := fs.String("remote", "", "origin after a clone: https (default) or ssh")
	presets := presetFlags{}
	fs.Var(presets, "preset", "an open preset, name=command ({path} is the workspace folder); repeatable")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := runner.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	wsPath := workspaceConfigPath(*cfgPath)
	wc, err := runner.LoadWorkspaceConfig(wsPath)
	if err != nil {
		return err
	}
	switch sub {
	case "enable":
		if *root != "" {
			wc.Root = *root
		}
		if *remote != "" {
			wc.Remote = *remote
		}
		if wc.Presets == nil {
			wc.Presets = runner.DefaultPresets()
		}
		for k, v := range presets {
			wc.Presets[k] = v
		}
		if err := wc.Validate(); err != nil {
			return err
		}
		if err := wc.Save(wsPath); err != nil {
			return err
		}
		if !slices.Contains(cfg.Capabilities, actions.CapWorkspaces) {
			cfg.Capabilities = append(cfg.Capabilities, actions.CapWorkspaces)
			if err := cfg.Save(*cfgPath); err != nil {
				return err
			}
		}
		fmt.Fprintln(stdout, "Workspaces are on for this device. Restart the runner to pick it up.")
	case "disable":
		cfg.Capabilities = slices.DeleteFunc(cfg.Capabilities, func(c string) bool { return c == actions.CapWorkspaces })
		if err := cfg.Save(*cfgPath); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Workspaces are off for this device. Restart the runner to pick it up.")
		return nil
	case "show":
	default:
		return errors.New(workspacesUsage)
	}
	dir, err := wc.RootDir()
	if err != nil {
		return err
	}
	state := "off"
	if slices.Contains(cfg.Capabilities, actions.CapWorkspaces) {
		state = "on"
	}
	rem := wc.Remote
	if rem == "" {
		rem = "https"
	}
	fmt.Fprintf(stdout, "Workspaces: %s\nFolder: %s\nRemote: %s\nConfig: %s\nOpen presets:\n", state, dir, rem, wsPath)
	for _, n := range wc.PresetNames() {
		fmt.Fprintf(stdout, "  %-10s %s\n", n, wc.Presets[n])
	}
	if len(wc.Presets) == 0 {
		fmt.Fprintln(stdout, "  (none: add one with --preset name=command)")
	}
	return nil
}
