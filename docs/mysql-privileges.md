# MySQL 最小权限建议

`mysql-mcp` 的三个凭据角色对应三个独立连接池：

- `credentials.read`：查询、元数据和 `effect: read` 存储函数；必填。
- `credentials.write`：DML、DDL、管理操作和 `effect: write` 存储函数；只读部署应省略。
- `credentials.monitor`：固定监控查询；省略时会回退到读账号，但生产环境建议独立配置。

配置这些账号时，数据库密码首选 `password: ${ENV_NAME}`。只有该精确整值形式才读取环境变量，变量缺失或为空会导致配置校验和启动失败；其他非空 scalar 会作为明文密码原样使用，不执行拼接或 default 展开。明文虽受支持但强烈不建议写入配置或提交 Git，诊断输出会对解析后的密码脱敏。`password_env` 与 `password_file` 仍兼容，但每个凭据必须在三者中只选一个。编排器提供文件 secret 时可继续使用 `password_file`，并以只读方式挂载、限制文件权限。

下列 SQL 只是授权模板，需按实际 MySQL 版本、账号来源地址、schema、函数和监控项裁剪。不要直接复制到生产环境后授予所有可选权限。

## 只读账号

```sql
CREATE USER 'mysql_mcp_orders_read'@'10.%'
  IDENTIFIED BY '<由密钥系统生成的随机密码>' REQUIRE SSL;

GRANT SELECT ON `orders`.*
  TO 'mysql_mcp_orders_read'@'10.%';

GRANT SELECT ON `orders_archive`.*
  TO 'mysql_mcp_orders_read'@'10.%';

-- 只给 allowlist 中实际需要的函数授权。
GRANT EXECUTE ON FUNCTION `orders`.`calculate_discount`
  TO 'mysql_mcp_orders_read'@'10.%';
```

上面的 schema 级 `GRANT` 适合整个 schema 都属于同一信任域的部署。若 `allowed_schemas` 或 `allowed_schema_patterns` 用于更严格的隔离，应改为逐表授权：视图的底层表/函数不会出现在客户端请求 AST 中，`SQL SECURITY DEFINER` 视图还可能借用定义者权限。只给逐个审计过的视图单独授权，并优先使用 `SQL SECURITY INVOKER`。

例如，`allowed_schema_patterns: ["*_dev"]` 只影响应用层的 schema 判定，不会创建、扩展或替代任何 MySQL `GRANT`。模式以后匹配到的新 schema 也不会自动获得数据库权限；每个账号的实际 `GRANT` 始终是最终授权边界。反过来，账号拥有某项 `GRANT` 也不会绕过应用层 allowlist。应同时审查模式可能匹配的名称集合和账号被授予的具体对象。

`INFORMATION_SCHEMA` 返回内容会随账号能看到的对象而变化。不要为了让元数据工具“看得更多”而授予业务不需要的 schema 权限。

## 写账号

仅启用 DML 时，通常从以下集合按需选择：

```sql
CREATE USER 'mysql_mcp_orders_write'@'10.%'
  IDENTIFIED BY '<由密钥系统生成的随机密码>' REQUIRE SSL;

GRANT SELECT, INSERT, UPDATE, DELETE
  ON `orders`.* TO 'mysql_mcp_orders_write'@'10.%';
```

`REPLACE` 同时依赖 INSERT，并可能根据冲突路径依赖 DELETE。若只需要 INSERT 或只更新少量表，应进一步使用表级授权，而不是 schema 级授权。

启用 DDL 时，再根据允许的变更类型从 `CREATE`、`ALTER`、`DROP`、`INDEX`、`REFERENCES`、`CREATE VIEW`、`SHOW VIEW`、`TRIGGER` 等权限中逐项增加。不要默认授予 `ALL PRIVILEGES`。

写存储函数应逐个授权：

```sql
GRANT EXECUTE ON FUNCTION `orders`.`refresh_customer_segment`
  TO 'mysql_mcp_orders_write'@'10.%';
```

