# gorm-doris

Apache Doris 的 GORM dialector。

```
module github.com/techquest-tech/gorm-doris
```

## 实现方式

包一层 `gorm.io/driver/mysql`：Doris 的 FE 对外提供 MySQL 协议，连接串、`?` 占位符、
反引号标识符、`information_schema` 查询都能直接复用。本包只替换 Doris 与 MySQL 不一致的地方：

1. 方言名 → `doris`；
2. 类型映射（VARCHAR 必须带长度、大文本是 STRING、无符号整数加宽成有符号、自增列强制 bigint）；
3. 建表 DDL（Doris 必须写 `KEY` 子句 + `DISTRIBUTED BY` 分桶 + `PROPERTIES`）；
4. 迁移器里 Doris 不支持的语法（`DROP TABLE ... CASCADE`、`SET FOREIGN_KEY_CHECKS`、
   `ALTER TABLE ... RENAME TO`）；
5. 连接层强制 `interpolateParams=true`（见下）。

## 安装

```bash
go get github.com/techquest-tech/gorm-doris
```

## 用法

```go
import (
    "gorm.io/gorm"
    doris "github.com/techquest-tech/gorm-doris"
)

db, err := gorm.Open(doris.Open("user:pass@tcp(fe.host:9030)/demo?charset=utf8mb4&parseTime=True&loc=Local"),
    &gorm.Config{
        SkipDefaultTransaction: true, // 建议：Doris 没有多语句事务语义
    })
if err != nil { return err }

db.AutoMigrate(&Order{})
```

带建表选项：

```go
db, err := gorm.Open(doris.New(doris.Options{
    DSN:            dsn,
    Buckets:        8,                  // DISTRIBUTED BY HASH(...) BUCKETS n
    ReplicationNum: "3",                // PROPERTIES 里的 replication_num
    Properties:     map[string]string{"storage_medium": "SSD"},
    KeyType:        doris.KeyUnique,     // 强制表模型；默认「有主键 → UNIQUE，无主键 → DUPLICATE」
    KeyColumns:     []string{"OrderNo"}, // 无主键时手工指定 key 列
    OnConflict:     doris.OnConflictDrop, // 默认丢掉 ON CONFLICT 子句
}), &gorm.Config{SkipDefaultTransaction: true})
```

`Options` 字段一览：`DSN` `Conn` `DriverName` `SkipInitializeWithVersion` `KeyType`
`KeyColumns` `Buckets` `ReplicationNum` `Properties` `OnConflict` `DefaultStringSize`
`DisableDatetimePrecision`。默认值：`DriverName=mysql`、`Buckets=1`、`ReplicationNum=1`、
`OnConflict=drop`、`DefaultStringSize=65533`、`DisableDatetimePrecision=true`。

## 生成的建表 DDL

模型：

```go
type Order struct {
    ID      uint      `gorm:"primaryKey;autoIncrement"`
    OrderNo string    `gorm:"size:64;comment:订单号"`
    Amount  float64   `gorm:"precision:18;scale:2"`
    Qty     int32
    CreatedAt time.Time
}
```

`db.Migrator().CreateTable(&Order{})` 生成：

```sql
CREATE TABLE IF NOT EXISTS `orders` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `order_no` varchar(64) NULL COMMENT '订单号',
  `amount` decimal(18, 2) NULL,
  `qty` int NULL,
  `created_at` datetime NULL
)
UNIQUE KEY (`id`)
DISTRIBUTED BY HASH(`id`) BUCKETS 1
PROPERTIES (
  "enable_unique_key_merge_on_write" = "true",
  "replication_num" = "1"
)
```

规则：

| 项 | 规则 |
| --- | --- |
| key 列 | 主键字段 → `Options.KeyColumns` → 第一个非忽略字段；key 列自动排到列定义最前（Doris 要求 key 必须是前缀）并强制 `NOT NULL` |
| 表模型 | 有主键 → `UNIQUE KEY`（INSERT 即 upsert）；无主键 → `DUPLICATE KEY`；`Options.KeyType` 可强制（含 `AGGREGATE`） |
| 分桶 | `DISTRIBUTED BY HASH(<第一个 key 列>) BUCKETS n` |
| PROPERTIES | `replication_num` + `Options.Properties`；有自增列且是 UNIQUE 表时追加 `enable_unique_key_merge_on_write=true`（DUPLICATE 表不写，该属性是 UNIQUE 专有） |
| 自增列 | Doris 要求 `BIGINT NOT NULL` 且只在 UNIQUE/DUPLICATE 表：自增字段一律映射成 `bigint`（含 uint）；`AUTO_INCREMENT` 需要 Doris 2.1+ |
| 列注释 | 内联 `COMMENT '...'` |
| 索引 | **不生成**。Doris 没有二级索引，`CreateIndex` 打 Warn 后跳过；倒排索引 / BloomFilter 请手写 `INDEX ... USING INVERTED` 或 `ALTER TABLE ... SET ("bloom_filter_columns"="c")` |
| 约束 | **不生成**。Doris 没有 `ALTER ... ADD CONSTRAINT`，`HasConstraint` 恒为 false |
| 类型变更 | `MigrateColumn` 是空实现，改列类型请手写 `ALTER TABLE` |
| `ON CONFLICT` | `OnConflictDrop`（默认，什么都不写，因为 UNIQUE 表 INSERT 本身就是 upsert）/ `OnConflictDuplicateKey`（渲染 `ON DUPLICATE KEY UPDATE`）/ `OnConflictError`（直接返回 `ErrOnConflictUnsupported`） |
| `AutoMigrate` | 只做「建缺失表 + 补缺失列」（已验证补列）；不动类型、约束、索引 |

