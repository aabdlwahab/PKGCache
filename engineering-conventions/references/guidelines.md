# Guidelines

This document defines the engineering conventions that govern how codebases are written, structured, reviewed, and evolved.

It is written as a standalone system document.

You should be able to read this file without opening any code and still understand:

- How code should be structured
- How responsibilities should be assigned
- How naming should reflect ownership
- How behavior should be organized
- How boundaries should be enforced
- How correctness, safety, and maintainability are preserved

This document defines **rules, NOT suggestions**.

## TL;DR

This document defines how code must be written and organized.

### Core Rules

- Classes own behavior, stores own state
- Functions are single-purpose and verb-based
- Prefer one-word function names (`run`, `load`, `save`, `validate`)
- Long function names indicate wrong ownership
- No generic utility classes

### Structure

- Follow fixed file ordering
- Group functions by responsibility
- Services orchestrate, helpers assist (never own logic)

### Behavior

- Validate all external input
- Never swallow errors
- Log context, not data
- Test behavior, not implementation

### Anti-Patterns

- Long or descriptive function names
- Mixed-responsibility classes (`Utils`)
- Functions doing multiple steps
- Duplicated or derived state
- Hidden dependencies

### Final Rule

Correct design leads to:

- Short function names
- Clear classes
- Safe refactoring

If not → fix ownership, not code.

---

## 1. Purpose

The purpose of this document is to enforce a predictable and scalable engineering system.

The conventions ensure that every codebase is:

- Deterministic
- Readable without execution
- Structurally consistent
- Easy to refactor safely
- Easy to review
- Easy to extend

This document is intentionally:

- Strict
- Opinionated
- Boundary-driven
- Ownership-focused

This document is intentionally not:

- Framework-specific
- Language-specific
- Flexible in interpretation

## 2. Design Principles

The system follows these principles:

- Clarity over cleverness
- Explicit over implicit
- Deterministic over surprising
- Predictable over flexible
- Composition over inheritance (where applicable)
- Separation of concerns
- Single source of truth
- Behavior belongs to owners, not utilities
- Classes define context, functions define actions
- Short names are a result of correct ownership

### Example

Bad:

```python
def process_user_data_and_send_email_and_store():
    ...
```

Good:

```python
class UserProcessor:
    def process(self):
        ...
```

## 3. Naming System

### Role

Naming encodes ownership and intent.

Names must reflect:

- What owns the behavior
- What the function does
- Whether the function mutates state

### Function Naming Rules

All functions must:

- Be verb-based
- Describe a single action
- Avoid encoding workflow logic

### Prefix System


| Category     | Prefix                   |
| ------------ | ------------------------ |
| Boolean      | is*, has*, can*, should* |
| Getter       | get                      |
| Setter       | set                      |
| Builder      | build                    |
| Mapper       | map                      |
| Validator    | validate                 |
| Loader       | load*, fetch*            |
| Creator      | create                   |
| Updater      | update                   |
| Deleter      | delete*, remove*         |
| Handler      | handle                   |
| Orchestrator | execute*, process*       |


### One-Word Rule

Functions should be **one word whenever context allows**.

This is not a stylistic rule — it is a **design constraint**.

If a function name requires multiple domain words:

→ The ownership is wrong

### Private Naming Rule

Private members must be explicitly marked.

- All private functions must be prefixed with `_`
- All private variables must be prefixed with `_`
- Private members are **internal to the class/module only**
- Private members must **never be accessed externally**
- Public APIs must not depend on private members directly

#### Example

Bad:

```python
def validate_user_request_payload():
    ...
```

Good:

```python
class UserRequestValidator:
    def validate(self):
        ...
```

Bad:

```python
def execute_order_payment_and_notification():
    ...
```

Good:

```python
class PaymentExecutor:
    def execute(self):
        ...
```

Bad:

```python
class UserService:
    def validate(self):
        return self.helper()

    def helper(self):
        ...
```

Good:

```python
class UserService:
    def validate(self):
        return self._helper()

    def _helper(self):
        ...
```

Bad:

```python
token = generate()
```

Good:

```python
_token = generate()
```

## 4. Structural Organization

### Role

Structure defines readability and predictability.

Every file must be readable top-to-bottom without jumping.

### File Ordering

1. Public exports
2. Constants
3. Types
4. State
5. Initialization
6. Predicates
7. Getters
8. Setters
9. Builders
10. Validators
11. Loaders
12. Creators
13. Updaters
14. Deleters
15. Orchestrators
16. Private helpers
17. Cleanup

### Example

```python
# constants
TIMEOUT = 30

# types
class UserRequest: ...

# predicates
def is_valid(): ...

# loaders
def load(): ...

# creators
def create(): ...

# orchestrators
def execute(): ...
```

