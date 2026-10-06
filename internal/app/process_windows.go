//go:build windows

package app

import (
	"golang.org/x/sys/windows"
	"os/exec"
	"syscall"
	"unsafe"
)

func prepareCommand(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }
func secureDirectory(path string) error {
	user, e := windows.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		return e
	}
	cmd := exec.Command("icacls", path, "/inheritance:r", "/grant:r", "*"+user.User.Sid.String()+":(OI)(CI)F")
	prepareCommand(cmd)
	return cmd.Run()
}
func ownProcess(c *exec.Cmd) (func(), error) {
	h, e := windows.CreateJobObject(nil, nil)
	if e != nil {
		return nil, e
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, e = windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if e != nil {
		windows.CloseHandle(h)
		return nil, e
	}
	p, e := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(c.Process.Pid))
	if e == nil {
		e = windows.AssignProcessToJobObject(h, p)
		windows.CloseHandle(p)
	}
	if e != nil {
		windows.CloseHandle(h)
		return nil, e
	}
	return func() { _ = windows.CloseHandle(h) }, nil
}
