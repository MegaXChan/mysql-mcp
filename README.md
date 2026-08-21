# mysql-mcp

English | [简体中文](README.zh-CN.md)

[![CI](https://github.com/MegaXChan/mysql-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/MegaXChan/mysql-mcp/actions/workflows/ci.yml)

`mysql-mcp` is a MySQL Model Context Protocol (MCP) server written in Go. It supports MySQL 5.7, MySQL 8.x, multiple data sources, stdio, and Streamable HTTP, with clearly separated tools for queries, metadata, monitoring, stored functions, and a small set of administrative operations.

The project's secure default is read-only: write operations must pass the global feature gates, the data source read-only policy, SQL AST validation, and MySQL account permissions.

## Key Features

- Provides CLI commands such as `serve` and `validate-config` using `github.com/urfave/cli/v3`.
- A single process can connect to multiple MySQL 5.7/8.x data sources, maintaining separate connection pools for reads, writes, and monitoring.
- In HTTP mode, each endpoint is fixed at `/{datasource_name}/mcp`; tool parameters cannot switch data sources.
- HTTP supports unauthenticated mode and static Bearer Token authentication.
- SQL is classified after building an AST with `vitess.io/vitess/go/vt/sqlparser`, rather than determining whether a statement is a `SELECT` from a string prefix.
- Supports read-only queries, DML, DDL, metadata, fixed monitoring queries, a stored function allowlist, and typed `KILL QUERY` operations.
- Strictly validates configuration files for unknown fields, duplicate YAML keys, invalid combinations, and secret references.
- Enforces limits on query duration, SQL size, row count, result size, and per-data-source concurrency.

## Requirements and Build

- Go 1.26.4
- MySQL 5.7 or MySQL 8.x

```bash
make build
make test
make test-race
```

On every push, Pull Request, and manual trigger, the GitHub Actions CI workflow reads the exact Go version from `go.mod` and runs dependency verification, `gofmt`, `go vet`, race tests, coverage, CLI build, bounded SQL policy fuzz testing, and a container build/smoke test. CI does not require database passwords or other repository secrets. A separate publishing workflow repeats the required verification before producing Docker images and GitHub Release artifacts from eligible branches and tags.

## Makefile

The repository includes a Makefile for reproducible local development, verification, builds, and image creation:

```bash
# List all available targets.
make help

# Run formatting and dependency checks, go vet, race tests, and a CLI build.
make check

# Build bin/mysql-mcp with the version and commit detected from Git.
make build

# Print both values embedded in the binary.
./bin/mysql-mcp --version

# Override either value explicitly when needed.
make build VERSION=v1.0.0 COMMIT=0123456789abcdef

# Package one release target. TARGET_ARM is required for 32-bit ARM only.
make release-build VERSION=v1.0.0 COMMIT=0123456789abcdef TARGET_OS=linux TARGET_ARCH=amd64
make release-build VERSION=v1.0.0 COMMIT=0123456789abcdef TARGET_OS=linux TARGET_ARCH=arm TARGET_ARM=7

# Fuzz the SQL read-policy boundary (10 seconds by default).
make fuzz
make fuzz FUZZ_TIME=30s

# Build and tag the container image while embedding the same version and commit.
make docker-build \
  IMAGE=megaxcn/mysql-mcp \
  TAG=v1.0.0 \
  VERSION=v1.0.0 \
  COMMIT=0123456789abcdef
```

`VERSION` defaults to `git describe --tags --always --dirty` (the current tag or Git description), while `COMMIT` defaults to `git rev-parse HEAD`. Both can be overridden for `make build`, `make release-build`, and `make docker-build`; `mysql-mcp --version` reports the embedded version and commit together. Release packages take `VERSION` from `github.ref_name`, every published artifact and image takes `COMMIT` from `github.sha`, and branch Docker builds derive `VERSION` as `edge-<short-sha>`. Docker OCI revision metadata records the same full commit SHA. `release-build` writes its archive to `dist/` by default.

## Docker

The final image uses `scratch`, contains only the static `mysql-mcp` binary and the CA certificate bundle, and runs as the non-root UID/GID `65532:65532`. It does not embed a configuration file, database password, HTTP Token, or any other secret.

Pull the continuously updated development image or pin an exact release from Docker Hub:

```bash
docker pull megaxcn/mysql-mcp:edge
docker pull megaxcn/mysql-mcp:v1.0.0
```

Each published tag is a multi-architecture manifest supporting `linux/386`, `linux/amd64`, `linux/arm/v6`, `linux/arm/v7`, and `linux/arm64`. Docker automatically selects the matching image for the host platform.

Before starting an HTTP container, set the listener in the mounted configuration to `0.0.0.0:8080`; a loopback listener inside the container cannot receive traffic published by Docker:

```yaml
server:
  transport: http
  http:
    listen: "0.0.0.0:8080"
```

Mount the configuration read-only at `/etc/mysql-mcp/config.yaml` and inject referenced environment variables through an environment file:

```bash
docker run --rm \
  --read-only \
  --cap-drop=ALL \
  --security-opt=no-new-privileges \
  -p 127.0.0.1:8080:8080 \
  --env-file ./mysql-mcp.env \
  --mount type=bind,src="$(pwd)/config.yaml",dst=/etc/mysql-mcp/config.yaml,readonly \
  megaxcn/mysql-mcp:v1.0.0
```

Protect `mysql-mcp.env` as a secret and do not commit it. Environment variables or orchestrator-managed secrets are preferred. If `password_file`, `token_file`, a TLS private key, or another file-based secret is used, mount each file separately as read-only and ensure container UID `65532` can read it. Because the runtime is `scratch` and the root filesystem is read-only, it has no shell, package manager, or writable configuration location.

The image deliberately has no in-container `HEALTHCHECK` dependency. Configure Docker or the orchestrator to probe the unauthenticated liveness and readiness endpoints from outside the container:

```bash
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
```

`GET /healthz` is the liveness probe and returns `200 OK`. `GET /readyz` returns `200 OK` while the process is ready and `503 Service Unavailable` during shutdown. Neither endpoint requires a Token, and both are deliberately excluded from request logs to avoid continuous probe noise.

## Releases

Publishing is driven by [the Publish workflow](.github/workflows/release.yml). Every branch or tag selected for publishing must pass the standard verification and bounded SQL policy fuzz test before any artifact is uploaded.

Release tags must be `v`-prefixed SemVer. Prerelease identifiers are supported, but `+build` metadata is rejected because different SemVer values could otherwise collide when converted to Docker tags.

- A push to `master` or `main` publishes the moving Docker tag `megaxcn/mysql-mcp:edge`; it does not create a GitHub Release.
- A stable tag such as `v1.0.0` publishes only the exact Docker tags `v1.0.0` and `1.0.0`, then creates a GitHub Release with the packaged binaries and `SHA256SUMS`.
- A prerelease tag such as `v1.1.0-rc.1` likewise publishes only `v1.1.0-rc.1` and `1.1.0-rc.1`, then creates a prerelease GitHub Release.

There are deliberately no `latest`, major, or major/minor floating tags. This prevents a republished older release or concurrent publication from moving a shared tag backward. Published Docker version tags and GitHub Releases are immutable: the workflow refuses to overwrite an existing one, so a corrected or repeated release requires a new version number.

Release downloads are built with `CGO_ENABLED=0` for the following targets:

| Operating system | Architectures | Archive format |
|---|---|---|
| Linux | `386`, `amd64`, `armv6`, `armv7`, `arm64` | `.tar.gz` |
| Windows | `386`, `amd64`, `arm64` | `.zip` |
| macOS (`darwin`) | `amd64`, `arm64` | `.tar.gz` |

There are no macOS `386` or 32-bit ARM archives and no Windows 32-bit ARM archive because those targets are not supported by the current Go toolchain. Windows executables are not currently code-signed, and macOS executables are neither code-signed nor notarized, so SmartScreen or Gatekeeper may display a warning. Download artifacts only from this repository's GitHub Releases and verify their checksums before running them.

Download `SHA256SUMS` with the archive. To verify one archive without downloading every release target, filter its checksum entry and pass it to the platform checksum tool:

```bash
# Linux example
grep ' mysql-mcp_v1.0.0_linux-amd64.tar.gz$' SHA256SUMS | sha256sum --check -

# macOS example
grep ' mysql-mcp_v1.0.0_darwin-arm64.tar.gz$' SHA256SUMS | shasum -a 256 --check
```

On Windows, compare the following result with the matching line in `SHA256SUMS`:

```powershell
Get-FileHash .\mysql-mcp_v1.0.0_windows-amd64.zip -Algorithm SHA256
Select-String -Path .\SHA256SUMS -Pattern 'mysql-mcp_v1.0.0_windows-amd64.zip$'
```

### Maintainer setup

Before enabling publishing, configure the following under **Settings → Secrets and variables → Actions**:

- Repository variable `DOCKERHUB_USERNAME` with value `megaxcn`.
- Repository secret `DOCKERHUB_TOKEN` containing a scoped Docker Hub access token. Use an access token, never the Docker Hub account password.

After those values are configured, branch and release publication can be triggered with:

```bash
git push origin main # or master; publishes the edge image after verification
git tag -a v1.0.0 -m 'v1.0.0'
git push origin v1.0.0 # publishes the stable images and GitHub Release
```

Protect `master`/`main` and the `v*` tag namespace with repository rulesets. Restrict changes to publishing workflows, release tag creation, and access to Docker Hub credentials to trusted maintainers. Enable Docker Hub tag immutability for the repository when that feature is available.

## Quick Start

Copy the example configuration and prepare the environment variables it references:

```bash
cp config.example.yaml config.yaml
export MYSQL_MCP_HTTP_TOKEN='replace-with-a-sufficiently-long-random-value'
export ORDERS57_READ_PASSWORD='...'
export ORDERS57_MONITOR_PASSWORD='...'
export ANALYTICS8_READ_PASSWORD='...'
export ANALYTICS8_MONITOR_PASSWORD='...'
```

Validate the configuration before starting the server:

```bash
./bin/mysql-mcp validate-config --config ./config.yaml
./bin/mysql-mcp serve --config ./config.yaml
```

`validate-config` performs strict configuration validation and resolves secret references, so every referenced environment variable or secret file must exist. It never prints passwords or Tokens in its output.

The preferred database-password form is an environment reference in the `password` field:

```yaml
credentials:
  read:
    username: mysql_mcp_orders_read
    password: ${ORDERS57_READ_PASSWORD}
```

Only a complete scalar that is exactly `${ENV_NAME}`, with a valid environment-variable name, triggers environment lookup by `mysql-mcp`. It is not shell, YAML, or general-purpose template expansion. Startup and `validate-config` fail if that exact reference names a missing or empty variable. Every other non-empty scalar is used verbatim as a plaintext password; forms such as `prefix-${ENV_NAME}` or `${ENV_NAME:-default}` have no interpolation or default semantics and are literal password text.

Each database credential must configure exactly one of `password`, the compatible `password_env: ENV_NAME` form, or `password_file: path/to/secret`. Relative secret-file paths are resolved from the configuration file's directory. File-based secrets remain useful when a container or orchestrator mounts a secret as a read-only file; do not configure a second password source alongside them. Plaintext `password` values are supported but should never be committed to Git; resolved passwords are redacted from configuration diagnostics.

The example configuration defines two data sources, `orders57` and `analytics8`, with the following HTTP endpoints:

```text
http://127.0.0.1:8080/orders57/mcp
http://127.0.0.1:8080/analytics8/mcp
```

There is no generic `/mcp` endpoint that can dispatch across data sources. Once a client calls one of these paths, the MCP session and all of its tools remain bound to that data source.

When Token authentication is enabled, send the following header with every MCP HTTP request:

```http
Authorization: Bearer <value of MYSQL_MCP_HTTP_TOKEN>
```

`GET /healthz` and `GET /readyz` return only process liveness/readiness status and expose no database metadata, so they do not require a Token.

## stdio Mode

Set `server.transport` to `stdio`. With multiple data sources configured, stdio must be explicitly bound to one data source using `--datasource`:

```bash
./bin/mysql-mcp serve --config ./config.yaml --datasource orders57
```

Even when the configuration contains only one data source, passing this argument explicitly is recommended to make the startup command auditable. In stdio mode, standard output is reserved exclusively for MCP protocol frames, while logs are written to standard error.

At stdio startup, configuration is narrowed to the selected data source before database secrets are resolved. Consequently, another data source being temporarily unavailable or missing injected secrets does not block the session; `validate-config` still resolves and validates secrets for every data source.

Example MCP client configuration:

```json
{
  "mcpServers": {
    "orders57": {
      "command": "/absolute/path/to/mysql-mcp",
      "args": [
        "serve",
        "--config",
        "/absolute/path/to/config.yaml",
        "--datasource",
        "orders57"
      ],
      "env": {
        "ORDERS57_READ_PASSWORD": "injected by the client's secret management capability"
      }
    }
  }
}
```

stdio relies on the local trust boundary of the process that starts it and does not use an HTTP Bearer Token. A stdio configuration that retains `server.http.auth.mode: token` is rejected; set it to `none`.

## HTTP Authentication

Unauthenticated mode is suitable for loopback addresses, trusted sidecars, or deployment behind a reverse proxy that has already authenticated the client:

```yaml
server:
  http:
    listen: "127.0.0.1:8080"
    auth:
      mode: none
```

Token mode requires exactly one of `token_env` or `token_file`; a Token cannot be written directly in plaintext YAML:

```yaml
server:
  http:
    listen: "127.0.0.1:8080"
    auth:
      mode: token
      token_env: MYSQL_MCP_HTTP_TOKEN
```

Or:

```yaml
server:
  http:
    auth:
      mode: token
      token_file: secrets/http-token
```

Relative secret file paths are resolved from the directory containing the configuration file. Tokens are compared in constant time, and every authentication failure returns `401 Unauthorized`. A Token is a shared static credential; it contains no user identity, role, or fine-grained authorization. In production, terminate TLS at an HTTPS reverse proxy, restrict source networks, and rotate Tokens regularly.

> `datasources[].tls` protects only the connection from the server to MySQL; it does not enable HTTPS on the MCP HTTP listener.

## HTTP Health Checks and Request Logging

`GET /healthz` is the liveness endpoint and returns `200 OK`. `GET /readyz` is the readiness endpoint: it returns `200 OK` while the service accepts work and `503 Service Unavailable` during shutdown. Both routes bypass Token authentication. Health and readiness requests are intentionally not logged, which keeps frequent orchestrator probes from overwhelming operational logs.

Every other completed HTTP request produces a structured log entry containing only `method`, `path`, `status`, `response_bytes`, `duration_ms`, `remote_addr`, and `request_id`. The response carries the corresponding identifier in `X-Request-ID` for correlation.

The request logger does not record the query string, `Authorization` value or Token, any request or response headers, the body, or SQL. `remote_addr` is Go's direct peer address. Behind a reverse proxy it therefore identifies the proxy, not the original client; the service deliberately does not trust `X-Forwarded-For` or similar forwarded-address headers.

## Read-only and Write Capabilities

The default configuration is equivalent to:

```yaml
server:
  read_only: true
  features:
    dml: false
    ddl: false
    admin: false
    function_write: false
```

`server.read_only: true` is mutually exclusive with every write feature gate. If writes are genuinely required, assess the risks first and then configure them explicitly:

```yaml
server:
  read_only: false
  features:
    dml: true
    ddl: false
    admin: false
    function_write: false
```

A write operation also requires the target data source to meet all of the following conditions:

1. `datasources[].read_only` is `false`; a data source may only tighten the global read-only policy, never override it.
2. `credentials.write` is configured.
3. The corresponding feature gate is enabled: DML, DDL, administrative operations, and stored functions with write side effects do not implicitly authorize one another.
4. SQL AST classification matches the target tool; for example, `mysql.execute` does not accept transaction control, session statements, or arbitrary administrative SQL.
5. The MySQL write account itself has the required minimum permissions.

Separate processes by environment and purpose: use a fully read-only instance for query-oriented Agents, while automation that must change data should use a separate instance, Token, account, and stricter network policy.

## MCP Tools

Whether a tool appears in `tools/list` depends on the read-only policy, feature gates, monitoring settings, and function allowlist. The current adapter uses the following names:

| Tool | Purpose | Availability |
|---|---|---|
| `mysql.query` | Executes one read-only `SELECT`/`UNION`; parameters are bound through driver placeholders | Always available |
| `mysql.explain` | Runs safe `EXPLAIN` on a query that has passed the read-only policy | Always available |
| `mysql.execute` | Executes one classified DML or DDL statement; the result identifies the transaction mode | Requires `features.dml` or `features.ddl` respectively, and a non-read-only data source |
| `mysql.metadata.schemas` | Lists visible schemas | Always available |
| `mysql.metadata.tables` | Lists tables/views in a schema | Always available |
| `mysql.metadata.describe_table` | Returns table and column information | Always available |
| `mysql.metadata.indexes` | Returns index information | Always available |
| `mysql.metadata.constraints` | Returns constraint and foreign key information | Always available |
| `mysql.monitor.overview` | Version, host, read-only status, connection counts, and other overview data | `monitoring.enabled` |
| `mysql.monitor.storage` | Aggregates tables, data, and index space by schema | `monitoring.enabled` |
| `mysql.monitor.sessions` | Shows sessions and their current statements | `monitoring.sessions` |
| `mysql.monitor.locks` | Shows waiting and blocking relationships | `monitoring.locks` |
| `mysql.monitor.top_queries` | Shows Performance Schema digest aggregates | `monitoring.top_queries`, with Performance Schema enabled |
| `mysql.monitor.replication` | Shows replication status | `monitoring.replication` |
| `mysql.monitor.innodb_status` | Shows a restricted InnoDB status view | `monitoring.innodb_status` |
| `mysql.function.list` | Lists only allowlisted stored functions that exist | `functions` is configured |
| `mysql.function.describe` | Shows an allowlisted function's signature and security properties | Function is in the allowlist |
| `mysql.function.call` | Calls a function through a server-generated `SELECT schema.function(?, ...)` | Function is in the allowlist; write functions also require `features.function_write` and a non-read-only data source |
| `mysql.admin.kill_query` | Cancels only the currently executing statement on a specified connection, without disconnecting it | `features.admin` and a non-read-only data source |

`mysql.monitor.*` does not accept SQL text; these tools only run built-in, version-adapted server queries. `mysql.admin.kill_query` accepts only a positive integer connection ID and does not provide an arbitrary administrative SQL entry point.

## Stored Functions

Stored functions must be added to the allowlist with schema-qualified names:

```yaml
functions:
  - name: orders.calculate_discount
    effect: read
    allow_definer: false
```

- `effect: read` uses the read connection pool and a read-only transaction; the call is rejected if the database declares the function as `MODIFIES SQL DATA`.
- `effect: write` uses the write connection pool and additionally requires `server.features.function_write: true`.
- `effect` is mandatory and never defaults to `read`; it is the deployer's explicit security declaration about the function's side effects.
- `allow_definer: false` rejects `SQL SECURITY DEFINER` functions to avoid borrowing the definer's privileges. Set it to `true` only after auditing each function body and DEFINER.
- Arguments accept scalar values only, are bound with `?`, and cannot contain SQL expressions.
- Only stored functions listed in `INFORMATION_SCHEMA.ROUTINES` are supported; stored procedures and loadable/native UDFs are not supported.
- Raw `mysql.query` rejects schema-qualified or unknown functions. Stored functions must be called through `mysql.function.call`.

MySQL's `SQL DATA ACCESS` declaration is primarily metadata and does not replace the application allowlist, account permissions, or code review.

## Multiple Data Sources and MySQL Versions

Each `datasources[]` entry independently configures:

- URL name, network address, and default database;
- literal and Glob-pattern schema allowlists;
- read/write/monitor credentials;
- TLS and connection pools;
- monitoring capabilities;
- stored function allowlist.

Data source names must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`, making them safe for use as a single URL path segment. Names must be unique within a configuration.

At startup, the server reads the actual MySQL version, accepts only 5.7.x and 8.x, and creates the corresponding Vitess parser version for each data source. Monitoring queries account for major version differences, including MySQL 5.7's `INFORMATION_SCHEMA.INNODB_LOCK_WAITS`, MySQL 8.x Performance Schema data locks, and the `SHOW REPLICA STATUS` terminology used by newer 8.x releases.

## Schema Allowlist

A data source can combine exact schema names with Glob patterns:

```yaml
default_database: orders_dev
allowed_schemas:
  - shared_reference
allowed_schema_patterns:
  - "*_dev"
```

`allowed_schemas` contains literal, complete schema names. Each `allowed_schema_patterns` entry must contain at least one `*` and is anchored to the complete schema name: only `*` is special and it matches any number of characters, including zero; every other character is literal. Put entries without a wildcard in `allowed_schemas`. Both forms are case-sensitive. A schema is allowed when it matches either list, so the example permits both `shared_reference` and names such as `orders_dev`. When both lists are empty, the application imposes no additional schema restriction.

When either list is non-empty, an unqualified physical table is attributed to `default_database`, and that database must satisfy the same combined rules. Configure an allowed default database if clients use unqualified table names; without one, those references cannot be evaluated safely.

The allowlist applies to direct physical table references visible in the request AST. It does not discover the underlying dependencies of views, and it does not change the existing instance-level boundary of monitoring tools. MySQL account `GRANT` permissions remain the final database authorization boundary: matching a configured name or pattern never grants access that the selected MySQL account does not already have.

## TLS Modes

| Mode | Encryption | Server identity verification | Description |
|---|---:|---:|---|
| `disabled` | No | No | Suitable only for a controlled local or isolated network |
| `preferred` | Preferred | No | Allows downgrade to plaintext when TLS is unavailable |
| `required` | Yes | No | Prevents eavesdropping but does not verify certificate identity |
| `verify-ca` | Yes | Verifies certificate chain | Does not verify the hostname |
| `verify-full` | Yes | Verifies certificate chain and hostname | Recommended for production |

Relative `ca_file`, `cert_file`, and `key_file` paths are resolved from the configuration file's directory; the client certificate and private key must be configured together. For a TCP address without `server_name`, the name is derived from the hostname. Set it explicitly when using a Unix Socket or an address whose name does not match the certificate.

## Configuration Reference

See [config.example.yaml](config.example.yaml) for a complete, commented configuration. Important rules include:

- `version` must currently be `1`.
- YAML is parsed strictly: unknown fields, duplicate keys, merge keys, and multiple YAML documents are rejected.
- Every database credential requires exactly one password source. Prefer `password: ${ENV_NAME}`; only that exact whole-value form reads the environment, and a missing or empty referenced variable fails validation. Other non-empty `password` scalars are supported as literal plaintext, without interpolation or default semantics, but should never be committed to Git. `password_env` and `password_file` remain compatible alternatives. HTTP Tokens continue to use exactly one of `token_env` or `token_file`.
- Tokens must conform to the RFC 6750 `b64token` character set. Leading, trailing, or embedded whitespace, control characters, and values that cannot be placed in a Bearer Header are rejected before startup.
- `allowed_schemas` and `allowed_schema_patterns` follow the combined, anchored, case-sensitive rules described in [Schema Allowlist](#schema-allowlist). Whether MySQL database names are case-sensitive depends on the server platform; the application always preserves exact case to avoid authorizing a different database on a case-sensitive host.
- `query_timeout` uses Go duration syntax, such as `500ms`, `10s`, or `2m`.
- Sizes accept integer bytes or `KiB`, `MiB`, `GiB`, `KB`, `MB`, and `GB` suffixes.
- Integers and DECIMAL values in results are represented as strings to avoid JSON/JavaScript precision loss; binary data is Base64-encoded.
- Request integers outside JavaScript's exactly representable range of `±(2^53-1)` must be passed as decimal strings; an out-of-range JSON number is rejected before reaching MySQL.

## Security Boundary

Read-only classification is not based on whether SQL starts with `SELECT`. The server first rejects multiple statements, executable comments, and optimizer hints, then validates the root node and every child node using the Vitess AST. Read-only queries reject:

- a CTE whose final operation is `INSERT`, `UPDATE`, or `DELETE`;
- `SELECT ... INTO`;
- locking reads such as `FOR UPDATE` and `LOCK IN SHARE MODE`;
- user-variable assignments, sequence advancement, and transaction/session state changes;
- dangerous functions such as `SLEEP`, `BENCHMARK`, `LOAD_FILE`, advisory locks, and replication waits;
- functions outside the safe built-in function set, as well as any schema-qualified stored function.

After AST validation, queries still execute in database read-only transactions and remain constrained by a least-privilege account, schema allowlist, timeout, row count, column count, cell size, result size, and concurrency limits. Row and result byte limits are applied after driver decoding, so MySQL-side `max_allowed_packet` and resource limits should also be configured. Multi-statements, parameter interpolation, and local file loading are always disabled at the driver layer.

Every connection uses a fixed parser-compatible `sql_mode`: `ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION,IGNORE_SPACE`. This prevents an inherited server mode such as `ANSI` from re-enabling `ANSI_QUOTES`, `PIPES_AS_CONCAT`, or other lexical behavior that differs from Vitess. If application SQL depends on an excluded mode, rewrite the SQL instead of weakening this security constraint.

The DML path in `mysql.execute` explicitly begins and commits a transaction; actual rollback capability still depends on whether the target tables use a transactional storage engine. Most DDL in MySQL 5.7/8.x causes an implicit commit, so DDL is not wrapped in a misleading transaction. Successful DDL results use `execution_mode: mysql_implicit_commit` and must not be assumed to be rollback-capable.

For a more complete threat model, see [docs/security.md](docs/security.md). For account grant recommendations, see [docs/mysql-privileges.md](docs/mysql-privileges.md).

## Operational Recommendations

- Keep `read_only: true` by default, enabling only the individual write gate required for a clearly defined use case.
- When exposing HTTP beyond a loopback address, place it behind an HTTPS authentication proxy or strict network policy; `mode: none` produces a security warning.
- Use separate accounts for every data source and role, avoiding `GRANT OPTION`, `FILE`, and unnecessary global privileges.
- Use table-level `GRANT` statements in strictly isolated environments. DEFINER views and stored functions are database code that must be audited separately; their indirect access cannot be inferred solely from the request AST's schema/function allowlists.
- Monitoring tools provide instance-level operational visibility and may return schema names outside the allowlist, SQL text, locks, and replication information. Do not expose a high-privilege monitoring account to a tenant-level Token.
- Inject Tokens and database passwords through a secret manager rather than committing them to Git; secret files should generally have `0600` permissions.
- Establish external auditing and approval workflows for `mysql.execute`, write-function calls, and `mysql.admin.kill_query`.
- Validate MySQL versions, Performance Schema, replication topology, and required privileges in a pre-production environment first.

## Testing

```bash
go test ./...
go test -race ./...
```

Unit tests cover strict configuration parsing, secret resolution and redaction, SQL AST policies, multi-data-source version capabilities, connection parameters, result boundaries, metadata/monitoring/function/administrative services, HTTP routing, and authentication. Tests do not require a live MySQL connection; integration testing against both MySQL 5.7 and 8.0 is still recommended before production deployment.