## 5. Ownership Model

### Role

Ownership defines where behavior lives.

### Rules

- Classes own behavior
- Stores own state
- Services own workflows
- Helpers must not own domain logic

### Anti-pattern

```python
class Utils:
    def validate_user(): ...
    def send_email(): ...
```

### Correct

```python
class UserValidator:
    def validate(): ...

class EmailSender:
    def send(): ...
```

## 6. Function Design

### Role

Functions define atomic behavior.

### Rules

- Single responsibility
- No hidden side effects
- Minimal branching
- Early returns preferred
- No workflow chaining

### Example

Bad:

```python
def process(order):
    validate(order)
    save(order)
    notify(order)
```

Good:

```python
class OrderProcessor:
    def process(self, order):
        self.validator.validate(order)
        self.repository.save(order)
        self.notifier.send(order)
```

## 7. State Model

### Role

State must be predictable and non-duplicated.

### Rules

- One source of truth
- No derived duplication
- Explicit mutation only

### Example

Bad:

```python
state = {
    "items": [1, 2],
    "total": 3
}
```

Good:

```python
state = {
    "items": [1, 2]
}

def get_total(state):
    return sum(state["items"])
```

## 8. Error Model

### Role

Errors must be explicit and traceable.

### Rules

- Never swallow errors
- Separate business vs system errors
- Log context, not noise

### Example

Bad:

```python
try:
    save()
except:
    pass
```

Good:

```python
try:
    save()
except DatabaseError as e:
    logger.error("Save failed", extra={"id": entity.id})
    raise SaveError() from e
```

## 9. Dependency Model

### Role

Dependencies must be explicit and replaceable.

### Rules

- No hidden instantiation
- Use injection
- Avoid circular dependencies

### Example

Bad:

```python
class Service:
    def __init__(self):
        self.repo = Repo()
```

Good:

```python
class Service:
    def __init__(self, repo):
        self.repo = repo
```

## 10. API Model

### Role

APIs define system boundaries.

### Rules

- Validate all input
- Version explicitly
- Return structured responses

### Example

Bad:

```python
def create(data):
    return repo.save(data)
```

Good:

```python
def create(request: UserRequest) -> UserResponse:
    validated = validator.validate(request)
    user = creator.create(validated)
    return mapper.map(user)
```

## 11. Logging Model

### Role

Logs explain behavior, not execution steps.

### Rules

- Log context, not data dumps
- Never log secrets
- Avoid noisy logs

### Example

Bad:

```python
logger.info(f"password={password}")
```

Good:

```python
logger.info("Login failed", extra={"user_id": user_id})
```

## 12. Performance Model

### Role

Performance follows correctness.

### Rules

- No premature optimization
- Remove redundant work
- Optimize bottlenecks only

### Example

Bad:

```python
for i in items:
    total = sum(items)
```

Good:

```python
total = sum(items)
```

## 13. Security Model

### Role

Security is enforced at boundaries.

### Rules

- Validate all input
- Never hardcode secrets
- Use parameterized queries

### Example

Bad:

```python
query = f"SELECT * FROM users WHERE email = '{email}'"
```

Good:

```python
query = "SELECT * FROM users WHERE email = :email"
```

## 14. Testing Model

### Role

Tests validate behavior, not structure.

### Rules

- Deterministic
- No implementation coupling
- Mock only external systems

### Example

Bad:

```python
assert service._internal() == 5
```

Good:

```python
assert service.execute(request).total == 5
```

## 15. Documentation Model

### Role

Documentation explains intent and boundaries.

### Rules

- Explain why
- Document public contracts only
- Remove outdated comments

### Example

Bad:

```python
# increment counter
counter += 1
```

Good:

```python
# retry once due to eventual consistency
retry()
```

## 16. Refactoring Model

### Role

Code must remain clean and minimal.

### Prohibited

- Dead code
- Commented code
- Unused imports
- Magic numbers
- Duplicated logic

### Example

Bad:

```python
# old logic
# run_old()

timeout = 300
```

Good:

```python
REQUEST_TIMEOUT = 300
```

## 17. Review Model

### Role

Reviews enforce system integrity.

### Checklist

- Naming reflects ownership
- Functions are single-purpose
- Classes follow SRP
- No hidden side effects
- Dependencies are injected
- Errors are explicit
- Logs are safe
- Tests validate behavior

## Final Principle

A correct system naturally produces:

- Short function names
- Small classes
- Clear ownership
- Minimal coupling

If the code requires:

- Long function names
- Large utility classes
- Unclear boundaries

Then the design is incorrect and must be refactored.
