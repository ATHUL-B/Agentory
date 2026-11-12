# CLAUDE.md

Guidance for Claude (and any AI/human contributor) working in this repository.
Read this file fully before making changes. When a rule here conflicts with a
quick shortcut, follow the rule.

## 1. Working Principles

- **Understand before changing.** Read the relevant code and `docs/` first.
- **Smallest correct change.** Do what was asked; no drive-by refactors or
  speculative features. Note unrelated issues in `docs/backlog.md` instead.
- **Ask when ambiguous.** If requirements are unclear or a decision is
  irreversible, ask rather than guess.
- **Be honest in reports.** State what was run, what passed, what failed, and
  what was skipped. Never claim something works without verifying it.

## 2. Code Structure & OOP

- Organize code into clear modules/packages with a single responsibility each.
- **Prefer classes and OOP** wherever they model the domain naturally:
  - Encapsulate state and behavior together; keep attributes private by default.
  - Program to interfaces/abstract base classes, not concrete implementations.
  - Favor **composition over inheritance**; inherit only for true "is-a" cases.
  - Use dependency injection (pass collaborators in) so units are testable.
- Follow SOLID, DRY, and KISS. Keep functions small (do one thing) and classes
  cohesive. Avoid god-classes and deep inheritance trees.
- Use pure functions/dataclasses for stateless logic and plain data; don't force
  a class where a function is clearer.
- Name things for intent. No magic numbers/strings: use constants or enums.
- Keep side effects (I/O, network, time, randomness) at the edges, behind
  interfaces, so core logic stays deterministic.
- Type-annotate public APIs and keep them documented (docstrings/comments for
  *why*, not *what*).

## 3. Testing

- **Every feature ships with tests.** No feature PR is complete without them.
- Cover: happy path, edge cases, and error handling. Every bug fix gets a
  regression test that fails before the fix.
- Test pyramid: many fast unit tests, fewer integration tests, minimal
  end-to-end tests.
- Tests must be deterministic and isolated: no reliance on network, wall-clock
  time, test ordering, or shared state. Mock/fake external boundaries only.
- Mirror source layout under `tests/` (e.g. `src/x/y.py` -> `tests/x/test_y.py`).
- Run the full test suite before every commit/push. Do not skip, delete, or
  weaken tests to make them pass; fix the cause.
- Keep coverage high on core logic and never let it drop in a PR.

## 4. Git Workflow

### Branching
- `main` is always releasable and protected: **no direct pushes**.
- One branch per feature/fix, named `<type>/<short-description>`
  (e.g. `feat/agent-registry`, `fix/config-loading`, `docs/update-hld`).

### Commits = checkpoints
- Commit **small and often**: each logical step of progress is its own commit.
  Commits are checkpoints you could safely return to.
- Each commit should build and pass tests where practical.
- Use [Conventional Commits](https://www.conventionalcommits.org/):
  `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`, `perf:`, `ci:`.
  Subject in imperative mood, <= 72 chars; body explains *why*.
- Never commit secrets, credentials, or large generated/binary artifacts.

### Pull requests
- **One PR per feature** (or fix). Keep PRs focused and reviewable.
- Each PR must include: description of what/why, linked backlog item, tests,
  and updated docs (see section 5).
- PR checklist before requesting review:
  - [ ] Tests added/updated and passing
  - [ ] Lint/format/type checks clean
  - [ ] `docs/` updated (architecture, backlog, HLD as applicable)
  - [ ] No secrets, debug code, or unrelated changes
- Prefer squash-merge to keep `main` history clean. Never force-push shared
  branches or rewrite `main`.

## 5. Documentation (`docs/`)

The `docs/` folder is the source of truth for design and planning and **must be
updated in the same PR as the change it describes**.

```
docs/
  architecture.md     # System overview, components, data flow, key decisions
  backlog.md          # Prioritized work items with status
  hld/                # High-Level Designs, one file per feature/epic
  adr/                # (optional) Architecture Decision Records
```

- **architecture.md** - current-state architecture; update whenever structure,
  components, or integrations change.
- **backlog.md** - every planned item with status (`todo` / `in-progress` /
  `done`); move items as work progresses and add newly discovered work.
- **hld/** - write the HLD *before* implementing a non-trivial feature
  (goals, non-goals, design, alternatives, risks, test plan).
- Record significant, hard-to-reverse choices as ADRs with context and
  consequences.
- Keep `README.md` accurate: purpose, setup, usage, and how to run tests.
- Stale docs are bugs. If code and docs disagree, fix the docs in the PR.

## 6. Code Quality & Tooling

- Use an auto-formatter and a linter; commit their config. Do not hand-debate
  style that a tool can enforce.
- Use static type checking where the language supports it.
- Pin dependencies (lockfile) and keep them minimal; justify every new one.
- Set up CI to run format, lint, type check, and tests on every PR; a red CI
  blocks merge.
- Consider pre-commit hooks to catch issues before they reach CI.

## 7. Security

- Never hardcode or log secrets. Load configuration from environment variables
  or a secrets manager; commit only a `.env.example`.
- Validate and sanitize all external input; apply least privilege.
- Avoid unsafe constructs (`eval`, shell string interpolation, unsafe
  deserialization). Parameterize queries and commands.
- Keep dependencies updated and review them for known vulnerabilities.
- Be especially careful with agent capabilities (tool execution, file access,
  network): sandbox, allow-list, and require explicit permission for
  destructive actions.

## 8. Error Handling, Logging & Observability

- Fail fast with clear, actionable errors. Define domain-specific exception
  types; never swallow exceptions silently.
- Use structured, leveled logging (not `print`). Never log sensitive data.
- Make behavior observable: meaningful log context, and metrics/tracing where
  applicable.
- Design external calls with timeouts, retries with backoff, and idempotency.

## 9. Configuration & Environments

- Separate config from code. Provide sensible defaults and validate config at
  startup.
- Keep dev, test, and prod behavior consistent; avoid environment-specific code
  paths.
- Builds and setup must be reproducible from a clean clone using documented
  commands.

## 10. Performance & Reliability

- Make it correct, then clear, then fast. Measure before optimizing.
- Avoid premature abstraction and premature optimization.
- Handle resources deterministically (context managers / `finally`) and keep
  operations idempotent where possible.

## 11. Working with AI Agents (Claude)

- Plan before coding for non-trivial tasks; share the plan for approval when
  scope is large.
- Verify, don't assume: run the tests/linters and read real output before
  declaring done.
- Do not take destructive or outward-facing actions (force-push, deleting
  branches/files, publishing, sending data externally) without explicit
  approval.
- Keep context small and focused; prefer reading specific files over broad
  dumps. Update this file if you discover conventions worth recording.

## 12. Definition of Done

A change is done only when:
1. It is implemented in a structured, OOP-appropriate way.
2. Tests cover it and the full suite passes.
3. Lint, format, and type checks pass.
4. `docs/` (architecture, backlog, HLD) reflect the change.
5. Work is committed in small conventional commits on a feature branch.
6. A focused PR is raised with a clear description.
