// Copyright 2026 The Taipan Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vm

import (
	"fmt"

	"github.com/lox/taipan/py"
)

// FrameExitExtCall indicates VM execution paused at an external function
// boundary.
type FrameExitExtCall struct {
	Function     *py.ExtFunction
	Args         py.Tuple
	Kwargs       py.StringDict
	PausedFrames []*py.Frame
}

func newFrameExitExtCall(fn *py.ExtFunction, args py.Tuple, kwargs py.StringDict, frame *py.Frame) *FrameExitExtCall {
	ext := &FrameExitExtCall{
		Function: fn,
		Args:     append(py.Tuple(nil), args...),
	}
	if kwargs != nil {
		kwargsCopy := py.NewStringDictSized(len(kwargs))
		for k, v := range kwargs {
			kwargsCopy[k] = v
		}
		ext.Kwargs = kwargsCopy
	}
	ext.AppendPausedFrame(frame)
	return ext
}

func (e *FrameExitExtCall) AppendPausedFrame(frame *py.Frame) {
	if e == nil || frame == nil {
		return
	}
	if n := len(e.PausedFrames); n > 0 && e.PausedFrames[n-1] == frame {
		return
	}
	e.PausedFrames = append(e.PausedFrames, frame)
}

func (e *FrameExitExtCall) Error() string {
	if e == nil || e.Function == nil {
		return "vm paused on external call"
	}
	return fmt.Sprintf("vm paused on external call %q", e.Function.Name)
}
