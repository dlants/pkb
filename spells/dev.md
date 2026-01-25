# The `dev` Command - Developer Utilities

The `dev` command is the primary CLI tool for Benchling development. It provides a unified interface for virtually all development tasks including testing, database management, Docker orchestration, code generation, deployment, and more.

**Location**: Installed at `~/.local/bin/dev`, source code in `packages/benchling-dev-utilities/`

**Discovery**: Use `dev tree` to explore commands, or `dev tree search <keyword>` to find specific functionality.

---

## Quick Reference - Most Common Commands

| Task | Command |
|------|---------|
| Run Python tests | `dev test pyunit run <path>` |
| Run JS tests | `dev test jsunit run` |
| Start devbox | `dev compose up` |
| Stop devbox | `dev compose down` |
| Run linters | `dev check lint` |
| Start webpack | `dev start webpack` |
| Open DB shell | `dev db pgcli` |
| Generate migration | `dev generate migration "message"` |
| Check test patterns | `dev test pyunit cheatsheet` |

---

## Command Categories

### 1. Docker Compose (`dev compose`)

Controls the local Docker development environment (devbox).

```bash
dev compose up           # Start all services (detached)
dev compose down         # Stop and remove containers
dev compose logs         # View container logs
dev compose ps           # List running containers
dev compose restart      # Restart services
dev compose exec <svc>   # Execute command in container
dev compose start/stop   # Start/stop without recreating
dev compose build        # Rebuild images
```

**When to use**: Starting/stopping your local development environment, viewing logs, debugging container issues.

---

### 2. Testing (`dev test`)

Runs all types of tests: Python, JavaScript, Cypress, and more.

#### Python Tests
```bash
# Run tests
dev test pyunit run tests/unit/lib/lib_test.py
dev test pyunit run tests.unit.lib.lib_test
dev test pyunit run tests.unit.lib.lib_test:LibTest
dev test pyunit run tests.unit.lib.lib_test:LibTest.test_method

# Filter by keyword
dev test pyunit run tests.unit.lib -k='pattern1 or pattern2'

# Server mode (faster subsequent runs)
dev test pyunit server                    # Start server
dev test pyunit client <test_path>        # Run test on server

# Debugging
dev test pyunit debug <test_path>         # VSCode debugger attach

# Profiling
dev test pyunit profile <test_path>       # Profile test execution

# Show common patterns
dev test pyunit cheatsheet
```

#### JavaScript Tests
```bash
dev test jsunit run           # Run JS unit tests
dev test jsunit watch         # Watch mode - rerun on changes
dev test jsunit debug         # Run with Node debugger
dev test jsunit profile       # Profile tests
```

#### Cypress (E2E) Tests
```bash
dev test cypress run          # Run headless
dev test cypress open         # Open Cypress GUI
dev test cypress debug        # Full debugging
dev test cypress flake-finder # Detect flaky tests
```

#### Other Test Commands
```bash
dev test diff                 # Run tests for changed files
dev test migration            # Run migration tests
dev test benchlint            # Test Benchlint ESLint plugin
```

**When to use**: Validating code changes, debugging test failures, TDD workflow.

---

### 3. Code Quality (`dev check`)

Runs linters, type checkers, and formatters.

```bash
dev check lint                # Run all linters (eslint, ruff, prettier, etc.)
dev check lint --autofix      # Auto-fix issues
dev check lint --only RUFF_LINT,ESLINT  # Run specific checkers
dev check mypy                # Run mypy type checking
dev check typescript          # Run TypeScript checking
dev check branch-migrations   # Verify migration branches don't conflict
dev check migration-upgrades  # Validate migration upgrades
dev check migration-downgrades # Validate migration downgrades
dev check domain-graph        # Validate domain graph format
```

