# Taipan — A Minimal Python Interpreter in Go for AI Agents

## Overview

Taipan is a Go port of [pydantic/monty](https://github.com/pydantic/monty) — a minimal, secure Python interpreter purpose-built for running LLM-generated code inside AI agents. Instead of making sequential JSON tool calls, an LLM writes Python that calls tools as regular functions. Taipan interprets the code, **pauses at each external function call**, lets the host execute it, and resumes.

### Why

JSON tool calling is the industry standard but has real limitations:

- **No control flow** — the LLM can't branch on a tool result without another round-trip to the model
- **No composition** — calling tool B with the output of tool A requires the model to see A's result first
- **No data manipulation** — simple transforms (filtering a list, formatting a string) require a tool or another model call

With Taipan, an LLM emits something like:

```python
results = search_files("*.go", pattern="TODO")
for file, matches in results.items():
    if len(matches) > 5:
        summary = summarise(file, matches)
        create_issue(title=f"Tech debt: {file}", body=summary)
```

The interpreter runs this, pausing at `search_files`, `summarise`, and `create_issue` — each resolved by the host (agent-harness) — then resumes. One model call, multiple tool invocations with branching logic.

### Target consumers

- **[agent-harness](https://github.com/lox/agent-harness)** — the LLM returns Python code instead of (or alongside) JSON tool calls. The harness runs it in Taipan, executing tool calls as they're yielded.
- **[gopherbox](https://github.com/buildkite/gopherbox)** — Taipan could be exposed as a `python` command within the sandbox, or Taipan could use gopherbox's VFS for sandboxed file operations.

---

## Architecture

```
taipan/
├── py/              # Value types: Object interface, Int, Float, String, List, Dict, etc.
├── parser/          # Lexer + parser → AST (lifted from gpython, extended)
├── ast/             # AST node definitions
├── symtable/        # Scope analysis (local/global/cell/free variables)
├── compiler/        # AST → bytecode
├── vm/              # Bytecode VM with external-call yielding
├── stdlib/          # Minimal stdlib: builtins, sys, typing
├── taipan.go        # Public API: Run, Snapshot, RunProgress
└── taipan_test.go   # Test suite
```

### Core execution flow

```
Python source string
  → parser.Parse()           → ast.Module
  → symtable.Analyse()       → SymTable
  → compiler.Compile()       → Code (bytecode + constants + metadata)
  → vm.Run(code, inputs, ext_fns)
      → executes bytecode
      → hits ext function call → returns RunProgress.FunctionCall{name, args, state}
      → host executes the function
      → vm.Resume(state, result) → continues executing
      → ... repeat ...
      → returns RunProgress.Complete{result}
```

---

## What to lift from gpython

[go-python/gpython](https://github.com/go-python/gpython) provides a complete Python 3.4 implementation in Go. We'll use it as our starting point, not as a dependency — fork the relevant packages and trim them down.

### Take as-is (with cleanup)

| Package | What | Why |
|---------|------|-----|
| `parser/` | Lexer (`lexer.go`) + yacc grammar (`grammar.y`) → AST | Complete, tested Python 3 parser. The yacc grammar is faithful to CPython 3.4. |
| `ast/` | AST node type definitions | Generated from Python.asdl, comprehensive. |
| `symtable/` | Scope/closure analysis | Correct handling of local/global/cell/free variables. Required for closures. |
| `compile/` | AST → bytecode compiler | Single-pass, handles all 3.4 constructs. Label resolution, stack depth calculation. |
| `py/` (partial) | Core value types: `Int`, `BigInt`, `Float`, `String`, `Bytes`, `Bool`, `None`, `Tuple`, `List`, `Dict`, `Set`, `Slice`, `Range` | Well-structured Go types with Python dunder method interfaces. |
| `py/code.go` | `Code` struct (bytecode + metadata) | Standard CPython code object layout. |
| `py/frame.go` | `Frame` struct (locals, stack, blocks) | Standard frame model. |
| `py/function.go` | `Function` and `Method` types | Needed for closures, generators, user-defined functions. |
| `py/exception.go` | Exception hierarchy | Full Python exception type tree. |
| `vm/` | Eval loop with jump-table dispatch | 80 opcodes, correct block-stack unwinding, generator support. |

### Take but modify

| Component | Changes needed |
|-----------|---------------|
| `vm/eval.go` | Add `ExtFunction` call type → yield `RunProgress.FunctionCall` instead of calling. Add resource limit checks (instruction count, time, memory). |
| `py/` types | Replace `StringDict` with a proper `Dict` supporting arbitrary hashable keys (gpython's biggest known limitation). Add `FrozenSet`. |
| `parser/` | Extend for Python 3.10+ syntax we need (f-string improvements, walrus operator, possibly match/case). See "Modern Python" section below. |

### Don't take

| Component | Why |
|-----------|-----|
| `importlib/` | We don't support file imports. Modules are built-in only. |
| `repl/` | No interactive mode needed. |
| `stdlib/` (most) | We only need `builtins`, `sys`, `typing`. No `os`, `math`, `time`, `glob`, etc. |
| `py/module.go` | Simplify — we don't need the full module registry system. |
| `examples/` | Not relevant. |

---

## The external function call mechanism

This is the key feature that makes Taipan useful for agents. The design follows pydantic/monty's `RunProgress` pattern.

### Public API

```go
package taipan

// Program is a parsed and compiled Python program, ready to execute.
// It is safe to reuse across multiple Run() calls.
type Program struct { /* compiled code, interned strings, ext function IDs */ }

// Compile parses and compiles Python source code.
// externalFunctions are names that will be available as callable globals.
func Compile(source string, externalFunctions []string) (*Program, error)

// RunProgress is the result of starting or resuming execution.
// The caller must type-switch on it.
type RunProgress interface{ runProgress() }

// FunctionCall means the VM has paused because Python code called an external function.
type FunctionCall struct {
    Name     string            // function name (e.g. "search_files")
    Args     []Object          // positional arguments
    Kwargs   map[string]Object // keyword arguments
    Snapshot *Snapshot         // opaque VM state — pass to Resume()
}

// Complete means execution finished normally.
type Complete struct {
    Result Object   // return value (None if no explicit return)
    Stdout string   // captured print() output
}

// Error means execution failed with an unhandled exception.
type Error struct {
    Exception MontyException
    Stdout    string
}

// Run starts executing a compiled program with the given inputs.
// Inputs are injected as globals (e.g. {"data": StringObject("hello")}).
func Run(ctx context.Context, prog *Program, inputs map[string]Object) RunProgress

// Resume continues execution after a FunctionCall, providing the return value.
func Resume(ctx context.Context, snap *Snapshot, result Object) RunProgress

// ResumeWithError continues execution, injecting an exception into the VM.
func ResumeWithError(ctx context.Context, snap *Snapshot, excType string, message string) RunProgress
```

### Integration with agent-harness

```go
// In an agent-harness tool executor:
func executePythonTool(ctx context.Context, code string, toolMap map[string]harness.Tool) (*harness.ToolResult, error) {
    prog, err := taipan.Compile(code, toolNames(toolMap))
    if err != nil {
        return &harness.ToolResult{Content: err.Error(), IsError: true}, nil
    }

    var stdout strings.Builder
    progress := taipan.Run(ctx, prog, nil)

    for {
        switch p := progress.(type) {
        case *taipan.FunctionCall:
            // Execute the tool via agent-harness
            result, err := toolMap[p.Name].Execute(ctx, toToolCall(p))
            if err != nil {
                progress = taipan.ResumeWithError(ctx, p.Snapshot, "RuntimeError", err.Error())
            } else {
                progress = taipan.Resume(ctx, p.Snapshot, fromToolResult(result))
            }

        case *taipan.Complete:
            return &harness.ToolResult{
                Content: p.Stdout + formatResult(p.Result),
            }, nil

        case *taipan.Error:
            return &harness.ToolResult{
                Content: p.Exception.Traceback(),
                IsError: true,
            }, nil
        }
    }
}
```

### VM implementation

In the VM eval loop, when `CALL_FUNCTION` dispatches to a callable and finds it's an `ExtFunction`:

```go
// vm/eval.go — inside the call dispatch
case *py.ExtFunction:
    // Don't call — yield to host
    return &frameExitExtCall{
        extID:  fn.ID,
        args:   positionalArgs,
        kwargs: keywordArgs,
    }
```

The outer `Run()`/`Resume()` function catches this exit, packages the VM state into a `Snapshot`, and returns `FunctionCall` to the caller.

On resume, the snapshot restores the VM exactly where it stopped, pushes the return value onto the operand stack, and continues executing.

---

## Resource limits

Following gopherbox's approach and Monty's `ResourceTracker`:

```go
type Limits struct {
    MaxDuration       time.Duration // Wall clock timeout. Default: 30s.
    MaxInstructions   int           // Bytecode ops executed. Default: 1_000_000.
    MaxCallDepth      int           // Function call recursion. Default: 100.
    MaxAllocations    int           // Heap objects created. Default: 100_000.
    MaxOutputBytes    int           // print() output. Default: 1MB.
}
```

Checked in the VM hot loop:
- **Instructions**: increment counter per opcode; check every 256 ops.
- **Duration**: `context.Context` deadline; checked every 256 ops alongside instruction count.
- **Call depth**: checked on `CALL_FUNCTION`.
- **Allocations**: checked on `BUILD_LIST`, `BUILD_DICT`, string concat, etc.
- **Output**: checked in the `print()` builtin.

Pre-flight guards (from Monty) to prevent DoS before allocation:
- `2 ** 10_000_000` — check exponent size before computing
- `"x" * 10_000_000` — check repeat count before allocating
- `[0] * 10_000_000` — check list repeat size

---

## Python subset: what we support

The goal is "the Python that LLMs actually generate." Based on analysis of pydantic/monty's 250+ test cases and real agent tool-calling patterns:

### Phase 1 — Core (MVP)

Everything an LLM needs for basic tool orchestration:

| Feature | Status | Notes |
|---------|--------|-------|
| Variables, assignment, augmented assignment | From gpython | `a = 1`, `a += 1`, `a, b = b, a` |
| Arithmetic, comparison, boolean operators | From gpython | `+`, `-`, `*`, `/`, `//`, `%`, `**`, `==`, `!=`, `<`, `>`, `and`, `or`, `not`, `in` |
| `if` / `elif` / `else` | From gpython | Including ternary `x if cond else y` |
| `for` / `while` loops | From gpython | Including `break`, `continue`, `for-else` |
| `def` functions | From gpython | Positional, keyword, `*args`, `**kwargs`, defaults, closures |
| `lambda` | From gpython | |
| `return` | From gpython | |
| `try` / `except` / `else` / `finally` | From gpython | Catching host-thrown exceptions |
| `raise` | From gpython | |
| Built-in types | From gpython | `int`, `float`, `str`, `bytes`, `bool`, `None`, `list`, `tuple`, `dict`, `set` |
| Type methods | From gpython | `str.split()`, `str.join()`, `list.append()`, `dict.get()`, `dict.items()`, etc. |
| List/dict/set comprehensions | From gpython | Including nested `for`, `if` filters |
| Built-in functions | From gpython | `len`, `range`, `enumerate`, `zip`, `map`, `filter`, `sorted`, `reversed`, `sum`, `min`, `max`, `any`, `all`, `abs`, `round`, `int`, `float`, `str`, `bool`, `list`, `tuple`, `dict`, `set`, `print`, `repr`, `type`, `isinstance`, `hasattr`, `getattr`, `iter`, `next`, `chr`, `ord`, `hex`, `bin`, `oct`, `hash`, `id`, `pow`, `divmod` |
| String formatting | From gpython | `f"hello {name}"`, `f"{x:.2f}"`, `"{}".format(x)` |
| Tuple unpacking | From gpython | `a, b = func()`, `for k, v in d.items()` |
| `assert` | From gpython | |
| `del` | From gpython | |
| `pass` | From gpython | |
| External function calls | **New** | The core feature — VM yields at ext call boundary |
| Resource limits | **New** | Instruction count, time, memory, output |
| `print()` capture | **New** | Stdout captured to result, not written to os.Stdout |

### Phase 2 — Comfortable

Features that make agent-generated code more natural:

| Feature | Status | Notes |
|---------|--------|-------|
| Walrus operator `:=` | Parser extension | `if (m := re.match(...)):` — LLMs occasionally generate this |
| `match` / `case` | Parser extension | Structural pattern matching (Python 3.10). Low priority — LLMs rarely use it. |
| Type annotations | Ignore | `def foo(x: int) -> str:` — parse and discard. LLMs add these frequently. |
| `@dataclass` | **New** | Define simple data containers. Common in agent code. |
| `class` (basic) | From gpython | Simple classes with `__init__`, methods, attributes. No metaclasses, no MRO complexity. |
| Named tuples | **New** | `from collections import namedtuple` or `typing.NamedTuple` |
| Generator functions | From gpython | `yield` / `yield from` — already in gpython |
| Slicing | From gpython | `items[1:3]`, `s[::-1]` |
| `with` statement | From gpython | Context managers (useful for agent patterns) |
| `*` unpacking in calls/literals | Parser extension | `[*a, *b]`, `{**a, **b}` — LLMs use this |
| `global` / `nonlocal` | From gpython | |

### Phase 3 — Async

For parallel tool execution (multiple tool calls in flight):

| Feature | Notes |
|---------|-------|
| `async def` / `await` | VM supports coroutine suspension |
| `asyncio.gather()` | Parallel external calls — host resolves multiple futures |
| Async external functions | `result = await search(query)` — VM yields, host can batch |

### Phase 4 — Snapshot serialisation

For durable execution (suspend to database, resume later):

| Feature | Notes |
|---------|-------|
| `Snapshot.Marshal()` / `Unmarshal()` | Serialise full VM state to `[]byte` |
| `Program.Marshal()` / `Unmarshal()` | Cache compiled bytecode |

### Explicitly out of scope

| Feature | Why |
|---------|-----|
| `import` (file-based) | No filesystem module loading. Built-in modules only. |
| Full stdlib | No `os`, `sys.path`, `json`, `re`, `math`, `datetime`, `collections`, etc. |
| Third-party packages | No `requests`, `pandas`, `pydantic`, etc. |
| Class inheritance / MRO | Too complex for the agent use case. Basic single-class definitions only. |
| Metaclasses | Not needed. |
| Descriptors / properties | Not needed for agent code. |
| Decorators (general) | Only `@dataclass`. General decorators add complexity for minimal agent value. |
| `exec()` / `eval()` | Security risk — no dynamic code execution within the sandbox. |
| Threading / multiprocessing | Not applicable. |
| File I/O | No `open()`. If needed, expose via external functions or gopherbox VFS. |
| `__dunder__` overriding | User-defined `__add__`, `__getattr__`, etc. Not needed. |

---

## Modern Python parser changes

gpython targets Python 3.4. LLMs generate Python 3.8–3.12 syntax. The delta is manageable:

### Must have (Phase 1)

| Syntax | Python version | Parser change | Compiler change |
|--------|---------------|---------------|-----------------|
| f-strings `f"..."` | 3.6 | gpython has partial support — needs expression parsing inside `{}` | Compile to `BUILD_STRING` or format call |
| Type annotations | 3.0+ | Already parsed by gpython (3.4 had annotations). Need to add `x: int = 1` variable annotations (3.6). | Discard — don't evaluate annotation expressions |
| `*` in literals | 3.5 | `[*a, *b]`, `{**a, **b}` — extend `BUILD_LIST`/`BUILD_MAP` productions | `LIST_EXTEND` / `DICT_MERGE` opcodes |

### Nice to have (Phase 2)

| Syntax | Python version | Parser change |
|--------|---------------|---------------|
| Walrus `:=` | 3.8 | New `NamedExpr` AST node, grammar production in `test` rule |
| Positional-only params `/` | 3.8 | Extend `typedargslist` grammar production |
| `match`/`case` | 3.10 | Significant grammar addition — new statement + pattern nodes. Defer. |

### Can skip

| Syntax | Why |
|--------|-----|
| `async for` / `async with` | LLMs rarely generate these |
| Exception groups `except*` | Python 3.11, very rare in agent code |
| `type` statement (3.12) | Type alias syntax, LLMs don't use it |

---

## gpython Dict limitation fix

gpython's `Dict` is `map[string]Object` (`StringDict`). This breaks:
- `d = {1: "a", 2: "b"}` — integer keys
- `d = {(1, 2): "a"}` — tuple keys
- `d = {True: "a", False: "b"}` — bool keys

Fix: replace `StringDict` with a proper `Dict` backed by an ordered map with hash-based lookup. Use the existing `Object.M__hash__()` and `M__eq__()` interfaces.

```go
type Dict struct {
    entries []dictEntry       // insertion-ordered
    index   map[uint64][]int  // hash → entry indices
}

type dictEntry struct {
    Key   Object
    Value Object
    Hash  uint64
}
```

This also gives us insertion-ordered dict semantics (Python 3.7+ guarantee) for free.

---

## Implementation plan

### Milestone 1: Fork and trim gpython (1 week)

1. Copy `parser/`, `ast/`, `symtable/`, `compile/`, `vm/`, `py/` into taipan
2. Remove all `import`/module infrastructure
3. Remove `repl/`, `examples/`, CLI
4. Strip stdlib to just `builtins` (the built-in functions)
5. Get `go build` and existing gpython tests passing
6. Relicense check (gpython is BSD-3)

Status (Mar 2026): **Implemented**.

- Core packages have been forked into this repo (`ast`, `compile`, `parser`, `py`, `symtable`, `vm`).
- CLI/repl/examples/importlib code is not included in Taipan.
- Stdlib is trimmed to a minimal runtime (`builtins` + `sys`; `sys` retained for `print()`/`input()` compatibility).
- Legacy `.pyc`/marshal module loading paths are disabled.
- `go build ./...` and `go test ./...` pass.
- Upstream BSD-3 licensing is included (`LICENSE`) with attribution (`THIRD_PARTY_NOTICES.md`).

### Milestone 2: External function calls (1 week)

1. Add `ExtFunction` value type to `py/`
2. Add `FunctionCall` / `Complete` / `Error` progress types
3. Modify VM eval loop to yield on `ExtFunction` call
4. Implement `Snapshot` (capture/restore VM state)
5. Implement `Run()` / `Resume()` / `ResumeWithError()` public API
6. Test: simple ext call, nested ext calls, ext call in loop, ext call in try/except, ext call in comprehension, ext call in f-string

Status (Mar 2026): **Implemented**.

- Added `py.ExtFunction` and VM pause semantics for external call boundaries.
- Added public execution API and progress model in `taipan.go` (`Compile`, `Run`, `Resume`, `ResumeWithError`, `FunctionCall`, `Complete`, `Error`, `Snapshot`).
- Added regression tests for simple calls, nested call ordering, loop calls, calls in comprehensions, and `ResumeWithError` try/except behaviour.
- Added stdout capture for `Complete.Stdout` / `Error.Stdout` and import sandboxing to built-in modules for Taipan `Run()`.
- Added support for a practical subset of f-strings in Taipan `Compile()` (expression interpolation used in agent tool-calling flows).
- Open bugs and limitations are tracked in `TODO.md`.

### Milestone 3: Resource limits (3 days)

1. Add `Limits` struct and `context.Context` integration
2. Instruction counting in VM hot loop
3. Call depth checking
4. Allocation counting
5. Output size limiting
6. Pre-flight DoS guards (power, repeat, shift size checks)
7. Test: each limit triggers correctly, limits don't affect normal execution

### Milestone 4: Fix Dict + polish types (3 days)

1. Replace `StringDict` with ordered hash map `Dict`
2. Add `FrozenSet` type
3. Ensure all type methods work: `str.split/join/strip/replace/find/startswith/endswith/upper/lower`, `list.append/insert/pop/remove/index/extend/sort/copy`, `dict.get/items/keys/values/pop/update/setdefault`
4. Test: dict with int/tuple/bool keys, method chaining

### Milestone 5: f-strings + type annotations (3 days)

1. Full f-string expression parsing (arbitrary expressions inside `{}`)
2. Format spec support (`{x:.2f}`, `{s:>10}`, `{n:04d}`)
3. `!s`, `!r`, `!a` conversions
4. `f'{x=}'` debug syntax
5. Variable annotation syntax (`x: int = 1`) — parse and discard type
6. Function annotation syntax — parse and discard types
7. Test: f-strings with ext calls, nested format specs, unicode padding

### Milestone 6: agent-harness integration (3 days)

1. Build a `harness.Tool` that wraps Taipan execution
2. Wire tool map → external function registry
3. Handle type conversion: `Object` ↔ `json.RawMessage` (tool call arguments)
4. Handle `print()` output → `ToolResult.UserContent`
5. Example: agent that writes Python to orchestrate tools
6. Benchmark: latency of compile + run for typical agent snippets (target: <1ms for 50-line scripts)

### Future milestones

- **Async/await** — coroutine suspension, `asyncio.gather()`, parallel tool execution
- **Snapshot serialisation** — `encoding/gob` or protobuf for durable execution
- **Basic classes** — `class Foo:` with `__init__` and methods (no inheritance)
- **`@dataclass`** — sugar for simple data containers
- **gopherbox integration** — expose as `python` command, or use VFS for sandboxed file ops
- **Walrus operator** — parser grammar extension for `:=`

---

## Testing strategy

### Approach: port Monty's test cases

pydantic/monty has ~250 test case `.py` files covering the exact Python subset we target. These are ideal because they test the agent-relevant patterns (external calls in loops, in try/except, in comprehensions, in f-strings, etc.).

Structure:
```
testdata/
├── execute_ok/          # scripts that should run successfully
│   ├── variables.py
│   ├── arithmetic.py
│   ├── control_flow.py
│   ├── functions.py
│   ├── comprehensions.py
│   ├── fstrings.py
│   └── ...
├── ext_call/            # scripts using external function calls
│   ├── basic.py
│   ├── nested.py
│   ├── in_loop.py
│   ├── in_try_except.py
│   └── ...
├── execute_err/         # scripts that should raise specific exceptions
│   ├── name_error.py
│   ├── type_error.py
│   └── ...
└── resource_limit/      # scripts that should hit resource limits
    ├── infinite_loop.py
    ├── deep_recursion.py
    └── ...
```

Test runner:
```go
func TestExecuteOK(t *testing.T) {
    files, _ := filepath.Glob("testdata/execute_ok/*.py")
    for _, f := range files {
        t.Run(filepath.Base(f), func(t *testing.T) {
            source, _ := os.ReadFile(f)
            prog, err := taipan.Compile(string(source), nil)
            require.NoError(t, err)
            result := taipan.Run(context.Background(), prog, nil)
            complete, ok := result.(*taipan.Complete)
            require.True(t, ok, "expected Complete, got %T", result)
            // Check stdout against expected output (# expect: comments in .py file)
        })
    }
}
```

### Compatibility testing

For any Python file that doesn't use external functions, we can validate correctness by running the same file through CPython and comparing stdout. This gives us a large, cheap regression suite.

---

## Risks and mitigations

| Risk | Mitigation |
|------|-----------|
| gpython is unmaintained (last commit 2+ years ago) | We're forking, not depending. We own the code. The parser/compiler are mature and correct for 3.4. |
| Python 3.4 parser misses modern syntax | The gap is well-defined (see table above). f-strings are the biggest item; walrus and match/case are optional. |
| `StringDict` limitation ripples through codebase | Fix early in Milestone 4. The `Object` interface already has `M__hash__` and `M__eq__`. |
| Snapshot serialisation is complex | Defer to Phase 4. The Run/Resume API works with in-memory snapshots from day one. |
| LLMs generate Python we don't support | Fail fast with a clear syntax/runtime error. The model can retry. In practice, LLMs generate simple Python when told the constraints. |
| Performance | gpython's jump-table dispatch is already fast. For agent use, compile+run latency matters more than throughput — target <1ms for typical 50-line agent snippets. |
