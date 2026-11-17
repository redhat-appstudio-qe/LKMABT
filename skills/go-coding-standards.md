---
name: go-coding-standards
description: >
  Use when writing or reviewing Go code to apply the team's coding standards
  for simplicity, readability, performance, and maintainability in the LKMABT
  codebase.
---

# Go Coding Standards

Team preferences for writing Go in LKMABT. Apply in every code change.

## Code Style

### 1. Fewer Lines, Better Readability

Eliminate unnecessary variables and boilerplate.
Avoid comments on unexported helpers whose name already explains intent.

```go
// Prefer
return DefaultKubeconfig(home, kfluxName)

// Avoid
path := DefaultKubeconfig(home, kfluxName)
return path
```

### 2. DRY — Extract Repeated Logic into Helpers

```go
// resolveKubeconfig and UsesDefaultKubeconfig both build the path this way.
func DefaultKubeconfig(home, kfluxName string) string {
    return filepath.Join(home, ".kube", "config-"+kfluxName)
}
```

### 3. Modularize — Small, Reusable Pieces

One concept per file. Break large functions into focused helpers.

```go
// internal/cluster/client.go       — ClientSet and kubeconfig loading
// internal/cluster/preflight.go    — prerequisite validation
// internal/cluster/build_outputs.go — Buildah artifact verification
// Each file owns one responsibility; changes stay isolated.
```

**File splitting convention:** when a file grows large, split into `X.go` and `X_helpers.go`.
`X.go` keeps the workflow-specific functions (single-caller, tied to a specific flow).
`X_helpers.go` keeps reusable utility functions (multi-caller or written for DRY purposes).
The same pattern applies to test files: `X_test.go` and `X_helpers_test.go`.

### 4. Single Responsibility — One Function, One Task

```go
func Execute(ctx context.Context, req config.Request, runner Runner, console *Console) error {
    if runner == nil {
        runner = UnconfiguredRunner{}
    }
    switch req.Command {
    case config.CommandRun:
        return RunClusters(ctx, req, runner, console)
    case config.CommandCleanup:
        cleanupRunner, ok := runner.(CleanupRunner)
        if !ok {
            return errors.New("cleanup services are not configured")
        }
        return cleanupRunner.Cleanup(ctx, req)
    }
    ...
}
```

### 5. Return Early — Guard Clauses Over Nesting

Fail fast at the top; keep the happy path at the lowest indentation level.

```go
func (l Ladder) runOCLogin(ctx context.Context, entry clusterfile.Entry) {
    if err := os.MkdirAll(filepath.Dir(entry.Kubeconfig), 0o700); err != nil {
        l.logf("create kubeconfig directory for cluster %s: %v", entry.Name, err)
        return
    }
    // ... happy path continues at base indentation
}
```

## Performance and Memory

The harness polls clusters and provider APIs while a cluster's provider
workflows run concurrently — these patterns matter.

| Guideline | Detail |
|-----------|--------|
| **Avoid allocations in hot paths** | Don't create slices/maps inside tight polling loops; reuse or lift outside. |
| **Pre-allocate when size is known** | `make([]string, 0, len(items))` instead of `var s []string`. |
| **Pointer receivers for large structs** | Methods on `RealCluster` and `Ladder` use value or pointer receivers matching their size and mutation needs. |
| **Respect `ctx.Done()`** | Check context cancellation in long-running cluster and provider calls. |
| **Prevent goroutine leaks** | Always pair goroutines with context or done-channel cleanup. |
| **Bounded retries** | Never retry indefinitely. Use bounded backoff or a max-attempt cap. |

## Quick Reference

| Principle | Do | Don't |
|-----------|----|-------|
| Brevity | `return DefaultKubeconfig(home, name)` | Assign to temp var, then return |
| DRY | Helper for repeated path/selector construction | Copy-paste the same 3 lines |
| Modularity | One concern per file | 500-line god function |
| Single responsibility | `Execute` dispatches, `RunClusters` executes | One function does both |
| Guard clauses | `if err != nil { return }` at top | Deeply nested if/else chains |
| Allocations | `make([]v1.TaskRun, 0, count)` | `var trs []v1.TaskRun` inside a loop |
| Context | `select { case <-ctx.Done(): }` | Blocking call with no cancellation |
| Retries | Bounded backoff with a max-attempt cap | `for { retry() }` |

For Go-specific pitfalls to avoid (nil interfaces, nil map writes, unsafe type assertions, etc.),
see the Go-Specific Pitfalls table in `skills/testing-standards.md`.

## Review Checklist

Before submitting code, verify:

- [ ] No unnecessary comments on unexported functions with clear names
- [ ] No duplicated logic that should be a helper
- [ ] Functions each have a single, clear responsibility
- [ ] Guard clauses used instead of deep nesting
- [ ] Slices/maps pre-allocated when capacity is known
- [ ] Context cancellation respected in cluster/network calls
- [ ] Retry loops are bounded (max attempts or backoff cap)
