package taipan

import (
	"context"

	"github.com/lox/taipan/compile"
	"github.com/lox/taipan/py"
	_ "github.com/lox/taipan/stdlib"
	"github.com/lox/taipan/vm"
)

// Object is the public object type used by Taipan programs.
type Object = py.Object

// Program is a parsed and compiled Python program ready for execution.
type Program struct {
	code              *py.Code
	externalFunctions map[string]struct{}
}

// Snapshot is an opaque resumable VM state.
type Snapshot struct {
	frames []*py.Frame
}

// RunProgress is the result of starting or resuming execution.
type RunProgress interface {
	runProgress()
}

// FunctionCall means execution paused at an external function boundary.
type FunctionCall struct {
	Name     string
	Args     []Object
	Kwargs   map[string]Object
	Snapshot *Snapshot
}

func (*FunctionCall) runProgress() {}

// Complete means execution finished normally.
type Complete struct {
	Result Object
	Stdout string
}

func (*Complete) runProgress() {}

// Error means execution failed with an unhandled exception.
type Error struct {
	Exception py.ExceptionInfo
	Stdout    string
}

func (*Error) runProgress() {}

// Compile parses and compiles Python source code.
func Compile(source string, externalFunctions []string) (*Program, error) {
	code, err := compile.Compile(source, "<taipan>", py.ExecMode, 0, true)
	if err != nil {
		return nil, err
	}

	extNames := make(map[string]struct{}, len(externalFunctions))
	for _, name := range externalFunctions {
		if name == "" {
			continue
		}
		extNames[name] = struct{}{}
	}

	return &Program{code: code, externalFunctions: extNames}, nil
}

// Run starts execution of the program with optional global inputs.
func Run(ctx context.Context, prog *Program, inputs map[string]Object) RunProgress {
	if prog == nil || prog.code == nil {
		return &Error{Exception: makeExceptionInfo(py.ExceptionNewf(py.ValueError, "program is nil"))}
	}

	pyCtx := py.NewContext(py.DefaultContextOpts())

	module, err := pyCtx.Store().NewModule(pyCtx, &py.ModuleImpl{
		Info: py.ModuleInfo{
			Name:     py.MainModuleName,
			FileDesc: "<taipan>",
		},
		Globals: py.NewStringDict(),
	})
	if err != nil {
		_ = pyCtx.Close()
		return &Error{Exception: makeExceptionInfo(err)}
	}

	for name, value := range inputs {
		if value == nil {
			value = py.None
		}
		module.Globals[name] = value
	}

	for name := range prog.externalFunctions {
		if _, exists := module.Globals[name]; exists {
			_ = pyCtx.Close()
			return &Error{Exception: makeExceptionInfo(py.ExceptionNewf(py.ValueError, "global %q conflicts with external function", name))}
		}
		module.Globals[name] = py.NewExtFunction(name)
	}

	snap := &Snapshot{frames: []*py.Frame{py.NewFrame(pyCtx, module.Globals, module.Globals, prog.code, nil)}}
	return runSnapshot(ctx, snap, nil, false, nil)
}

// Resume continues execution after a FunctionCall by supplying the function result.
func Resume(ctx context.Context, snap *Snapshot, result Object) RunProgress {
	if snap == nil || len(snap.frames) == 0 {
		return &Error{Exception: makeExceptionInfo(py.ExceptionNewf(py.ValueError, "snapshot is nil"))}
	}
	if result == nil {
		result = py.None
	}
	return runSnapshot(ctx, snap, result, true, nil)
}

// ResumeWithError continues execution and injects an exception at the paused call site.
func ResumeWithError(ctx context.Context, snap *Snapshot, excType string, message string) RunProgress {
	if snap == nil || len(snap.frames) == 0 {
		return &Error{Exception: makeExceptionInfo(py.ExceptionNewf(py.ValueError, "snapshot is nil"))}
	}
	frame := snap.frames[0]

	typeObj := py.RuntimeError
	if excType != "" {
		if candidate, ok := frame.Builtins[excType]; ok {
			if candidateType, ok := candidate.(*py.Type); ok {
				typeObj = candidateType
			}
		}
	}

	exc := py.ExceptionNewf(typeObj, "%s", message)
	excInfo := py.ExceptionInfo{Type: typeObj, Value: exc}
	return runSnapshot(ctx, snap, nil, false, &excInfo)
}

