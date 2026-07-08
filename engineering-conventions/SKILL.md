---
name: engineering-conventions
description: Enforce strict engineering conventions on ALL code work. Use this skill whenever the user writes, reads, edits, refactors, reviews, debugs, designs, plans, or restructures any code — including writing new functions, classes, modules, files, services, handlers, stores, repositories, APIs, helpers, or tests; fixing bugs; implementing features; renaming things; organizing files or folders; splitting responsibilities; improving naming, readability, or maintainability; designing architecture; converting logic to pseudocode; cleaning dead code, duplication, hidden dependencies, or unclear ownership; or any task that produces or modifies code in any language. Apply this skill even when the user does not explicitly ask for "conventions," "clean code," or "refactoring" — if code is being touched, use it. The only coding tasks to skip are throwaway one-liner shell commands and pure read-only file inspection.
---

# Engineering Conventions Enforcement

## What this skill is

A set of strict, non-negotiable engineering rules. They define ownership, naming, structure, error handling, dependencies, state, security, and testing.

The full ruleset lives in `references/guidelines.md`. **Read that file before doing any non-trivial code work in this project.** It is the source of truth — this SKILL.md is only the trigger and the enforcement contract.

## When to load the full guidelines

Always read `references/guidelines.md` when:

- Writing a new file, function, class, module, service, handler, store, repository, validator, mapper, builder, loader, creator, updater, deleter, or test.
- Refactoring, renaming, or reorganizing existing code.
- Reviewing code (yours or the user's).
- Designing architecture, planning implementation, or producing pseudocode.
- Removing dead code, duplication, or hidden dependencies.

Skip the read only for: throwaway one-liners and pure read-only inspection where no code will be produced.

## Hard rules — enforce always

These are summarized from the guidelines. They are not the full ruleset. Treat any conflict with the reference file as the reference file winning.

- Classes own behavior. Stores own state. Services own workflows. Helpers assist — they must never own domain logic.
- Functions are single-purpose and verb-based. Prefer one-word names (`run`, `load`, `save`, `validate`, `build`, `map`, `create`, `update`, `delete`, `handle`, `execute`).
- Long function names are a design smell. Fix ownership, not the name.
- No generic `Utils` / `Common` / `Manager` / `Helper` classes that hide domain responsibility.
- Private members start with `_` and are never accessed externally.
- Dependencies are injected through constructors or explicit parameters — never instantiated inside the consumer.
- Validate all external input at boundaries.
- Never swallow errors. Never use bare `except` / silent `pass`. Preserve causal chains with `raise ... from ...`.
- Log context, not raw data. Never log secrets, tokens, passwords, or sensitive payloads.
- One source of truth for state. Never store derived values; compute them.
- Test behavior, not implementation. Never call private methods from tests.
- No dead code, commented-out code, unused imports, magic values, or duplicated logic.
- Use parameterized queries. Never hardcode secrets. Validate every boundary input.

## Required workflow

For any non-trivial code work:

1. Identify ownership boundaries before writing anything.
2. Assign behavior to the correct owner (validator validates, mapper transforms, loader loads, repository persists, service orchestrates, handler adapts boundaries, store holds state).
3. Name functions by one action. If a verb-noun-noun-noun name appears, ownership is wrong — fix the owner first.
4. Inject dependencies explicitly. No hidden instantiation.
5. Validate inputs before using them.
6. Make error handling explicit and traceable.
7. Order file contents by responsibility (see guidelines §4).
8. Strip dead code, magic values, and duplication.
9. Test behavior through public APIs.

## Response behavior

- Be direct and implementation-focused.
- Do not restate the guidelines document.
- Mention only the rules that materially affected the change.
- If a rule is violated and cannot be fixed safely, call it out explicitly.
- If user intent conflicts with these conventions, preserve user intent but flag the tradeoff briefly.
- Favor clean, modular, non-over-engineered solutions.

## Final rule

Correct design naturally produces short function names, small clear classes, explicit ownership, safe boundaries, and minimal coupling. If the code requires long function names, large utility classes, hidden dependencies, or unclear boundaries — fix ownership first, then everything else falls out.
