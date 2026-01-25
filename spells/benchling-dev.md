---
name: benchling-dev
description: Guide to the `dev` CLI for Benchling development - testing, Docker, DB, codegen
---

# The `dev` Command

The `dev` CLI is the unified tool for Benchling development tasks.

## Quick Reference

| Task | Command |
|------|---------|
| Start devbox | `dev compose up` |
| Stop devbox | `dev compose down` |
| View logs | `dev compose logs` |
| Run Python tests | `dev test pyunit run <path>` |
| Run JS tests | `dev test jsunit run` |
| Run linters | `dev check lint` |
| Auto-fix lint | `dev check lint --autofix` |
| Start webpack | `dev start webpack` |
| DB shell | `dev db pgcli` |
| Django shell | `dev run shell` |
| Generate migration | `dev generate migration "message"` |
| Install deps | `dev setup requirements` |
| Regenerate routes | `dev generate registration` |

## Discovering Commands

```bash
dev --help                    # Top-level groups
dev <group> --help            # Subcommands in a group
dev tree print                # Full command tree
dev tree search <keyword>     # Find commands by keyword
```

## Commands You Forget

### `dev setup requirements`

**When to use**: After pulling changes that modify `requirements*.txt` or `package.json`, or when dependencies seem broken.

```bash
dev setup requirements              # Install Python + JS deps on host and devbox
dev setup requirements --force      # Force reinstall everything
dev setup requirements --no-restart # Don't restart containers after
dev setup requirements --uv         # Use uv instead of pip (faster)
```

This installs:
- Python packages from requirements files
- JS packages via yarn
- pipx global tools

### `dev generate registration`

**When to use**: After adding or modifying Flask route registrations (lazy API routes). CI will fail with "Generated API is out-of-date" if you forget.

```bash
dev generate registration           # Regenerate lazy Flask routes
dev generate registration --check   # Check if up-to-date (used by CI)
```

Run this when you:
- Add a new API endpoint
- Modify route decorators
- See CI failures about generated API being out-of-date

## Test Patterns

```bash
# File
dev test pyunit run tests/unit/lib/lib_test.py

# Module path
dev test pyunit run tests.unit.lib.lib_test

# Specific class
dev test pyunit run tests.unit.lib.lib_test:LibTest

# Specific method
dev test pyunit run tests.unit.lib.lib_test:LibTest.test_method

# Filter by keyword
dev test pyunit run tests.unit.lib -k='pattern'

# Show more patterns
dev test pyunit cheatsheet
```

## Full Documentation

See `~/notes/dev.md` for comprehensive documentation of all command groups.
