# 安全模型

本文描述 `mysql-mcp` 能控制的边界，以及部署方仍需承担的责任。

## 信任边界

一次 HTTP MCP 请求依次经过：

```text
客户端
  -> HTTPS/网络边界（部署方）
  -> none 或 Bearer Token 认证
  -> 固定的 /{datasource_name}/mcp 路由
  -> 工具级功能开关与参数校验
  -> Vitess SQL AST 策略或服务端固定 SQL
  -> 读/写/监控专用连接池
  -> MySQL 权限与事务语义
```

任一层允许某项操作都不会自动绕过下一层。HTTP 路径在路由建立时已经绑定数据源，工具输入中不提供 datasource 字段，避免已获某一数据源访问权的会话横向切换。

## SQL 判定

服务不使用正则或 `strings.HasPrefix(sql, "SELECT")` 判断只读语句。每个数据源按实际探测到的 MySQL 5.7/8.x 版本构造 Vitess parser，并执行以下步骤：

1. 拒绝 MySQL executable comment 和 optimizer hint。
2. 使用 SQL-aware splitter 确认只有一个非空语句；字符串中的分号不会误判为分隔符。
3. 解析完整 AST，按根节点分类为 read、write、DDL、transaction、session、admin、stored program 等类别。
4. 递归检查子节点，拒绝锁定读、`SELECT INTO`、变量赋值、序列推进、危险函数和未知函数。
5. 验证请求 AST 中的直接物理表引用是否属于 `allowed_schemas`。

这会覆盖诸如 `WITH ... UPDATE`、注释隐藏关键字、嵌套危险函数等仅看首个 token 无法正确判定的情况。

原始 SQL 工具不允许直接调用存储函数。函数必须通过 allowlist 和 `mysql.function.call` 调用，服务端负责引用 schema/function 标识符并为每个参数生成 `?` 占位符。视图会隐藏其底层表和函数依赖；这些间接依赖不在请求 AST 中，不能由此策略静态证明安全。

## 深度防御

AST 校验只是第一层，运行时还包括：

- 查询连接使用只读事务，成功后也回滚关闭事务；
- DML 使用独立写事务，只有执行和结果读取均成功才提交；非事务型表仍受其存储引擎语义约束；
- DDL 走独立的直接执行路径并返回 `mysql_implicit_commit`，明确反映 MySQL 大多数 DDL 无法通过外层事务回滚；
- multi-statements、参数插值、local infile 和任意本地文件访问固定关闭；
- 每个物理连接使用固定、解析器兼容的 `sql_mode`：`ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION,IGNORE_SPACE`。不继承可能重新启用 `ANSI_QUOTES`、`NO_BACKSLASH_ESCAPES`、`PIPES_AS_CONCAT` 等词法差异的服务器组合模式；
- 读、写、监控账号与池分离；只读部署不会依赖写账号授权；
- SQL 长度、超时、返回行/列/单元格/结果大小和并发量受限；驱动必须先解码当前行，单元格/结果字节上限不是服务端 packet 分配前的硬隔离；
- 管理服务只有类型化 `KILL QUERY <uint64>`，没有任意 admin SQL；
- 监控服务只运行编译进程序的固定 SQL；
- schema、table 等值作为驱动参数传递，而不是拼入 SQL 语法。

## 只读策略

有效只读值为：

```text
server.read_only OR datasource.read_only
```

数据源不能放宽全局策略。`server.read_only: true` 时，配置校验会拒绝 `dml`、`ddl`、`admin` 或 `function_write` 中任一开关。关闭只读也不会自动开放写工具，仍需逐项开启 feature。

存储函数的 `effect` 是部署方作出的安全声明。服务会用 MySQL 的 `SQL DATA ACCESS` 元数据做冲突检查，但该声明本身不是可信沙箱。所有 allowlist 函数都需要代码审查；`allow_definer: true` 尤其需要审查 DEFINER 账号及其权限。

`allowed_schemas` 是请求文本的直接引用策略，不是数据库对象依赖分析器。`SQL SECURITY DEFINER` 视图可能借用定义者权限访问 allowlist 外对象或调用函数。需要强隔离时，应使用表级授权，只为逐个审计过的视图单独授权，并优先采用 `SQL SECURITY INVOKER`；不要把 `GRANT SELECT ON schema.*` 当成租户沙箱。

监控服务有意提供实例级运维视角。会话、锁、digest、InnoDB 状态和复制输出可能包含 `allowed_schemas` 之外的对象名或 SQL 文本，且部分输出无法可靠归属到单个 schema。应把启用监控及监控账号权限视作独立授权边界，不应与租户级查询 Token 共用。

## HTTP 安全

Bearer Token 模式提供单个共享凭据的认证，不提供：

- 用户级身份；
- 租户隔离；
- 工具级角色；
- Token 过期、撤销列表或自动轮换；
- 传输加密。

因此，非本机部署应在前置代理终止 HTTPS，并结合身份认证、网络 allowlist、速率限制和审计日志。若在非回环监听器上使用 `mode: none`，服务会产生安全告警。

健康检查不需要 Token，但只返回固定的存活/就绪文本。不要在代理层把其他路径重写到 `/healthz` 或 `/readyz`。

HTTP 服务限制 Header、请求体总大小和读取时间；客户端必须在 30 秒内完成请求读取。响应写入超时为查询超时再加固定编码余量。外层代理仍应设置连接数、请求速率和更靠近网络边界的超时。

## 密钥处理

- 配置不接受明文 password/token 字段，只接受环境变量名或文件路径。
- 同一个密钥只能选择一个来源，环境变量名会校验格式。
- 相对文件路径以配置文件目录为基准。
- 日志格式化配置对象时会脱敏已解析的密钥。
- 不应把密钥放进 CLI 参数，因为参数通常可被同机进程和运维系统观察。

部署方应使用 secret manager 注入、限制文件权限、定期轮换，并确保错误日志和反向代理日志不记录 `Authorization` 请求头。

## 数据暴露与资源消耗

只读不等于无风险：复杂查询可能消耗 CPU/IO，合法结果也可能包含个人数据或密钥。应同时：

- 仅授权必要 schema、表和列；
- 设置较短查询超时和合理结果上限；
- 在 MySQL 端设置合理的 `max_allowed_packet`；应用的结果字节限制发生在驱动读取/解码后，不能阻止单个超大 packet 的首次分配；
- 使用 MySQL 资源组、只读副本或专用报表库隔离负载；
- 在 MCP 客户端侧限制谁能查看工具结果；
- 避免给读账号访问 `mysql` 系统库及业务密钥表。

## 不在本服务范围内

- HTTP 证书管理和 TLS 终止；
- 企业 SSO、OAuth/OIDC 和细粒度 RBAC；
- MySQL 审计插件、备份恢复和高可用切换；
- 对存储函数体的静态/动态安全证明；
- 将恶意查询的资源消耗绝对归零。

这些能力应由网关、身份系统、MySQL 和运维平台共同提供。