## 类型映射

| Go / gorm | Doris |
| --- | --- |
| `bool` | `boolean` |
| `int8/int16/int32/int64` | `tinyint` / `smallint` / `int` / `bigint` |
| `uint8/uint16/uint32/uint64` | `smallint` / `int` / `bigint` / `largeint`（加宽到有符号，不依赖版本对 UNSIGNED 的支持） |
| 自增整型（任意宽度） | `bigint`（Doris 自增列必须 BIGINT） |
| `float32` / `float64` | `float` / `double` |
| `precision,scale` 标签 | `decimal(p, s)` |
| `string` + `size:n` | `varchar(n)` |
| `string` 无 size | `varchar(65533)`（Doris VARCHAR 上限） |
| `type:text` / `[]byte` | `string` |
| `time.Time` | `datetime`；`precision:3` → `datetime(3)` |
| `type:jsonb` | `json` |

key 列建议显式写 `size:`（Doris 对 key 列长度有限制），本包默认的 65533 适合普通值列。

## 连接层：强制 interpolateParams

`Initialize` 会把 DSN 上的 `interpolateParams` 打开（`Conn` 注入路径改不了 DSN，需调用方自己保证）。

原因：走 go-sql-driver 的服务端 prepared statement 时，gorm 的 `First()` 生成的

```sql
SELECT * FROM `t` WHERE name = ? ORDER BY `t`.`id` LIMIT 1
```

会被 Doris 前端判成 `mismatched input 'LIMIT' expecting {<EOF>, ';'}`；改成客户端内联参数后
CRUD 全部通过。转义仍由驱动负责，不影响防注入。

另外实测：Doris **接受** gorm 默认的 `START TRANSACTION` / `COMMIT`，但 Doris 并没有真正的多语句
事务，建议还是配 `SkipDefaultTransaction: true`。

## 视图

视图走 **gorm 原生接口**，dialector 没有任何额外实现：

```go
db.Migrator().CreateView("order_view", gorm.ViewOption{
    Replace: true,                       // → CREATE OR REPLACE VIEW
    Query:   db.Model(&Order{}).Select("id", "order_no"),
})
db.Migrator().DropView("order_view")     // DROP VIEW IF EXISTS
```

批量建视图就是对原生接口循环：

```go
for name, query := range views {          // views: map[string]string，值是子查询 SQL
    if err := db.Migrator().CreateView(name, gorm.ViewOption{Replace: true, Query: db.Raw(query)}); err != nil {
        return err
    }
}
```

Doris 上视图相关语句的支持情况：

| 语句 | 结果 |
| --- | --- |
| `CREATE VIEW x AS SELECT ...` | 支持 |
| `CREATE OR REPLACE VIEW x AS SELECT ...` | 支持 |
| `DROP VIEW IF EXISTS x` | 支持 |
| `CREATE VIEW ... WITH CHECK OPTION` | **不支持**（服务端报 `Error 1105 ... mismatched input 'WITH'`） |
| `CREATE OR REPLACE ALGORITHM = UNDEFINED VIEW ...` | **不支持** |

最后一条是很多 ORM/框架的默认视图模板：它们会在 `CREATE OR REPLACE` 后面插入
`ALGORITHM = UNDEFINED`。在 Doris 上要改用不带 `ALGORITHM` 的写法——本包的 gorm 原生
`CreateView` 就不会生成 `ALGORITHM`，开箱可用。

## 何时别用这个 dialector

大批量数据同步（去重、多值 INSERT 分批、`Data too long` 自动扩容 VARCHAR 重试）请用专门的
同步工具/方言；本包面向应用侧 CRUD，只做 gorm 语义到 Doris DDL 的翻译。

## 测试

```bash
go test ./...
```

不连库：DryRun 校验 DDL/DML、类型映射、`ON CONFLICT`、视图、删表语法。

真机用例需要 `DORIS_DSN` 指向一个 Doris 实例，未设置时自动跳过：

```bash
DORIS_DSN='user:pass@tcp(<fe-host>:9030)/demo?charset=utf8mb4&parseTime=True&loc=Local' \
  go test -run TestLive -v ./...
```

覆盖：建表（含 UNIQUE KEY / 自动属性 / AUTO_INCREMENT）→ 插入 → 查询 → UNIQUE KEY 覆盖插入 →
更新 → 计数 → 删除 → 建视图/查视图/删视图 → 删表；另有两条用例分别验证
`AutoMigrate` 补列路径与「gorm 默认事务是否被接受」。用例会建 `gorm_doris_smoke_*` 临时表
并在结束时删除，建议指向测试库。

## 已验证 / 未验证

已验证：建表 DDL、CRUD、UNIQUE KEY upsert、`AutoMigrate` 补列、
`information_schema` 查询（mysql 方言的 ColumnTypes 直接用）、视图、删表、事务行为。

未验证：`RenameTable`（按 Doris 语法写成 `ALTER TABLE old RENAME new`，未实跑）、
唯一索引/约束的跳过分支、列类型变更（本包不做）、不同 Doris 版本间的差异。