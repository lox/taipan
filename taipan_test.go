package taipan

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/lox/taipan/py"
)

func TestRunResumeBasicFunctionCall(t *testing.T) {
	prog, err := Compile(`x = echo("hello", answer=42)
record(x)
`, []string{"echo", "record"})
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	progress := Run(context.Background(), prog, nil)

	firstCall, ok := progress.(*FunctionCall)
	if !ok {
		t.Fatalf("expected FunctionCall, got %T", progress)
	}
	if firstCall.Name != "echo" {
		t.Fatalf("expected echo, got %q", firstCall.Name)
	}
	if len(firstCall.Args) != 1 {
		t.Fatalf("expected 1 arg, got %d", len(firstCall.Args))
	}
	requireString(t, firstCall.Args[0], "hello")
	requireInt(t, firstCall.Kwargs["answer"], 42)

	progress = Resume(context.Background(), firstCall.Snapshot, py.String("ok"))

	secondCall, ok := progress.(*FunctionCall)
	if !ok {
		t.Fatalf("expected second FunctionCall, got %T", progress)
	}
	if secondCall.Name != "record" {
		t.Fatalf("expected record, got %q", secondCall.Name)
	}
	if len(secondCall.Args) != 1 {
		t.Fatalf("expected record arg, got %d args", len(secondCall.Args))
	}
	requireString(t, secondCall.Args[0], "ok")

	progress = Resume(context.Background(), secondCall.Snapshot, py.None)
	if _, ok := progress.(*Complete); !ok {
		t.Fatalf("expected Complete, got %T", progress)
	}
}

func TestExternalCallsNestedOrder(t *testing.T) {
	prog, err := Compile(`record(add(get_a(), get_b()))`, []string{"record", "add", "get_a", "get_b"})
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	progress := Run(context.Background(), prog, nil)
	callOrder := make([]string, 0, 4)

	for {
		switch p := progress.(type) {
		case *FunctionCall:
			callOrder = append(callOrder, p.Name)
			switch p.Name {
			case "get_a":
				progress = Resume(context.Background(), p.Snapshot, py.Int(2))
			case "get_b":
				progress = Resume(context.Background(), p.Snapshot, py.Int(3))
			case "add":
				if len(p.Args) != 2 {
					t.Fatalf("add expected 2 args, got %d", len(p.Args))
				}
				requireInt(t, p.Args[0], 2)
				requireInt(t, p.Args[1], 3)
				progress = Resume(context.Background(), p.Snapshot, py.Int(5))
			case "record":
				requireInt(t, p.Args[0], 5)
				progress = Resume(context.Background(), p.Snapshot, py.None)
			default:
				t.Fatalf("unexpected function call %q", p.Name)
			}
		case *Complete:
			want := []string{"get_a", "get_b", "add", "record"}
			if len(callOrder) != len(want) {
				t.Fatalf("unexpected call count: got %d want %d", len(callOrder), len(want))
			}
			for i := range want {
				if callOrder[i] != want[i] {
					t.Fatalf("call %d mismatch: got %q want %q", i, callOrder[i], want[i])
				}
			}
			return
		case *Error:
			t.Fatalf("unexpected python error: %v", p.Exception)
		default:
			t.Fatalf("unexpected progress type %T", progress)
		}
	}
}

func TestExternalCallsInLoop(t *testing.T) {
	prog, err := Compile(`
total = 0
for i in [1, 2, 3]:
    total = add(total, i)
record(total)
`, []string{"add", "record"})
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	progress := Run(context.Background(), prog, nil)

	for {
		switch p := progress.(type) {
		case *FunctionCall:
			switch p.Name {
			case "add":
				if len(p.Args) != 2 {
					t.Fatalf("add expected 2 args, got %d", len(p.Args))
				}
				a := mustInt(t, p.Args[0])
				b := mustInt(t, p.Args[1])
				progress = Resume(context.Background(), p.Snapshot, py.Int(a+b))
			case "record":
				requireInt(t, p.Args[0], 6)
				progress = Resume(context.Background(), p.Snapshot, py.None)
			default:
				t.Fatalf("unexpected function call %q", p.Name)
			}
		case *Complete:
			return
		case *Error:
			t.Fatalf("unexpected python error: %v", p.Exception)
		default:
			t.Fatalf("unexpected progress type %T", progress)
		}
	}
}

func TestExternalCallInComprehension(t *testing.T) {
	prog, err := Compile(`
values = [double(x) for x in [1, 2, 3]]
record(values)
`, []string{"double", "record"})
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	progress := Run(context.Background(), prog, nil)

	for {
		switch p := progress.(type) {
		case *FunctionCall:
			switch p.Name {
			case "double":
				if len(p.Args) != 1 {
					t.Fatalf("double expected 1 arg, got %d", len(p.Args))
				}
				x := mustInt(t, p.Args[0])
				progress = Resume(context.Background(), p.Snapshot, py.Int(2*x))
			case "record":
				if len(p.Args) != 1 {
					t.Fatalf("record expected 1 arg, got %d", len(p.Args))
				}
				list, ok := p.Args[0].(*py.List)
				if !ok {
					t.Fatalf("record arg expected list, got %T", p.Args[0])
				}
				if len(list.Items) != 3 {
					t.Fatalf("list expected 3 items, got %d", len(list.Items))
				}
				requireInt(t, list.Items[0], 2)
				requireInt(t, list.Items[1], 4)
				requireInt(t, list.Items[2], 6)
				progress = Resume(context.Background(), p.Snapshot, py.None)
			default:
				t.Fatalf("unexpected function call %q", p.Name)
			}
		case *Complete:
			return
		case *Error:
			t.Fatalf("unexpected python error: %v", p.Exception)
		default:
			t.Fatalf("unexpected progress type %T", progress)
		}
	}
}

