# TODO

## Current focus

Build Taipan into a safe, fast inline execution runtime for agent tool orchestration.

Success bar:
- A model can emit 20-80 lines of Python with loops, comprehensions, helper functions, string formatting, JSON/text parsing, and 1-10 tool calls.
- Execution either completes successfully or fails fast under hard limits with clear errors.

## Priority 1: bounded and safe execution

- [x] Add real execution limits in the VM hot loop.
- [x] Enforce `context.Context` cancellation during bytecode execution, not only between frames.
- [x] Add hard limits for instructions, call depth, and stdout volume.
- [ ] Add pre-flight guards for obvious DoS cases such as giant repeats/exponents.
- [x] Remove dangerous or blocking builtins from the Taipan runtime: `open`, `eval`, `exec`, `compile`, `input`.
- [x] Add hostile-code tests for infinite loops, deep recursion, excessive output, and filesystem access attempts.

## Priority 2: common agent-code compatibility

- [ ] Add a minimal `typing` module or equivalent compatibility shim.
- [ ] Add a minimal `json` module.
- [ ] Evaluate a minimal `re` module after `typing` and `json`.
- [ ] Support variable annotations such as `x: int = 1`.
- [ ] Extend f-string support to handle common format specs and conversions.
- [ ] Keep failure modes explicit for unsupported syntax so callers can retry with simpler code.

## Priority 3: deterministic data model and tool interop

- [ ] Replace `StringDict` with an ordered dict that supports arbitrary hashable keys.
- [ ] Preserve deterministic dict iteration order in `items()`, `keys()`, and `values()`.
- [ ] Add shared Go/JSON <-> Taipan object conversion helpers for tool arguments and results.
- [ ] Ensure tool-call argument passing stays deterministic across runs.

## Priority 4: host API and performance

- [ ] Add an executor wrapper around the `Run`/`Resume` loop for hosts.
- [ ] Replace temp-file stdout capture with an in-memory bounded buffer.
- [ ] Add compile caching keyed by source plus external function set.
- [ ] Add end-to-end compile+run benchmarks for representative agent snippets.

## Priority 5: regression coverage

- [ ] Add a broader agent-focused corpus similar to the Monty test plan described in `README.md`.
- [ ] Add end-to-end tests for tool-call orchestration, exception injection, and conversion boundaries.
- [ ] Add regression coverage for compatibility shims and unsupported-syntax failures.
- [ ] Keep `go test ./...` green while broadening the runtime surface.

## Deferred until the core is solid

- [ ] Async/await and parallel external calls.
- [ ] Snapshot serialisation.
- [ ] Dataclasses and other higher-level convenience features.
- [ ] Broader stdlib surface area beyond the measured agent needs.

## Completed

- [x] `Complete.Stdout` and `Error.Stdout` now capture `print()` output.
- [x] Taipan `Run()` now restricts imports to built-in modules only.
- [x] Taipan `Compile()` now supports a practical subset of f-strings, including expressions used in agent tool-calling flows.
