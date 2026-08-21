# mysql-mcp

[English](README.md) | 简体中文

[![CI](https://github.com/MegaXChan/mysql-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/MegaXChan/mysql-mcp/actions/workflows/ci.yml)
[![GitHub Release](https://img.shields.io/github/v/release/MegaXChan/mysql-mcp?display_name=tag&sort=semver)](https://github.com/MegaXChan/mysql-mcp/releases/latest)

`mysql-mcp` 是一个使用 Go 编写的 MySQL Model Context Protocol（MCP）服务端。它支持 MySQL 5.7、MySQL 8.x、多数据源、stdio 与 Streamable HTTP，并将查询、元数据、监控、存储函数和少量管理操作拆分为边界明确的工具。

项目的安全默认值是“只读”：写操作必须同时通过全局功能开关、数据源只读策略、SQL AST 校验和 MySQL 账号权限。

## 主要能力

- 使用 `github.com/urfave/cli/v3` 提供 `serve`、`validate-config` 等 CLI 命令。
- 一个进程可连接多个 MySQL 5.7/8.x 数据源，并为读、写、监控分别维护连接池。
- HTTP 模式中，每个端点固定为 `/{datasource_name}/mcp`；工具参数不能切换数据源。
- HTTP 支持无认证和静态 Bearer Token 认证。
- SQL 使用 `vitess.io/vitess/go/vt/sqlparser` 构建 AST 后分类，不通过字符串前缀判断 `SELECT`。
- 支持只读查询、DML、DDL、元数据、固定监控查询、存储函数 allowlist，以及类型化的 `KILL QUERY`。
- 配置文件严格校验未知字段、重复 YAML key、非法组合和密钥引用。
- 查询时长、SQL 大小、行数、结果大小和每数据源并发量都有上限。

## 环境要求与构建

- Go 1.26.4
- MySQL 5.7 或 MySQL 8.x

```bash
make build
make test
make test-race
```

GitHub Actions CI 流水线会在每次 push、Pull Request 和手动触发时，从 `go.mod` 读取准确的 Go 版本，并执行依赖校验、`gofmt`、`go vet`、race 测试、覆盖率、CLI 构建、有界 SQL 策略 fuzz 测试，以及容器镜像的构建与冒烟测试。CI 不需要数据库密码或其他仓库 Secret。独立的发布流水线会先重复执行必要校验，再从符合条件的分支和标签制作 Docker 镜像与 GitHub Release 制品。

## Makefile

仓库提供 Makefile，用于可复现的本地开发、校验、构建和镜像制作：

```bash
# 列出全部可用目标。
make help

# 执行格式与依赖检查、go vet、race 测试和 CLI 构建。
make check

# 构建 bin/mysql-mcp，并从 Git 自动获取版本和提交。
make build

# 同时显示嵌入二进制文件的版本和提交。
./bin/mysql-mcp --version

# 需要时可显式覆盖任一值。
make build VERSION=v1.0.0 COMMIT=0123456789abcdef

# 打包一个发布目标。仅 32 位 ARM 需要 TARGET_ARM。
make release-build VERSION=v1.0.0 COMMIT=0123456789abcdef TARGET_OS=linux TARGET_ARCH=amd64
make release-build VERSION=v1.0.0 COMMIT=0123456789abcdef TARGET_OS=linux TARGET_ARCH=arm TARGET_ARM=7

# 对 SQL 只读策略边界执行 fuzz 测试（默认 10 秒）。
make fuzz
make fuzz FUZZ_TIME=30s

# 构建并标记容器镜像，同时嵌入相同的版本和提交。
make docker-build \
  IMAGE=megaxcn/mysql-mcp \
  TAG=v1.0.0 \
  VERSION=v1.0.0 \
  COMMIT=0123456789abcdef
```

`VERSION` 默认取自 `git describe --tags --always --dirty`（当前 tag 或 Git 描述），`COMMIT` 默认取自 `git rev-parse HEAD`。`make build`、`make release-build` 和 `make docker-build` 均可显式覆盖这两个值；`mysql-mcp --version` 会同时显示嵌入的版本和提交。Release 压缩包从 `github.ref_name` 获取 `VERSION`，所有发布制品和镜像从 `github.sha` 获取 `COMMIT`，分支 Docker 构建则使用 `edge-<short-sha>` 作为 `VERSION`。Docker 的 OCI revision 元数据记录相同的完整 commit SHA。`release-build` 默认将压缩包写入 `dist/`。

## Docker

最终镜像使用 `scratch`，仅包含静态 `mysql-mcp` 二进制文件和 CA 证书包，并以非 root UID/GID `65532:65532` 运行。镜像不会内置配置文件、数据库密码、HTTP Token 或任何其他 secret。

Docker Hub 提供三类面向用户的标签：

| 标签 | 含义 |
|---|---|
| `megaxcn/mysql-mcp:latest` | 最新的稳定 SemVer 版本 |
| `megaxcn/mysql-mcp:edge` | 从 `master` 或 `main` 持续更新的开发镜像 |
| `megaxcn/mysql-mcp:vX.Y.Z` | 不可变的精确版本；可复现部署推荐使用 |

通常直接使用 `latest` 获取最新稳定版；只有明确需要持续更新的开发构建时才使用 `edge`：

```bash
docker pull megaxcn/mysql-mcp:latest
docker pull megaxcn/mysql-mcp:edge
```

如需快速检查版本，`--pull=always` 会让 Docker 在启动容器前刷新这个浮动标签：

```bash
docker run --pull=always --rm megaxcn/mysql-mcp:latest --version
docker run --pull=always --rm megaxcn/mysql-mcp:edge --version
```

如需可复现部署或回滚，请把 `latest` 替换为 GitHub Releases 页面显示的精确标签，例如 `vX.Y.Z`。

每个已发布标签都是多架构 manifest，支持 `linux/386`、`linux/amd64`、`linux/arm/v6`、`linux/arm/v7` 和 `linux/arm64`。Docker 会自动选择与宿主机平台匹配的镜像。

以 HTTP 容器方式启动前，必须将挂载配置中的监听地址设为 `0.0.0.0:8080`；容器内的回环监听无法接收 Docker 发布端口的流量：

```yaml
server:
  transport: http
  http:
    listen: "0.0.0.0:8080"
```

将配置只读挂载到 `/etc/mysql-mcp/config.yaml`，并通过环境变量文件注入配置所引用的变量：

```bash
docker run --rm \
  --pull=always \
  --read-only \
  --cap-drop=ALL \
  --security-opt=no-new-privileges \
  -p 127.0.0.1:8080:8080 \
  --env-file ./mysql-mcp.env \
  --mount type=bind,src="$(pwd)/config.yaml",dst=/etc/mysql-mcp/config.yaml,readonly \
  megaxcn/mysql-mcp:latest
```

上面的命令会跟随最新稳定版；如果部署不能自动升级，请把 `latest` 替换为明确的 `vX.Y.Z` 标签。

应将 `mysql-mcp.env` 作为 secret 保护且不得提交。优先使用环境变量或编排器托管的 secrets。若使用 `password_file`、`token_file`、TLS 私钥或其他文件型 secret，应将每个文件分别只读挂载，并确保容器 UID `65532` 对其具有读取权限。由于运行时为 `scratch` 且根文件系统只读，容器内没有 shell、包管理器或可写配置目录。

镜像有意不依赖容器内的 `HEALTHCHECK`。请配置 Docker 或编排器从容器外探测无需认证的存活与就绪端点：

```bash
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
```

`GET /healthz` 是存活探针，返回 `200 OK`。`GET /readyz` 在进程就绪时返回 `200 OK`，关闭过程中返回 `503 Service Unavailable`。两个端点都不要求 Token，并且会被故意排除在请求日志之外，避免持续探针产生噪声。

## 版本发布

发布由 [Publish 工作流](.github/workflows/release.yml)驱动。所有进入发布流程的分支或标签都必须先通过标准校验及有界 SQL 策略 fuzz 测试，才会上传任何制品。

版本标签必须使用带 `v` 前缀的 SemVer。支持预发布标识，但拒绝 `+build` 元数据，因为不同 SemVer 值转换为 Docker 标签时可能发生碰撞。

- 推送到 `master` 或 `main` 会发布滚动更新的 Docker 标签 `megaxcn/mysql-mcp:edge`，但不会创建 GitHub Release。
- 推送 `v1.0.0` 这样的稳定标签会发布 `v1.0.0` 和 `1.0.0` 两个精确 Docker 标签，创建包含已打包二进制文件及 `SHA256SUMS` 的 GitHub Release，然后根据全部已发布 GitHub Releases 中的最高稳定 SemVer 校准 `latest`。
- 推送 `v1.1.0-rc.1` 这样的预发布标签只发布 `v1.1.0-rc.1` 和 `1.1.0-rc.1` 两个精确标签，然后创建预发布 GitHub Release；预发布绝不更新 `latest`。

`latest` 始终表示发布流程已知的最高稳定 SemVer 版本。补发或重跑较旧的稳定版本不会使其回退；即使 `latest` 被删除，任意稳定版发布任务也会根据全局最高且已验证的 Release 重建它。项目不发布主版本或主/次版本浮动标签。精确 Docker 版本标签和 GitHub Release 资产均不可变：完全相同的重跑会在验证后通过，而替换或修正已有制品必须使用新版本号；`edge` 与 `latest` 是有意设计为可移动的别名。

如果 Runner 在 GitHub 仍保留不完整 Draft Release 时中断，请先检查并删除该草稿，再重跑同一标签。工作流会有意拒绝覆盖远端的部分资产，也不会猜测如何修复它们。

发布下载使用 `CGO_ENABLED=0` 构建，覆盖以下目标：

| 操作系统 | 架构 | 压缩格式 |
|---|---|---|
| Linux | `386`、`amd64`、`armv6`、`armv7`、`arm64` | `.tar.gz` |
| Windows | `386`、`amd64`、`arm64` | `.zip` |
| macOS（`darwin`） | `amd64`、`arm64` | `.tar.gz` |

macOS 不提供 `386` 或 32 位 ARM 压缩包，Windows 不提供 32 位 ARM 压缩包，因为当前 Go 工具链不支持这些目标。Windows 可执行文件目前没有代码签名，macOS 可执行文件目前既没有代码签名也没有公证，因此 SmartScreen 或 Gatekeeper 可能显示警告。请仅从本仓库的 GitHub Releases 下载制品，并在运行前校验其 checksum。

请将 `SHA256SUMS` 与压缩包一起下载。若只下载一个目标，可筛选对应 checksum 条目并交给平台校验工具，无需下载全部发布制品：

```bash
# Linux 示例
grep ' mysql-mcp_v1.0.0_linux-amd64.tar.gz$' SHA256SUMS | sha256sum --check -

# macOS 示例
grep ' mysql-mcp_v1.0.0_darwin-arm64.tar.gz$' SHA256SUMS | shasum -a 256 --check
```

Windows 上可将下列结果与 `SHA256SUMS` 中对应行进行比较：

```powershell
Get-FileHash .\mysql-mcp_v1.0.0_windows-amd64.zip -Algorithm SHA256
Select-String -Path .\SHA256SUMS -Pattern 'mysql-mcp_v1.0.0_windows-amd64.zip$'
```

### 维护者设置

启用发布前，请在 **Settings → Secrets and variables → Actions** 中配置：

- Repository variable `DOCKERHUB_USERNAME`，值为 `megaxcn`。
- Repository secret `DOCKERHUB_TOKEN`，内容为限制作用域的 Docker Hub access token。必须使用 access token，不能使用 Docker Hub 账户密码。

完成配置后，可通过以下命令触发分支和版本发布：

```bash
git push origin main # 也可以是 master；校验通过后发布 edge 镜像
git tag -a v1.0.0 -m 'v1.0.0'
git push origin v1.0.0 # 发布稳定镜像与 GitHub Release
```

请使用仓库 ruleset 保护 `master`/`main` 及 `v*` 标签命名空间，并仅允许可信维护者修改发布流水线、创建发布标签及访问 Docker Hub 凭据。若 Docker Hub 支持 tag immutability 规则，只应对精确 SemVer 标签启用；`latest` 与 `edge` 必须保持可变。

## 快速开始

复制示例配置并准备其中引用的环境变量：

```bash
cp config.example.yaml config.yaml
export MYSQL_MCP_HTTP_TOKEN='请替换为足够长的随机值'
export ORDERS57_READ_PASSWORD='...'
export ORDERS57_MONITOR_PASSWORD='...'
export ANALYTICS8_READ_PASSWORD='...'
export ANALYTICS8_MONITOR_PASSWORD='...'
```

先校验配置，再启动服务：

```bash
./bin/mysql-mcp validate-config --config ./config.yaml
./bin/mysql-mcp serve --config ./config.yaml
```

`validate-config` 会执行严格配置校验并解析密钥引用，因此被引用的环境变量或密钥文件也必须存在。它不会在输出中打印密码或 Token。

数据库密码首选在 `password` 字段中使用环境变量引用：

```yaml
credentials:
  read:
    username: mysql_mcp_orders_read
    password: ${ORDERS57_READ_PASSWORD}
```

只有整个标量严格等于 `${ENV_NAME}`，且 `ENV_NAME` 是合法的环境变量名时，`mysql-mcp` 才会读取环境变量。这不是 shell、YAML 或通用模板展开；精确引用的变量缺失或为空时，启动和 `validate-config` 都会失败。其他任意非空 scalar 均作为明文密码原样使用，`prefix-${ENV_NAME}`、`${ENV_NAME:-default}` 等形式没有拼接或 default 语义，只是密码字面值。

每个数据库凭据必须在 `password`、兼容写法 `password_env: ENV_NAME`、`password_file: path/to/secret` 中三选一。相对 secret 文件路径以配置文件目录为基准。容器或编排器把 secret 挂载为只读文件时仍可使用文件写法，但不能同时配置第二种密码来源。明文 `password` 虽受支持，但绝不应提交到 Git；配置诊断会对解析后的密码脱敏。

示例配置定义了 `orders57` 和 `analytics8` 两个数据源，HTTP 端点分别为：

```text
http://127.0.0.1:8080/orders57/mcp
http://127.0.0.1:8080/analytics8/mcp
```

不存在可跨数据源调度的通用 `/mcp` 端点。客户端调用某一路径后，该 MCP 会话及其全部工具都固定绑定到该数据源。

启用 Token 认证时，请在每个 MCP HTTP 请求中发送：

```http
Authorization: Bearer <MYSQL_MCP_HTTP_TOKEN 的值>
```

`GET /healthz` 和 `GET /readyz` 只返回进程存活/就绪状态，不暴露数据库元数据，因此不要求 Token。

## stdio 模式

将 `server.transport` 改为 `stdio`。多数据源配置下，stdio 必须通过 `--datasource` 明确绑定一个数据源：

```bash
./bin/mysql-mcp serve --config ./config.yaml --datasource orders57
```

即使配置文件中只有一个数据源，也建议显式传入该参数，便于审计启动命令。stdio 的标准输出只用于 MCP 协议帧，日志写入标准错误。

stdio 启动时会在解析数据库密钥前缩减到选中的数据源，因此其他数据源暂时不可用或未注入密钥不会阻断该会话；`validate-config` 仍会解析并校验全部数据源密钥。

MCP 客户端配置示例：

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
        "ORDERS57_READ_PASSWORD": "由客户端的密钥管理能力注入"
      }
    }
  }
}
```

stdio 依赖启动进程的本地信任边界，不使用 HTTP Bearer Token；stdio 配置若保留 `server.http.auth.mode: token` 会被拒绝，请设为 `none`。

## HTTP 认证

无认证模式适合回环地址、受信任的 sidecar 或已经完成身份认证的反向代理之后：

```yaml
server:
  http:
    listen: "127.0.0.1:8080"
    auth:
      mode: none
