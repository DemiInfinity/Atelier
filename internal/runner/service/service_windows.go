//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func recoveryActions() []mgr.RecoveryAction {
	actions := make([]mgr.RecoveryAction, windowsRestartCount)
	for i := range actions {
		actions[i] = mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: windowsRestartDelaySeconds * time.Second}
	}
	return actions
}

func windowsInstall(s Spec, w io.Writer) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service: connect to Service Control Manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	cfg := mgr.Config{
		DisplayName:      s.DisplayName,
		Description:      s.Description,
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
	}

	sv, err := m.OpenService(s.Name)
	if err == nil {
		// Already installed: stop it and update the configuration in place.
		fmt.Fprintf(w, "Service %s exists; updating\n", s.Name)
		if err := stopAndWait(sv, windowsStopWaitSeconds*time.Second); err != nil {
			sv.Close()
			return err
		}
		cur, err := sv.Config()
		if err != nil {
			sv.Close()
			return fmt.Errorf("service: read config of %s: %w", s.Name, err)
		}
		cur.DisplayName = cfg.DisplayName
		cur.Description = cfg.Description
		cur.StartType = cfg.StartType
		cur.DelayedAutoStart = cfg.DelayedAutoStart
		cur.BinaryPathName = WindowsCommandLine(s.Exe, s.Args)
		if err := sv.UpdateConfig(cur); err != nil {
			sv.Close()
			return fmt.Errorf("service: update %s: %w", s.Name, err)
		}
	} else {
		sv, err = m.CreateService(s.Name, s.Exe, cfg, s.Args...)
		if err != nil {
			return fmt.Errorf("service: create %s: %w", s.Name, err)
		}
		fmt.Fprintf(w, "Created service %s\n", s.Name)
	}
	defer sv.Close()

	if err := sv.SetRecoveryActions(recoveryActions(), windowsResetPeriodSeconds); err != nil {
		return fmt.Errorf("service: set recovery actions: %w", err)
	}
	if err := sv.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("service: set recovery on non-crash failures: %w", err)
	}
	if err := setServiceEnv(s); err != nil {
		return err
	}
	if err := sv.Start(); err != nil {
		return fmt.Errorf("service: start %s: %w", s.Name, err)
	}
	fmt.Fprintf(w, "Started service %s\n", s.Name)
	return nil
}

// setServiceEnv writes (or clears) the per-service Environment value.
func setServiceEnv(s Spec) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+s.Name, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("service: open registry key for %s: %w", s.Name, err)
	}
	defer k.Close()
	if len(s.Env) == 0 {
		if err := k.DeleteValue("Environment"); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("service: clear environment: %w", err)
		}
		return nil
	}
	if err := k.SetStringsValue("Environment", windowsEnvBlock(s.Env)); err != nil {
		return fmt.Errorf("service: set environment: %w", err)
	}
	return nil
}

func stopAndWait(sv *mgr.Service, timeout time.Duration) error {
	st, err := sv.Query()
	if err != nil {
		return fmt.Errorf("service: query %s: %w", sv.Name, err)
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if _, err := sv.Control(svc.Stop); err != nil {
			return fmt.Errorf("service: stop %s: %w", sv.Name, err)
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		st, err = sv.Query()
		if err != nil {
			return fmt.Errorf("service: query %s: %w", sv.Name, err)
		}
		if st.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service: %s did not stop within %s", sv.Name, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func windowsUninstall(s Spec, w io.Writer) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service: connect to Service Control Manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()
	sv, err := m.OpenService(s.Name)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			fmt.Fprintf(w, "Service %s is not installed\n", s.Name)
			return nil
		}
		return fmt.Errorf("service: open %s: %w", s.Name, err)
	}
	defer sv.Close()
	if err := stopAndWait(sv, windowsStopWaitSeconds*time.Second); err != nil {
		return err
	}
	if err := sv.Delete(); err != nil {
		return fmt.Errorf("service: delete %s: %w", s.Name, err)
	}
	fmt.Fprintf(w, "Deleted service %s\n", s.Name)
	return nil
}

// windowsStatus opens the SCM and service with query-only rights, so it
// works without Administrator and cannot change anything.
func windowsStatus(s Spec) (string, error) {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "", fmt.Errorf("service: connect to Service Control Manager: %w", err)
	}
	defer windows.CloseServiceHandle(h)
	name, err := windows.UTF16PtrFromString(s.Name)
	if err != nil {
		return "", err
	}
	sh, err := windows.OpenService(h, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return "not installed", nil
		}
		return "", fmt.Errorf("service: open %s: %w", s.Name, err)
	}
	sv := &mgr.Service{Name: s.Name, Handle: sh}
	defer sv.Close()
	st, err := sv.Query()
	if err != nil {
		return "", fmt.Errorf("service: query %s: %w", s.Name, err)
	}
	return "installed, " + stateName(st.State), nil
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start pending"
	case svc.StopPending:
		return "stop pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue pending"
	case svc.PausePending:
		return "pause pending"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("state %d", s)
	}
}

// RunAsService runs run under the Windows Service Control Manager when the
// process was started as a Windows service, returning (true, err). Stop and
// Shutdown requests cancel run's context; the service reports StopPending
// until run returns. When not running as a service it returns (false, nil)
// without calling run.
func RunAsService(name string, run func(ctx context.Context) error) (bool, error) {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return false, fmt.Errorf("service: detect Windows service: %w", err)
	}
	if !isSvc {
		return false, nil
	}
	h := &handler{run: run}
	if err := svc.Run(name, h); err != nil {
		return true, err
	}
	return true, h.err
}

type handler struct {
	run func(ctx context.Context) error
	err error
}

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()

	running := svc.Status{State: svc.Running, Accepts: accepts}
	status <- running
	stopping := false
	var checkpoint uint32
	for {
		select {
		case err := <-done:
			if stopping && errors.Is(err, context.Canceled) {
				err = nil
			}
			h.err = err
			if err != nil {
				// Non-zero exit lets the SCM recovery actions restart us.
				return false, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				if stopping {
					checkpoint++
					status <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 90000}
				} else {
					status <- running
				}
			case svc.Stop, svc.Shutdown:
				if !stopping {
					stopping = true
					checkpoint++
					status <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 90000}
					cancel()
				}
			}
		}
	}
}
