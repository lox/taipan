# TODO

## Open Issues

- [ ] `Complete.Stdout` is not populated.
  - Current behaviour: `taipan.Complete.Stdout` is always empty.
  - Expected: `print()` output should be captured and returned via `Complete.Stdout` and `Error.Stdout`.

- [ ] Sandbox import policy is not fully locked down.
  - Current behaviour: there is still file-based import fallback to keep upstream tests passing.
  - Expected: imports should be restricted to built-in modules only unless explicitly enabled.

- [ ] f-string syntax is not supported yet.
  - Current behaviour: modern f-string parsing fails.
  - Expected: support is planned in Milestone 5.