Available checkers: `AST_GREP`, `BAZEL`, `DOMAIN_GRAPH_LINT`, `ESLINT`, `FIXIT`, `FLAKE8`, `MONOLITH_MYPY`, `OXLINT`, `PRETTIER`, `RUFF_FORMAT`, `RUFF_LINT`, `TYPESCRIPT`, and more.

**When to use**: Before committing code, CI pre-checks, fixing lint errors.

---

### 4. Database (`dev db`)

Database management and interaction.

```bash
dev db pgcli                  # Interactive PostgreSQL client
dev db psql                   # psql shell
dev db pgcli-warehouse        # Connect to warehouse DB
dev db init                   # Recreate database (destructive!)
dev db seed                   # Seed database with test data
dev db backup                 # Backup to dumpfile
dev db restore                # Restore from dumpfile
dev db upgrade                # Run alembic upgrade to head
dev db alembic <args>         # Run alembic commands
dev db dump-schema            # Dump schema for tests
dev db fix-migration-head-file       # Update cached alembic head
dev db fix-alembic-migrations        # Fix down revisions
```

**When to use**: DB exploration, running migrations, debugging data issues, resetting local state.

---

### 5. Code Generation (`dev generate`)

Generate boilerplate, migrations, and auto-generated code.

```bash
# Migrations
dev generate migration "Add user column"      # Generate DB migration
dev generate migration-shared "Shared change" # Shared schema migration

# Frontend
dev generate frontend         # Generate GraphQL types (alias: graphql)
dev generate public-interface # Generate public interfaces

# Search
dev generate search migration # Generate search migration
dev generate search type      # Add new search type
dev generate search filter    # Add new search filter

# Other
dev generate bazel            # Generate BUILD files with Gazelle
dev generate v3-spec          # Generate V3 REST API spec
dev generate domain-graph     # Generate domain graph types
dev generate protos           # Compile protocol buffers
dev generate boilerplate      # Generate component boilerplate
dev generate react-component  # React component boilerplate
dev generate pipeline-step    # Generate pipeline step
```

**When to use**: Adding DB migrations, updating generated types after schema changes, creating new components.

---

### 6. Setup (`dev setup`)

Initialize and configure development environment.

```bash
dev setup monolith            # Full monolith setup (resets DB!)
dev setup everything          # Run all essential setup commands
dev setup images              # Pull Docker images
dev setup db                  # Recreate database
dev setup requirements        # Install Python/JS dependencies
dev setup backend-deps        # Install Python requirements
dev setup yarn                # Run yarn with Benchling config
dev setup git-hooks           # Install pre-push/post-checkout hooks
dev setup dev-tools           # Install formatters, linters
dev setup pip-compile         # Regenerate requirements files
dev setup pycharm             # Configure PyCharm
dev setup sg3                 # Set up Sightglass3
dev setup warehouse           # Set up warehouse
dev setup integration-tests   # Set up for integration tests
dev setup datadog enable/disable/status  # Configure Datadog agent
```

**When to use**: New environment setup, after pulling major changes, fixing broken dev environment.

---

### 7. Debugging (`dev debug`)

Attach debuggers to running processes.

```bash
dev debug web                 # CLI debugging (pdb) with web foreground
dev debug web-vscode          # VSCode debugging with web foreground
dev debug worker              # Debug Celery worker
dev debug pyunit <test>       # Debug Python test (VSCode attach)
dev debug jsunit              # Debug JS tests (Node debugger)
dev debug llm-eval            # Debug LLM eval runs
```

**When to use**: Stepping through code, investigating runtime issues.

---

### 8. Long-Running Servers (`dev start`)

Start development servers.

```bash
dev start webpack             # Webpack dev server (JS/TS builds, proxying)
dev start storybook           # Storybook for React component development
dev start dmypy               # Daemon mypy server for fast typechecking
dev start mypy-watch          # dmypy with directory watching
```

**When to use**: Frontend development, component isolation testing.

---

### 9. Running Scripts (`dev run`)

Execute one-off commands and scripts.