```

Token 模式要求 `token_env` 和 `token_file` 二选一，不能把 Token 明文直接写进 YAML：

```yaml
server:
  http:
    listen: "127.0.0.1:8080"
    auth:
      mode: token
      token_env: MYSQL_MCP_HTTP_TOKEN
```

或：

```yaml
server:
  http:
    auth:
      mode: token
      token_file: secrets/http-token
```

相对密钥文件路径以配置文件所在目录为基准。Token 比较使用常量时间比较，认证失败统一返回 `401 Unauthorized`。Token 是共享静态凭据，不包含用户身份、角色或细粒度授权；生产环境应使用 HTTPS 反向代理终止 TLS、限制来源网络并定期轮换 Token。

> `datasources[].tls` 只保护服务端到 MySQL 的连接，不会为 MCP HTTP 监听器启用 HTTPS。

## HTTP 健康检查与请求日志

`GET /healthz` 是存活端点，返回 `200 OK`。`GET /readyz` 是就绪端点：服务可接受请求时返回 `200 OK`，关闭过程中返回 `503 Service Unavailable`。两个路由均绕过 Token 认证。健康和就绪请求被有意设为不产生日志，避免编排器的高频探针淹没运维日志。

其他每个已完成的 HTTP 请求都会产生一条结构化日志，且只包含 `method`、`path`、`status`、`response_bytes`、`duration_ms`、`remote_addr` 和 `request_id`。响应通过 `X-Request-ID` 返回对应标识，便于关联排查。

请求日志不会记录 query string、`Authorization` 值或 Token、任何请求或响应 Header、body、SQL。`remote_addr` 是 Go 看到的直接对端地址；位于反向代理之后时，它表示代理而不是原始客户端。服务有意不信任 `X-Forwarded-For` 或其他转发地址 Header。

## 只读与写能力

默认配置等价于：

```yaml
server:
  read_only: true
  features:
    dml: false
    ddl: false
    admin: false
    function_write: false