func runSnapshot(ctx context.Context, snap *Snapshot, firstResult Object, pushFirstResult bool, injected *py.ExceptionInfo) RunProgress {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		closeSnapshot(snap)
		return &Error{Exception: makeExceptionInfo(err)}
	}
	if snap == nil || len(snap.frames) == 0 {
		return &Error{Exception: makeExceptionInfo(py.ExceptionNewf(py.ValueError, "snapshot is nil"))}
	}

	var (
		result Object
		err    error
	)

	for i, frame := range snap.frames {
		if err := ctx.Err(); err != nil {
			closeSnapshot(snap)
			return &Error{Exception: makeExceptionInfo(err)}
		}

		switch {
		case i == 0 && injected != nil:
			result, err = vm.RunFrameWithException(frame, *injected)
		case i == 0 && pushFirstResult:
			if firstResult == nil {
				firstResult = py.None
			}
			frame.Stack = append(frame.Stack, firstResult)
			result, err = vm.RunFrame(frame)
		case i == 0:
			result, err = vm.RunFrame(frame)
		default:
			if result == nil {
				result = py.None
			}
			frame.Stack = append(frame.Stack, result)
			result, err = vm.RunFrame(frame)
		}

		if err == nil {
			continue
		}

		if extCall, ok := err.(*vm.FrameExitExtCall); ok {
			snap.frames = mergePausedFrames(extCall, snap.frames, i)
			return &FunctionCall{
				Name:     extCall.Function.Name,
				Args:     tupleToArgs(extCall.Args),
				Kwargs:   kwargsToMap(extCall.Kwargs),
				Snapshot: snap,
			}
		}

		closeSnapshot(snap)
		return &Error{Exception: makeExceptionInfo(err)}
	}

	if result == nil {
		result = py.None
	}
	closeSnapshot(snap)
	return &Complete{Result: result}
}

func mergePausedFrames(extCall *vm.FrameExitExtCall, frames []*py.Frame, frameIndex int) []*py.Frame {
	if extCall == nil {
		return nil
	}

	paused := make([]*py.Frame, 0, len(extCall.PausedFrames)+len(frames))
	paused = append(paused, extCall.PausedFrames...)
	if len(paused) == 0 && frameIndex < len(frames) {
		paused = append(paused, frames[frameIndex])
	}
	if frameIndex+1 < len(frames) {
		paused = append(paused, frames[frameIndex+1:]...)
	}
	return paused
}

func tupleToArgs(args py.Tuple) []Object {
	if len(args) == 0 {
		return nil
	}
	out := make([]Object, len(args))
	for i := range args {
		out[i] = args[i]
	}
	return out
}

func kwargsToMap(kwargs py.StringDict) map[string]Object {
	if len(kwargs) == 0 {
		return nil
	}
	out := make(map[string]Object, len(kwargs))
	for k, v := range kwargs {
		out[k] = v
	}
	return out
}

func makeExceptionInfo(err error) py.ExceptionInfo {
	switch e := err.(type) {
	case py.ExceptionInfo:
		return e
	case *py.Exception:
		info := py.ExceptionInfo{Type: e.Type(), Value: e}
		if tb, ok := e.Traceback.(*py.Traceback); ok {
			info.Traceback = tb
		}
		return info
	default:
		exc := py.MakeException(err)
		return py.ExceptionInfo{Type: exc.Type(), Value: exc}
	}
}

func closeSnapshot(snap *Snapshot) {
	if snap == nil || len(snap.frames) == 0 {
		return
	}
	ctx := snap.frames[len(snap.frames)-1].Context
	snap.frames = nil
	if ctx != nil {
		_ = ctx.Close()
	}
}