若函数使用 `SQL SECURITY INVOKER`，写账号还需要函数体实际访问对象的权限。若使用 `SQL SECURITY DEFINER`，应优先保持 `allow_definer: false`；确需开放时，必须审计函数体、DEFINER 账号、动态 SQL 和对象解析路径。

## 监控账号

基础版本/存储概览依赖系统变量、`INFORMATION_SCHEMA` 和账号可见对象。查看所有会话、锁、复制状态等通常需要按监控项增加全局权限：

```sql
CREATE USER 'mysql_mcp_orders_monitor'@'10.%'
  IDENTIFIED BY '<由密钥系统生成的随机密码>' REQUIRE SSL;

-- 查看其他账号的会话；InnoDB 状态也可能要求 PROCESS。
GRANT PROCESS ON *.*
  TO 'mysql_mcp_orders_monitor'@'10.%';

-- SHOW SLAVE STATUS / SHOW REPLICA STATUS。
GRANT REPLICATION CLIENT ON *.*
  TO 'mysql_mcp_orders_monitor'@'10.%';

-- 只读 digest、线程和 data lock 信息。不同发行版的默认可见性可能不同。
GRANT SELECT ON `performance_schema`.*
  TO 'mysql_mcp_orders_monitor'@'10.%';
```

监控账号不需要 UPDATE Performance Schema setup tables，因为服务不会修改 consumers/instruments；如果目标实例没有启用所需 consumer，应由 DBA 在服务之外管理。

监控是实例级能力，不受业务 `allowed_schemas` 与 `allowed_schema_patterns` 的完整隔离：会话、锁、digest、InnoDB 状态和复制信息可能包含其他 schema 的对象名或 SQL 文本。需要租户隔离时应关闭这些监控项，或把监控端点放在独立服务/Token 后。

MySQL 5.7 和 8.0 的锁表、复制术语与动态权限不同。应先只授予当前启用监控项所需的权限，运行 `validate-config` 和预生产探测，再依据明确的 access denied 错误补充，而不是预先授予 `SUPER`。

## `KILL QUERY` 权限

`mysql.admin.kill_query` 可取消其他业务连接正在执行的语句，属于高风险能力：

- MySQL 8.0 对跨账号连接管理通常使用全局动态权限 `CONNECTION_ADMIN`（具体要求取决于补丁版本和托管服务策略）。
- MySQL 5.7 缺少该细粒度动态权限，跨账号 KILL 往往需要高权限；若只能通过 `SUPER` 满足，应优先禁用 `features.admin`，或使用严格隔离的专用实例/代理能力。
- 同一账号通常可以管理自己的线程，但 MCP 的写账号一般不是业务查询账号，因此这不足以实现实际的故障处置。

当前配置把管理操作归入写连接池，因此开启 `features.admin` 会提高 `credentials.write` 所需权限。不要把监控账号的密码复用为写账号密码。

## 明确不应授予

除非有与本项目无关且经过审计的用途，否则 MCP 账号不应拥有：

- `GRANT OPTION`；
- `FILE`；
- 创建用户、授权或角色管理能力；
- replication applier、binlog 管理或服务器关闭权限；
- unrestricted `SUPER`；
- 对 `mysql` 系统 schema 的写权限；
- Performance Schema setup 表的 UPDATE 权限。

## 校验实际授权

以每个角色账号连接后检查：

```sql
SHOW GRANTS FOR CURRENT_USER;
SELECT CURRENT_USER(), USER(), @@version, @@read_only;
```

还应分别验证：

1. 允许的查询和监控工具可以运行。
2. 访问 allowlist 外 schema 被拒绝。
3. 读账号无法 INSERT/UPDATE/DELETE/DDL。
4. 未加入 allowlist 的函数无法调用。
5. 写账号无法执行未启用类别的操作。
6. Token、密码和 SQL 参数不出现在应用及代理日志中。

账号权限是 SQL 策略之外的最后一道边界。即使应用已经正确拒绝某项操作，也不应依赖一个过度授权的 MySQL 账号。