```

`server.read_only: true` 与任意写功能开关互斥。若确实需要写入，应先评估风险，然后显式设置：

```yaml
server:
  read_only: false
  features:
    dml: true
    ddl: false
    admin: false
    function_write: false
```

写操作还要求目标数据源满足以下条件：

1. `datasources[].read_only` 为 `false`；数据源只能收紧，不能覆盖全局只读策略。
2. 配置了 `credentials.write`。
3. 对应功能开关已开启：DML、DDL、管理操作和有写副作用的存储函数互不隐式授权。
4. SQL AST 分类符合目标工具；例如 `mysql.execute` 不接受事务控制、会话语句或任意管理 SQL。
5. MySQL 写账号本身具有所需的最小权限。

建议按环境拆分进程：查询型 Agent 使用完全只读的实例，需要变更数据的自动化使用独立实例、独立 Token、独立账号和更严格的网络策略。

## MCP 工具

工具是否出现在 `tools/list` 中取决于只读策略、功能开关、监控开关及函数 allowlist。当前适配层采用以下命名：

| 工具 | 用途 | 启用条件 |
|---|---|---|
| `mysql.query` | 执行一个只读 `SELECT`/`UNION`，参数通过驱动占位符绑定 | 始终可用 |
| `mysql.explain` | 对已通过只读策略的查询执行安全 `EXPLAIN` | 始终可用 |
| `mysql.execute` | 执行一个已分类的 DML 或 DDL 语句；结果会标明事务模式 | 分别需要 `features.dml` 或 `features.ddl`，且数据源非只读 |
| `mysql.metadata.schemas` | 列出可见 schema | 始终可用 |
| `mysql.metadata.tables` | 列出 schema 中的表/视图 | 始终可用 |
| `mysql.metadata.describe_table` | 返回表与列信息 | 始终可用 |
| `mysql.metadata.indexes` | 返回索引信息 | 始终可用 |
| `mysql.metadata.constraints` | 返回约束和外键信息 | 始终可用 |
| `mysql.monitor.overview` | 版本、主机、只读状态、连接数等概览 | `monitoring.enabled` |
| `mysql.monitor.storage` | 按 schema 汇总表、数据与索引空间 | `monitoring.enabled` |
| `mysql.monitor.sessions` | 查看会话和当前语句 | `monitoring.sessions` |
| `mysql.monitor.locks` | 查看等待与阻塞关系 | `monitoring.locks` |
| `mysql.monitor.top_queries` | 查看 Performance Schema digest 聚合 | `monitoring.top_queries`，且 Performance Schema 已启用 |
| `mysql.monitor.replication` | 查看复制状态 | `monitoring.replication` |
| `mysql.monitor.innodb_status` | 查看受限的 InnoDB 状态 | `monitoring.innodb_status` |
| `mysql.function.list` | 仅列出 allowlist 中存在的存储函数 | 配置了 `functions` |
| `mysql.function.describe` | 查看 allowlist 函数签名与安全属性 | 函数在 allowlist 中 |
| `mysql.function.call` | 使用服务端生成的 `SELECT schema.function(?, ...)` 调用函数 | 函数在 allowlist 中；写函数还需 `features.function_write` 和非只读数据源 |
| `mysql.admin.kill_query` | 仅取消指定连接上正在执行的语句，不断开连接 | `features.admin`，且数据源非只读 |

`mysql.monitor.*` 不接受 SQL 文本，只执行服务端内置、版本适配的查询。`mysql.admin.kill_query` 只接受正整数连接 ID，不提供“任意管理 SQL”入口。

## 存储函数

存储函数必须使用 schema 限定名加入 allowlist：

```yaml
functions:
  - name: orders.calculate_discount
    effect: read
    allow_definer: false
