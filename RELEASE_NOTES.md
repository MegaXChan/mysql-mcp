# mysql-mcp v1.0.0

这是 `mysql-mcp` 的首个公开版本。

`mysql-mcp` 是一个使用 Go 编写、默认只读的 MySQL Model Context Protocol
（MCP）服务端，面向需要受控查询、元数据访问和运维能力的 Agent。当前版本
支持 MySQL 5.7/8.x、多数据源、stdio 与 Streamable HTTP。

## 主要能力

- 单进程连接多个 MySQL 数据源，分别维护读、写和监控连接池。
- HTTP 端点固定为 `/{datasource_name}/mcp`，会话及工具参数不能切换数据源；
  stdio 可通过 `--datasource` 绑定一个数据源。
- 基于 `vitess.io/vitess/go/vt/sqlparser` AST 判断 SQL 类型，不依赖
  `SELECT` 字符串前缀。
- 提供只读查询、安全 `EXPLAIN`、schema/表/列/索引/约束元数据，以及针对
  MySQL 5.7/8.x 适配的会话、锁、慢查询聚合、复制和 InnoDB 监控。
- 可按独立功能开关启用 DML、DDL、存储函数调用和类型化 `KILL QUERY`。
- 支持 schema 精确 allowlist 与仅使用 `*` 的整库名 Glob 模式。
- 支持无认证和静态 Bearer Token HTTP 认证。
- 提供 `/healthz`、`/readyz` 和不记录 SQL、Token、Header、body 的结构化
  HTTP 请求日志。
- 严格校验 YAML 未知字段、重复 key、非法组合、密钥引用和资源限制。
- 数据库密码支持 `password: ${ENV_NAME}`、明文、`password_env` 或
  `password_file`，并在配置诊断中脱敏。
- MySQL 连接支持 `disabled`、`preferred`、`required`、`verify-ca` 和
  `verify-full` TLS 模式。

## MCP 工具

- 基础信息：`mysql.info`
- 查询与执行：`mysql.query`、`mysql.explain`、`mysql.execute`
- 元数据：`mysql.metadata.schemas`、`mysql.metadata.tables`、
  `mysql.metadata.describe_table`、`mysql.metadata.indexes`、
  `mysql.metadata.constraints`
- 监控：`mysql.monitor.overview`、`mysql.monitor.storage`、
  `mysql.monitor.sessions`、`mysql.monitor.locks`、
  `mysql.monitor.top_queries`、`mysql.monitor.replication`、
  `mysql.monitor.innodb_status`
- 存储函数：`mysql.function.list`、`mysql.function.describe`、
  `mysql.function.call`
- 管理：`mysql.admin.kill_query`

工具是否出现在 `tools/list` 中取决于只读策略、功能开关、监控配置、MySQL
能力和存储函数 allowlist。

## 快速开始

Docker Hub 的 `latest` 标签始终跟随最新稳定版本：

```bash
docker pull megaxcn/mysql-mcp:latest
docker run --pull=always --rm megaxcn/mysql-mcp:latest --version
```

也可以下载本 Release 对应平台的压缩包，然后复制并修改示例配置：

```bash
cp config.example.yaml config.yaml
./mysql-mcp validate-config --config ./config.yaml
./mysql-mcp serve --config ./config.yaml
```

HTTP 模式按数据源提供独立端点，例如：

```text
http://127.0.0.1:8080/orders57/mcp
http://127.0.0.1:8080/analytics8/mcp
```

stdio 模式示例：

```bash
./mysql-mcp serve --config ./config.yaml --datasource orders57
```

完整配置与部署说明请参阅
[README.zh-CN.md](https://github.com/MegaXChan/mysql-mcp/blob/v1.0.0/README.zh-CN.md)。

## 发布产物

GitHub Release 提供以下使用 `CGO_ENABLED=0` 构建的归档：

| 操作系统 | 架构 | 格式 |
|---|---|---|
| Linux | `386`、`amd64`、`armv6`、`armv7`、`arm64` | `.tar.gz` |
| Windows | `386`、`amd64`、`arm64` | `.zip` |
| macOS | `amd64`、`arm64` | `.tar.gz` |

Docker 镜像为以非 root UID/GID `65532:65532` 运行的 `scratch` 静态镜像，
支持 `linux/386`、`linux/amd64`、`linux/arm/v6`、`linux/arm/v7` 和
`linux/arm64`。

请同时下载 `SHA256SUMS` 并在运行前校验归档。需要可复现部署或回滚时，请
固定使用 `megaxcn/mysql-mcp:v1.0.0`，不要使用浮动的 `latest` 标签。

## 安全默认值

- 默认 `server.read_only: true`；DML、DDL、管理操作及写存储函数分别关闭。
- 写操作必须同时满足全局非只读、数据源非只读、独立 write 凭据、对应功能
  开关、SQL AST 策略和 MySQL 最小权限账号。
- 每次 SQL 调用只接受一个语句；只读查询拒绝 executable comments、optimizer
  hints、`SELECT ... INTO`、锁定读、变量赋值、危险或未知函数等绕过方式。
- 查询受超时、并发、SQL 大小、行列数、单元格和结果大小限制。
- 存储函数必须加入 schema-qualified allowlist，并通过
  `mysql.function.call` 调用；不支持存储过程和 loadable/native UDF。

## 使用前注意

- `config.example.yaml` 中的主机、账号、函数和证书路径均为示例，不能原样用于
  生产环境。请先运行 `validate-config`。
- MCP HTTP 监听器本身不提供 HTTPS。`datasources[].tls` 只保护服务端到 MySQL
  的连接；远程部署应位于 HTTPS 认证代理或严格网络策略之后。
- Bearer Token 是共享静态凭据，不提供用户身份、RBAC 或细粒度授权。
- Schema allowlist 只检查请求 AST 中直接引用的对象，不展开视图依赖，也没有
  表或列 denylist；MySQL `GRANT` 始终是最终授权边界。
- 监控工具提供实例级视角，可能返回 allowlist 外的 schema、SQL、锁或复制
  信息，建议使用独立低权限 monitor 账号。
- MySQL DDL 通常会隐式提交，不能假设可回滚；DML 的实际回滚能力取决于表所用
  存储引擎。
- 应用结果字节限制发生在驱动解码后，仍需在 MySQL 端配置合理的
  `max_allowed_packet` 和资源限制。
- Windows 二进制尚未代码签名；macOS 二进制尚未签名或公证，SmartScreen 或
  Gatekeeper 可能显示警告。
- 单元测试不连接真实 MySQL。生产上线前应分别在 MySQL 5.7/8.x 环境验证账号
  权限、Performance Schema、复制拓扑及实际工作负载。

感谢使用 `mysql-mcp`。问题与建议请提交到
[GitHub Issues](https://github.com/MegaXChan/mysql-mcp/issues)。
