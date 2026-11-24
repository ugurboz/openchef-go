package procs

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stillActive is STILL_ACTIVE (STATUS_PENDING), the exit code of a running process.
const stillActive = 259

var procNtResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// platformState owns the Job Object that forms the process's kill domain and a handle to
// the process itself. Holding the process handle keeps its PID reserved until release.
type platformState struct {
	job  windows.Handle
	proc windows.Handle
	once sync.Once
}

// prepare creates a Job Object that kills every member when its last handle closes, and
// arranges for the child to start suspended so it is inside the job before it can spawn
// anything.
func prepare(cmd *exec.Cmd) (*platformState, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("procs: create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), //nolint:gosec // G103: SetInformationJobObject takes a raw pointer to the struct
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job) // error path; original error returned
		return nil, fmt.Errorf("procs: configure job object: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP,
	}
	return &platformState{job: job}, nil
}

// attach puts the suspended child into the job and resumes it.
func (s *platformState) attach(pid int) error {
	const access = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE |
		windows.PROCESS_SUSPEND_RESUME | windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE
	h, err := windows.OpenProcess(access, false, uint32(pid)) //nolint:gosec // PIDs fit in uint32 on Windows
	if err != nil {
		return fmt.Errorf("procs: open pid %d: %w", pid, err)
	}
	s.proc = h
	if err := windows.AssignProcessToJobObject(s.job, h); err != nil {
		return fmt.Errorf("procs: assign pid %d to job: %w", pid, err)
	}
	if status, _, _ := procNtResumeProcess.Call(uintptr(h)); status != 0 {
		return fmt.Errorf("procs: resume pid %d: NTSTATUS 0x%08x", pid, status)
	}
	return nil
}

func (*platformState) supportsTermSignal() bool { return false }

func (*platformState) signalTerm(int) error { return ErrUnsupported }

// kill terminates every process in the job.
func (s *platformState) kill(int) error {
	if s.job == 0 {
		return nil
	}
	if err := windows.TerminateJobObject(s.job, 1); err != nil {
		return fmt.Errorf("procs: terminate job: %w", err)
	}
	return nil
}

// release kills anything left in the job and closes the handles. Idempotent.
func (s *platformState) release() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.job != 0 {
			_ = windows.TerminateJobObject(s.job, 1) // job may already be empty
			_ = windows.CloseHandle(s.job)           // closing our own handle
		}
		if s.proc != 0 {
			_ = windows.CloseHandle(s.proc) // closing our own handle
		}
	})
}

// waitExitNoReap waits on the process handle. The PID stays reserved while the handle
// held by platformState is open.
func waitExitNoReap(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // PIDs fit in uint32
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }() // closing our own handle
	ev, err := windows.WaitForSingleObject(h, windows.INFINITE)
	return err == nil && ev == windows.WAIT_OBJECT_0
}

// StartToken returns the process creation time as a FILETIME (100 ns units since 1601).
func StartToken(pid int) (uint64, error) {
	token, _, found, err := query(pid)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("procs: pid %d not found", pid)
	}
	return token, nil
}

func probe(pid int) (token uint64, running bool, err error) {
	token, running, found, err := query(pid)
	if err != nil || !found {
		return 0, false, err
	}
	return token, running, nil
}

func query(pid int) (token uint64, running, found bool, err error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // PIDs fit in uint32
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, fmt.Errorf("procs: open pid %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }() // closing our own handle

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, false, false, fmt.Errorf("procs: process times of pid %d: %w", pid, err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return 0, false, false, fmt.Errorf("procs: exit code of pid %d: %w", pid, err)
	}
	token = uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	return token, code == stillActive, true, nil
}

// killOrphan terminates a process left by a previous daemon. Its job was destroyed with
// that daemon (KILL_ON_JOB_CLOSE), so normally nothing is left; this covers processes
// that escaped the job.
func killOrphan(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid)) //nolint:gosec // PIDs fit in uint32
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("procs: open pid %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }() // closing our own handle
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("procs: terminate pid %d: %w", pid, err)
	}
	return nil
}