```

- `effect: read` 使用读连接池和只读事务；若数据库声明为 `MODIFIES SQL DATA`，调用会被拒绝。
- `effect: write` 使用写连接池，并额外要求 `server.features.function_write: true`。
- `effect` 必须显式填写，不会默认为 `read`；这是部署方对函数副作用作出的安全声明。
- `allow_definer: false` 拒绝 `SQL SECURITY DEFINER` 函数，避免借用定义者权限；只有逐个审计函数体和 DEFINER 后才应设为 `true`。
- 参数只接受标量值并使用 `?` 绑定，不接受 SQL 表达式。
- 仅支持 `INFORMATION_SCHEMA.ROUTINES` 中的存储函数；不支持存储过程和可加载/native UDF。
- 原始 `mysql.query` 会拒绝 schema-qualified 或未知函数，调用存储函数必须走 `mysql.function.call`。

MySQL 的 `SQL DATA ACCESS` 声明主要是元数据，不能替代应用 allowlist、账号权限和代码审计。

## 多数据源与 MySQL 版本

每个 `datasources[]` 条目独立配置：

- URL 名称、网络地址和默认数据库；
- schema 精确名称与 Glob 模式 allowlist；
- 读/写/监控凭据；
- TLS 和连接池；
- 监控能力；
- 存储函数 allowlist。

数据源名称必须匹配 `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`，因此可安全地作为单个 URL path segment。名称在一个配置内不能重复。

服务启动时读取实际 MySQL 版本，只接受 5.7.x 和 8.x，并为每个数据源创建对应版本的 Vitess parser。监控查询会处理主要版本差异，例如 5.7 的 `INFORMATION_SCHEMA.INNODB_LOCK_WAITS`、8.x 的 Performance Schema data locks，以及新版 8.x 的 `SHOW REPLICA STATUS` 术语。

## Schema allowlist

一个数据源可以组合 schema 精确名称和 Glob 模式：

```yaml
default_database: orders_dev
allowed_schemas:
  - shared_reference
