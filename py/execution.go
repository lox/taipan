package py

import (
	"context"
	"errors"
)

// ExecutionLimits defines the runtime safety limits for a Taipan execution.
type ExecutionLimits struct {
	MaxInstructions int
	MaxCallDepth    int
	MaxOutputBytes  int
}

// ExecutionState tracks the mutable runtime state for a single execution.
type ExecutionState struct {
	HostContext      context.Context
	Limits           ExecutionLimits
	instructionCount int
	callDepth        int
	outputBytes      int
}

type executionStateGetter interface {
	ExecutionState() *ExecutionState
}

type executionStateSetter interface {
	SetExecutionState(*ExecutionState)
}

// GetExecutionState returns the optional execution state attached to ctx.
func GetExecutionState(ctx Context) *ExecutionState {
	carrier, ok := ctx.(executionStateGetter)
	if !ok {
		return nil
	}
	return carrier.ExecutionState()
}

// SetExecutionState attaches an execution state to ctx if it supports it.
func SetExecutionState(ctx Context, state *ExecutionState) {
	carrier, ok := ctx.(executionStateSetter)
	if !ok {
		return
	}
	carrier.SetExecutionState(state)
}

// ExceptionFromContextError maps host context cancellation to Python exceptions.
func ExceptionFromContextError(err error) *Exception {
	switch {
	case errors.Is(err, context.Canceled):
		return ExceptionNewf(InterruptedError, "execution interrupted")
	case errors.Is(err, context.DeadlineExceeded):
		return ExceptionNewf(TimeoutError, "execution deadline exceeded")
	default:
		return nil
	}
}

// BeforeOpcode records one executed opcode and enforces the instruction budget.
func (s *ExecutionState) BeforeOpcode() error {
	if s == nil {
		return nil
	}
	if err := s.CheckContext(); err != nil {
		return err
	}
	s.instructionCount++
	if s.Limits.MaxInstructions > 0 && s.instructionCount > s.Limits.MaxInstructions {
		return ExceptionNewf(RuntimeError, "instruction limit exceeded (%d)", s.Limits.MaxInstructions)
	}
	return nil
}

// EnterCall increments call depth and enforces the configured recursion budget.
func (s *ExecutionState) EnterCall() error {
	if s == nil {
		return nil
	}
	if err := s.CheckContext(); err != nil {
		return err
	}
	s.callDepth++
	if s.Limits.MaxCallDepth > 0 && s.callDepth > s.Limits.MaxCallDepth {
		s.callDepth--
		return ExceptionNewf(RuntimeError, "call depth limit exceeded (%d)", s.Limits.MaxCallDepth)
	}
	return nil
}

// ExitCall decrements call depth after a function returns.
func (s *ExecutionState) ExitCall() {
	if s == nil || s.callDepth == 0 {
		return
	}
	s.callDepth--
}

// RestoreCallDepth resets the tracked Python frame depth after a resume.
func (s *ExecutionState) RestoreCallDepth(depth int) {
	if s == nil {
		return
	}
	if depth < 0 {
		depth = 0
	}
	s.callDepth = depth
}

// ReserveOutput records bytes about to be written to stdout/stderr.
func (s *ExecutionState) ReserveOutput(n int) error {
	if s == nil {
		return nil
	}
	if err := s.CheckContext(); err != nil {
		return err
	}
	if s.Limits.MaxOutputBytes > 0 && s.outputBytes+n > s.Limits.MaxOutputBytes {
		return ExceptionNewf(RuntimeError, "output limit exceeded (%d bytes)", s.Limits.MaxOutputBytes)
	}
	s.outputBytes += n
	return nil
}

// CheckContext reports host cancellation as a Python exception.
func (s *ExecutionState) CheckContext() error {
	if s == nil || s.HostContext == nil {
		return nil
	}
	if err := s.HostContext.Err(); err != nil {
		if exc := ExceptionFromContextError(err); exc != nil {
			return exc
		}
		return err
	}
	return nil
}
