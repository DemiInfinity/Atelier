//go:build !windows

package service

import (
	"context"
	"errors"
	"io"
)

var errNotWindows = errors.New("service: Windows service management is only available on windows")

func windowsInstall(Spec, io.Writer) error   { return errNotWindows }
func windowsUninstall(Spec, io.Writer) error { return errNotWindows }
func windowsStatus(Spec) (string, error)     { return "", errNotWindows }

// RunAsService runs run under the Windows Service Control Manager when the
// process was started as a Windows service. On other OSes it returns
// (false, nil) immediately without calling run.
func RunAsService(name string, run func(ctx context.Context) error) (bool, error) {
	return false, nil
}