allowed_schema_patterns:
  - "*_dev"
```

`allowed_schemas` 包含完整的字面 schema 名。每个 `allowed_schema_patterns` 模式必须至少包含一个 `*`，并锚定匹配完整 schema 名：只有 `*` 具有特殊含义，可匹配任意数量（包括零个）字符，其他字符均按字面值处理；不含通配符的名称应放入 `allowed_schemas`。两种匹配都严格区分大小写。schema 匹配任一列表即可允许，因此上例同时允许 `shared_reference` 和 `orders_dev` 等名称。两个列表都为空时，应用层不增加 schema 限制。

任一列表非空时，未限定 schema 的物理表会归属于 `default_database`，该数据库必须符合相同的合并规则。客户端使用未限定 schema 的表名时，应配置一个符合 allowlist 的默认数据库；否则无法安全判断这些引用。

allowlist 只检查请求 AST 中可见的直接物理表引用，不会发现视图的底层依赖，也不会改变监控工具既有的实例级边界。MySQL 账号的 `GRANT` 权限仍是最终的数据库授权边界：配置名称或模式匹配成功，绝不会赋予所选 MySQL 账号原本没有的权限。

## TLS 模式

| 模式 | 加密 | 服务端身份校验 | 说明 |
|---|---:|---:|---|
| `disabled` | 否 | 否 | 仅适用于受控本机/隔离网络 |
| `preferred` | 优先 | 否 | TLS 不可用时允许降级到明文 |
| `required` | 是 | 否 | 防窃听，但不验证证书身份 |
| `verify-ca` | 是 | 校验证书链 | 不校验主机名 |
| `verify-full` | 是 | 校验证书链和主机名 | 生产环境推荐 |

`ca_file`、`cert_file`、`key_file` 的相对路径以配置文件目录为基准；客户端证书与私钥必须成对配置。TCP 地址未提供 `server_name` 时会从主机名推导，使用 Unix Socket 或名称不匹配的地址时应显式设置。

## 配置说明

完整、带注释的配置见 [config.example.yaml](config.example.yaml)。重要规则如下：

- `version` 当前必须为 `1`。
- YAML 使用严格解析：未知字段、重复 key、merge key 和多个 YAML document 都会被拒绝。
- 每个数据库凭据必须且只能配置一种密码来源。首选 `password: ${ENV_NAME}`；只有该精确整值形式才读取环境变量，引用变量缺失或为空会导致校验失败。其他非空 `password` scalar 作为明文原样使用，不具有插值或 default 语义，且绝不应提交到 Git。`password_env` 和 `password_file` 仍作为兼容选项。HTTP Token 仍使用 `token_env` 与 `token_file` 二选一。
- Token 必须符合 RFC 6750 `b64token` 字符集；首尾/内部空白、控制字符和无法放入 Bearer Header 的值会在启动前被拒绝。
- `allowed_schemas` 和 `allowed_schema_patterns` 遵循 [Schema allowlist](#schema-allowlist) 中的合并、整库名锚定和大小写精确匹配规则。MySQL 数据库名是否区分大小写取决于服务器平台；应用始终保留精确大小写，避免在区分大小写的主机上误授权另一个数据库。
- `query_timeout` 使用 Go duration，例如 `500ms`、`10s`、`2m`。
- 大小支持整数 byte 或 `KiB`、`MiB`、`GiB`、`KB`、`MB`、`GB`。
- 结果中的整数和 DECIMAL 使用字符串表示，避免 JSON/JavaScript 精度损失；二进制数据使用 Base64。
- 请求参数中的整数若超出 JavaScript 可精确表示的 `±(2^53-1)`，必须以十进制字符串传入；超范围 JSON number 会在到达 MySQL 前被拒绝。

## 安全边界

只读判断不是“SQL 是否以 `SELECT` 开头”。服务会先拒绝多语句、可执行注释和 optimizer hint，再使用 Vitess AST 校验根节点及所有子节点。只读查询会拒绝：

- CTE 最终执行 `INSERT`、`UPDATE` 或 `DELETE`；
- `SELECT ... INTO`；
- `FOR UPDATE`、`LOCK IN SHARE MODE` 等锁定读；
- 用户变量赋值、序列推进和事务/会话状态修改；
- `SLEEP`、`BENCHMARK`、`LOAD_FILE`、advisory lock、复制等待等危险函数；
- 未纳入安全内置函数集合的函数，以及任意 schema-qualified 存储函数。

通过 AST 之后，查询仍会在数据库只读事务中执行，并受到最小权限账号、schema allowlist、超时、行数、列数、单元格大小、结果大小和并发上限保护。行/结果字节上限在驱动解码后执行，因此还应在 MySQL 端限制 `max_allowed_packet` 和资源使用。驱动层固定禁用 multi-statements、参数插值和本地文件读取。

每个连接使用固定的解析器兼容 `sql_mode`：`ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION,IGNORE_SPACE`。这避免服务器继承 `ANSI` 等组合模式后重新启用 `ANSI_QUOTES`、`PIPES_AS_CONCAT` 或其他与 Vitess 不同的词法。若业务 SQL 依赖被排除模式，应先改写 SQL，而不是放宽该安全约束。

`mysql.execute` 的 DML 路径显式开启并提交事务；实际回滚能力仍取决于目标表是否使用事务型存储引擎。MySQL 5.7/8.x 的大多数 DDL 会隐式提交，所以 DDL 不会被包装成一个误导性的事务，成功结果中的 `execution_mode` 为 `mysql_implicit_commit`，不能假设可回滚。

更完整的威胁模型见 [docs/security.md](docs/security.md)，账号授权建议见 [docs/mysql-privileges.md](docs/mysql-privileges.md)。

## 运维建议

- 默认保持 `read_only: true`，仅为明确场景开启单个写开关。
- HTTP 暴露到非回环地址时必须放在 HTTPS 认证代理或严格网络策略后；`mode: none` 会产生安全告警。
- 为每个数据源和角色使用不同账号，避免 `GRANT OPTION`、`FILE` 和不必要的全局权限。
- 严格隔离场景使用表级 `GRANT`；DEFINER 视图和存储函数属于需要单独审计的数据库代码，不能仅靠请求 AST 的 schema/function allowlist 推断其间接访问。
- 监控工具是实例级运维能力，可能返回 allowlist 外的 schema 名、SQL 文本、锁和复制信息；不要把高权限监控账号暴露给租户级 Token。
- Token 与数据库密码通过 secret manager 注入，不提交到 Git；密钥文件权限建议为 `0600`。
- 对 `mysql.execute`、函数写调用和 `mysql.admin.kill_query` 建立外部审计与审批流程。
- 先在预生产环境验证 MySQL 版本、Performance Schema、复制拓扑和所需权限。

## 测试

```bash
go test ./...
go test -race ./...
```

单元测试覆盖严格配置解析、密钥解析和脱敏、SQL AST 策略、多数据源版本能力、连接参数、结果边界、元数据/监控/函数/管理服务、HTTP 路由与认证。测试不要求连接真实 MySQL；上线前仍建议分别对 MySQL 5.7 与 8.0 运行集成测试。