```bash
dev run devbox <command>      # Run command on devbox
dev run shell                 # Open Django shell (manage.py shell)
dev run script <script_name>  # Run Python script via run-scripts system
```

**When to use**: Running maintenance scripts, interactive exploration, one-off operations.

---

### 10. Bazel (`dev bazel`)

Bazel build system commands.

```bash
dev bazel test <target>       # Run Bazel tests
dev bazel run <target>        # Run Bazel target
dev bazel generate            # Generate BUILD files (Gazelle)
dev bazel fix                 # Auto-fix BUILD files
dev bazel format              # Format Bazel files
dev bazel clean               # Clean build cache
dev bazel check               # Run Bazel tests for monolith
dev bazel remote-test         # Run tests on BuildBuddy (remote)
dev bazel guess-tests         # Find tests affected by changes
dev bazel graph               # Create dependency graph
dev bazel create-target       # Create BUILD.bazel files for paths
```

**When to use**: Running specific build targets, managing BUILD files, CI-related tasks.

---

### 11. AWS (`dev aws`)

AWS authentication and helpers.

```bash
dev aws login                 # Cache AWS access token (SSO)
dev aws logout                # Clear cached credentials
dev aws browser-login         # Open AWS console session
dev aws ecr-login             # Authenticate with ECR
dev aws ecr-pull <image>      # Pull image from ECR (with auth)
dev aws exec <cmd>            # Run command with AWS profile
dev aws export                # Print credentials as ENV vars
dev aws list                  # List all profiles
dev aws debug-sso             # Debug SSO credential status
```

**When to use**: AWS authentication, pulling images, debugging credential issues.

---

### 12. Deployment (`dev deploy`)

Deploy services to environments.

```bash
dev deploy <target>           # Deploy service
dev deploy --rev <commit>     # Deploy specific revision
dev deploy --local            # Fast local deployment (EKS only)
dev deploy --dry-run          # Preview without deploying
dev deploy --gxp              # Deploy to GXP environment
dev deploy --qualify          # Run qualification tests
```

Key options:
- `--rev`: Git revision to deploy
- `--local/-l`: Fast local deploy (updates code only, not persistent)
- `--skip-publish`: Skip image publishing
- `--rush/--no-rush`: Rush deploy mode
- `--unlock-instance`: Unlock terraform workspace

**When to use**: Deploying to dev/staging environments, testing deployments.

---

### 13. Search (`dev search`)

Elasticsearch/search management.

```bash
dev search migrate-up         # Run ES index migrations
dev search reindex            # Reindex all models
dev search reset-index        # Delete, reinitialize, reindex
dev search reset-volumes      # Reset volumes, recreate assets
dev search export-node-paths  # Export GraphQuery paths to YAML
```

**When to use**: Search index issues, schema changes affecting search.

---

### 14. CI (`dev ci`)

Interact with Buildkite CI.

```bash
dev ci errors                 # List CI errors for current branch
dev ci failures               # List CI failures for current branch
dev ci failed-tests           # List failed tests from build
dev ci buildbuddy-artifacts   # Download test artifacts from BuildBuddy
```

**When to use**: Debugging CI failures, retrieving test results.

---

### 15. Doctor (`dev doctor`)

Diagnose and fix development environment issues.

```bash
dev doctor checkup            # List pending setup commands
dev doctor heal               # Run all pending commands
dev doctor cocktail           # Quick fix for common issues
dev doctor list               # Show all registered commands
dev doctor pop                # Run next pending command
dev doctor skip               # Skip to latest command index
```

**When to use**: Environment problems, after updates, troubleshooting.

---

### 16. Self Management (`dev self`)

Manage dev-utilities itself.

```bash
dev self version              # Show current version
dev self update               # Update to required version
dev self list                 # List available versions
dev self install <version>    # Install specific version
```

**When to use**: Updating dev tools, version issues.

---

### 17. TypeScript (`dev typescript`)

TypeScript-specific commands.

