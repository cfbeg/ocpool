//go:build !windows

package app

import (
	"os"
	"os/exec"
	"syscall"
)

func secureDirectory(path string) error { return os.Chmod(path, 0700) }

func prepareCommand(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func ownProcess(c *exec.Cmd) (func(), error) {
	return func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }, nil
}
