package taipan

import (
	"context"
	"os"

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
	frames     []*py.Frame
	stdoutFile *os.File
	stdoutPath string
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
	rewrittenSource, err := rewriteFStrings(source)
	if err != nil {
		return nil, err
	}

	code, err := compile.Compile(rewrittenSource, "<taipan>", py.ExecMode, 0, true)
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
	if err := installRestrictedImport(pyCtx); err != nil {
		_ = pyCtx.Close()
		return &Error{Exception: makeExceptionInfo(err)}
	}

	stdoutFile, stdoutPath, err := configureOutputCapture(pyCtx)
	if err != nil {
		_ = pyCtx.Close()
		return &Error{Exception: makeExceptionInfo(err)}
	}

	module, err := pyCtx.Store().NewModule(pyCtx, &py.ModuleImpl{
		Info: py.ModuleInfo{
			Name:     py.MainModuleName,
			FileDesc: "<taipan>",
		},
		Globals: py.NewStringDict(),
	})
	if err != nil {
		_ = stdoutFile.Close()
		_ = os.Remove(stdoutPath)
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

	snap := &Snapshot{
		frames:     []*py.Frame{py.NewFrame(pyCtx, module.Globals, module.Globals, prog.code, nil)},
		stdoutFile: stdoutFile,
		stdoutPath: stdoutPath,
	}
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
		stdout := closeSnapshot(snap)
		return &Error{Exception: makeExceptionInfo(err), Stdout: stdout}
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
			stdout := closeSnapshot(snap)
			return &Error{Exception: makeExceptionInfo(err), Stdout: stdout}
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

		stdout := closeSnapshot(snap)
		return &Error{Exception: makeExceptionInfo(err), Stdout: stdout}
	}

	if result == nil {
		result = py.None
	}
	stdout := closeSnapshot(snap)
	return &Complete{Result: result, Stdout: stdout}
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

func closeSnapshot(snap *Snapshot) string {
	if snap == nil {
		return ""
	}

	stdout := ""
	if snap.stdoutPath != "" {
		if snap.stdoutFile != nil {
			_ = snap.stdoutFile.Close()
			snap.stdoutFile = nil
		}
		if data, err := os.ReadFile(snap.stdoutPath); err == nil {
			stdout = string(data)
		}
		_ = os.Remove(snap.stdoutPath)
		snap.stdoutPath = ""
	}

	if len(snap.frames) == 0 {
		return stdout
	}

	ctx := snap.frames[len(snap.frames)-1].Context
	snap.frames = nil
	if ctx != nil {
		_ = ctx.Close()
	}
	return stdout
}

func configureOutputCapture(ctx py.Context) (*os.File, string, error) {
	sysModule, err := ctx.GetModule("sys")
	if err != nil {
		return nil, "", err
	}

	file, err := os.CreateTemp("", "taipan-stdout-")
	if err != nil {
		return nil, "", err
	}

	stream := &py.File{File: file, FileMode: py.FileWrite}
	sysModule.Globals["stdout"] = stream
	sysModule.Globals["stderr"] = stream
	return file, file.Name(), nil
}

func installRestrictedImport(ctx py.Context) error {
	builtins := ctx.Store().Builtins
	if builtins == nil {
		return py.ExceptionNewf(py.SystemError, "builtins module not loaded")
	}

	importMethod := py.MustNewMethod("__import__", func(self py.Object, args py.Tuple, kwargs py.StringDict) (py.Object, error) {
		kwlist := []string{"name", "globals", "locals", "fromlist", "level"}
		var name py.Object
		var globals py.Object = py.None
		var locals py.Object = py.None
		var fromlist py.Object = py.Tuple{}
		var level py.Object = py.Int(0)

		err := py.ParseTupleAndKeywords(args, kwargs, "U|OOOi:__import__", kwlist, &name, &globals, &locals, &fromlist, &level)
		if err != nil {
			return nil, err
		}

		moduleName := string(name.(py.String))
		if levelObj, ok := level.(py.Int); ok {
			levelInt, err := levelObj.GoInt()
			if err != nil {
				return nil, err
			}
			if levelInt != 0 {
				return nil, py.ExceptionNewf(py.ImportError, "relative imports are disabled")
			}
		}

		if module, err := ctx.GetModule(moduleName); err == nil {
			return module, nil
		}
		if impl := py.GetModuleImpl(moduleName); impl != nil {
			return ctx.ModuleInit(impl)
		}
		return nil, py.ExceptionNewf(py.ImportError, "No module named %q", moduleName)
	}, 0, "")
	importMethod.Module = builtins
	builtins.Globals["__import__"] = importMethod
	return nil
}