```bash
dev typescript check          # Run TypeScript type checking
dev typescript script <name>  # Run script from client/tools/scripts
```

---

### 18. Codemods (`dev codemod`)

Automated code transformations.

```bash
dev codemod jscodeshift <transform>  # Run jscodeshift transform
dev codemod monkeytype               # Work with runtime types
```

---

### 19. Celery (`dev celery`)

Celery queue management.

```bash
dev celery queue-lengths      # Show queue lengths
dev celery dump-queue <queue> # Dump queue contents (up to 1000)
dev celery clear-queue <queue># Clear specific queue
dev celery clear-all-queues   # Clear all queues
```

---

### 20. Tracing (`dev tracing`)

Local distributed tracing.

```bash
dev tracing start             # Start OpenTelemetry collector + Jaeger
dev tracing stop              # Stop tracing services
```

---

### 21. Events (`dev events`)

Local event delivery.

```bash
dev events start              # Start event delivery services
dev events stop               # Stop and tear down
dev events restart            # Restart services
```

---

### 22. CDC (`dev cdc`)

Change Data Capture (Debezium/Kafka).

```bash
dev cdc start                 # Start CDC services
dev cdc stop                  # Stop CDC services
dev cdc down                  # Stop and remove all CDC services
dev cdc logs <service>        # Tail service logs
dev cdc connect-pg            # Create Debezium connector
dev cdc list-connectors       # Show existing connectors
dev cdc restart-connector     # Restart Debezium connector
dev cdc snapshot-table        # Trigger incremental snapshot
```

---

### 23. Domain Graph (`dev domain-graph`)

Domain graph management.

```bash
dev domain-graph generate     # Lint SDL, generate types
dev domain-graph version check/upgrade/delete  # Version management
```

---

### 24. Info (`dev info`)

Learn about the codebase.

```bash
dev info me                   # Your GitHub teams and components
dev info inspect <path>       # Get info (codeowners, etc.) for path
dev info codeowners <path>    # Detailed codeowners resolution
dev info routes               # List Flask routes
dev info dependency <pkg>     # Get dependency owner
```

---

### 25. Config (`dev config`)

Manage dev tool configuration.

```bash
dev config show               # Display current config
dev config path               # Print config file path
dev config clear              # Clear config
```

---

### 26. Other Commands

```bash
dev benchmark                 # Benchmark command execution time
dev simulate async            # Simulate celery/jobs locally
dev simulate prepare-data     # Download Datadog metrics for testing
dev profile-python <cmd>      # Profile Python with py-spy
dev helpme dockernet          # Fix Docker networking to 172.17.x.x
dev visual compare            # Compare PDF exports visually
dev specs validate/test/dist  # Manage dev_platform specs
dev v3-spec generate          # Generate V3 REST API spec
dev jj check-lint             # Run lint on changed files (jj VCS)
dev mcp-tools atlassian       # Start Atlassian MCP Docker tool
```

---

## Legacy `bin/dev` Commands

The `bin/dev` script wraps the modern `dev` command. These aliases still work but show deprecation notices:

| Legacy | Modern |
|--------|--------|
| `bin/dev pyunit` | `dev test pyunit run` |
| `bin/dev webpack` | `dev start webpack` |
| `bin/dev initdb` | `dev setup db` |
| `bin/dev check` | `dev check lint` |
| `bin/dev psql` | `dev db psql` |
| `bin/dev up` | `dev compose up` |
| `bin/dev down` | `dev compose down` |

---

## Tips

1. **Explore commands**: `dev tree print` shows the full command tree
2. **Search for commands**: `dev tree search <keyword>`
3. **Get help**: `dev <command> --help` or `dev <command> -h`
4. **Test patterns**: `dev test pyunit cheatsheet` for common pytest patterns
5. **Fast pyunit**: Use server mode (`dev test pyunit server` then `client`) for faster iteration
6. **Update dev tools**: Run `dev self update` when prompted or after pulling changes