func TestResumeWithErrorAndStringFormattingCall(t *testing.T) {
	prog, err := Compile(`
try:
    value = might_fail()
except RuntimeError:
    value = "fallback"
msg = "value=%s" % format_value(value)
record(msg)
`, []string{"might_fail", "format_value", "record"})
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	progress := Run(context.Background(), prog, nil)

	firstCall, ok := progress.(*FunctionCall)
	if !ok {
		t.Fatalf("expected FunctionCall, got %T", progress)
	}
	if firstCall.Name != "might_fail" {
		t.Fatalf("expected might_fail call, got %q", firstCall.Name)
	}

	progress = ResumeWithError(context.Background(), firstCall.Snapshot, "RuntimeError", "boom")

	formatCall, ok := progress.(*FunctionCall)
	if !ok {
		t.Fatalf("expected format_value call, got %T", progress)
	}
	if formatCall.Name != "format_value" {
		t.Fatalf("expected format_value call, got %q", formatCall.Name)
	}
	requireString(t, formatCall.Args[0], "fallback")

	progress = Resume(context.Background(), formatCall.Snapshot, py.String("fallback!"))

	recordCall, ok := progress.(*FunctionCall)
	if !ok {
		t.Fatalf("expected record call, got %T", progress)
	}
	if recordCall.Name != "record" {
		t.Fatalf("expected record call, got %q", recordCall.Name)
	}
	requireString(t, recordCall.Args[0], "value=fallback!")

	progress = Resume(context.Background(), recordCall.Snapshot, py.None)
	if _, ok := progress.(*Complete); !ok {
		t.Fatalf("expected Complete, got %T", progress)
	}
}

func TestStdoutCapturedOnComplete(t *testing.T) {
	prog, err := Compile(`
print("before")
value = identity()
print("after")
`, []string{"identity"})
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	progress := Run(context.Background(), prog, nil)
	call, ok := progress.(*FunctionCall)
	if !ok {
		t.Fatalf("expected FunctionCall, got %T", progress)
	}
	if call.Name != "identity" {
		t.Fatalf("expected identity call, got %q", call.Name)
	}

	progress = Resume(context.Background(), call.Snapshot, py.Int(1))
	complete, ok := progress.(*Complete)
	if !ok {
		t.Fatalf("expected Complete, got %T", progress)
	}
	if complete.Stdout != "before\nafter\n" {
		t.Fatalf("stdout mismatch: %q", complete.Stdout)
	}
}

func TestStdoutCapturedOnError(t *testing.T) {
	prog, err := Compile(`
print("before")
1 / 0
`, nil)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	progress := Run(context.Background(), prog, nil)
	result, ok := progress.(*Error)
	if !ok {
		t.Fatalf("expected Error, got %T", progress)
	}
	if result.Stdout != "before\n" {
		t.Fatalf("stdout mismatch: %q", result.Stdout)
	}
}

func TestRunDisallowsFileImports(t *testing.T) {
	tempDir := t.TempDir()
	moduleSource := []byte("VALUE = 7\n")
	if err := os.WriteFile(tempDir+"/filemod.py", moduleSource, 0o644); err != nil {
		t.Fatalf("failed to write module: %v", err)
	}

	prog, err := Compile(fmt.Sprintf(`
import sys
sys.path.append(%q)
import filemod
`, tempDir), nil)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	progress := Run(context.Background(), prog, nil)
	errResult, ok := progress.(*Error)
	if !ok {
		t.Fatalf("expected Error, got %T", progress)
	}
	if errResult.Exception.Type != py.ImportError {
		t.Fatalf("expected ImportError, got %v", errResult.Exception.Type)
	}
}

func TestFStringExternalCall(t *testing.T) {
	prog, err := Compile(`
name = lookup()
record(f"hello {name}")
`, []string{"lookup", "record"})
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	progress := Run(context.Background(), prog, nil)
	lookupCall, ok := progress.(*FunctionCall)
	if !ok {
		t.Fatalf("expected FunctionCall, got %T", progress)
	}
	if lookupCall.Name != "lookup" {
		t.Fatalf("expected lookup call, got %q", lookupCall.Name)
	}

	progress = Resume(context.Background(), lookupCall.Snapshot, py.String("world"))
	recordCall, ok := progress.(*FunctionCall)
	if !ok {
		t.Fatalf("expected FunctionCall, got %T", progress)
	}
	if recordCall.Name != "record" {
		t.Fatalf("expected record call, got %q", recordCall.Name)
	}
	requireString(t, recordCall.Args[0], "hello world")

	progress = Resume(context.Background(), recordCall.Snapshot, py.None)
	if _, ok := progress.(*Complete); !ok {
		t.Fatalf("expected Complete, got %T", progress)
	}
}

func mustInt(t *testing.T, obj Object) int {
	t.Helper()
	value, ok := obj.(py.Int)
	if !ok {
		t.Fatalf("expected int object, got %T", obj)
	}
	out, err := value.GoInt()
	if err != nil {
		t.Fatalf("int conversion failed: %v", err)
	}
	return out
}

func requireInt(t *testing.T, obj Object, want int) {
	t.Helper()
	if got := mustInt(t, obj); got != want {
		t.Fatalf("int mismatch: got %d want %d", got, want)
	}
}

func requireString(t *testing.T, obj Object, want string) {
	t.Helper()
	value, ok := obj.(py.String)
	if !ok {
		t.Fatalf("expected string object, got %T", obj)
	}
	if string(value) != want {
		t.Fatalf("string mismatch: got %q want %q", string(value), want)
	}
}
