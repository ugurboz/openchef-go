//go:build !darwin && !linux && !windows

package procs

import "os/exec"

type platformState struct{}

func prepare(*exec.Cmd) (*platformState, error) { return nil, ErrUnsupported }

func (*platformState) attach(int) error         { return ErrUnsupported }
func (*platformState) release()                 {}
func (*platformState) supportsTermSignal() bool { return false }
func (*platformState) signalTerm(int) error     { return ErrUnsupported }
func (*platformState) kill(int) error           { return ErrUnsupported }

func waitExitNoReap(int) bool { return false }

// StartToken is not available on this platform.
func StartToken(int) (uint64, error) { return 0, ErrUnsupported }

func probe(int) (uint64, bool, error) { return 0, false, ErrUnsupported }

func killOrphan(int) error { return ErrUnsupported }
