// Package doris 提供 Apache Doris 的 GORM dialector。
//
// 实现方式是包一层 gorm.io/driver/mysql：Doris 的 FE 对外提供 MySQL 协议，
// 连接、占位符（?）、反引号标识符、information_schema 查询都能直接复用；
// 本包只替换三处 Doris 与 MySQL 不一致的地方：
//
//  1. 方言名（doris，便于 gorm 生态按方言分支）；
//  2. 类型映射（Doris 没有 unsigned 的语义差异、VARCHAR 必须带长度、大文本是 STRING）；
//  3. 建表 DDL（Doris 必须写 KEY 子句 + DISTRIBUTED BY 分桶 + PROPERTIES）。
package doris

import (
	"errors"
	"fmt"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

// DriverName 是底层 database/sql 驱动名：Doris 走 MySQL 协议。
const DriverName = "mysql"

// ErrOnConflictUnsupported 在 Options.OnConflict = OnConflictError 时返回。
var ErrOnConflictUnsupported = errors.New("doris: Doris 不支持 ON DUPLICATE KEY UPDATE；UNIQUE KEY 表的 INSERT 本身就是 upsert")

// KeyType 是 Doris 的表模型。
type KeyType string

const (
	// KeyUnique 唯一键模型：INSERT 即 upsert（有主键时的默认值）。
	KeyUnique KeyType = "UNIQUE"
	// KeyDuplicate 明细模型：允许重复行（无主键且没指定 key 列时的默认值）。
	KeyDuplicate KeyType = "DUPLICATE"
	// KeyAggregate 聚合模型：按 key 聚合（需显式指定聚合函数，本包只写 key 子句）。
	KeyAggregate KeyType = "AGGREGATE"
)

// OnConflictMode 决定 gorm 的 clause.OnConflict 怎么落到 Doris 语法上。
type OnConflictMode string

const (
	// OnConflictDrop（默认）删掉该子句：UNIQUE KEY 表的 INSERT 已经按 key 覆盖。
	OnConflictDrop OnConflictMode = "drop"
	// OnConflictDuplicateKey 渲染成 MySQL 的 ON DUPLICATE KEY UPDATE（部分 Doris 版本不支持）。
	OnConflictDuplicateKey OnConflictMode = "duplicate-key"
	// OnConflictError 遇到 OnConflict 直接报错，避免静默改变语义。
	OnConflictError OnConflictMode = "error"
)

// Options 是 dialector 的连接与建表配置。
type Options struct {
	// DSN 形如 user:pass@tcp(host:9030)/dbname?charset=utf8mb4&parseTime=True&loc=Local。
	DSN string
	// Conn 直接注入已有连接池；设置后 DSN 与 DriverName 都不生效。
	Conn gorm.ConnPool
	// DriverName 默认 mysql。
	DriverName string
	// SkipInitializeWithVersion 为 true 时不发 SELECT VERSION()（无权限/离线时用）。
	SkipInitializeWithVersion bool
	// KeyType 强制表模型；空值表示按「有主键 → UNIQUE，无主键 → DUPLICATE」推断。
	KeyType KeyType
	// KeyColumns 无主键时手工指定 key 列（按 DB 列名）。
	KeyColumns []string
	// Buckets 是 DISTRIBUTED BY HASH(...) BUCKETS n 的 n，默认 1。
	Buckets int
	// ReplicationNum 是 PROPERTIES 里的 replication_num，默认 "1"。
	ReplicationNum string
	// Properties 覆盖/追加 PROPERTIES，键值都按字符串写。
	Properties map[string]string
	// OnConflict 见 OnConflictMode，默认 OnConflictDrop。
	OnConflict OnConflictMode
	// DefaultStringSize 是 string 字段没有 size 标签时用的 VARCHAR 长度，默认 65533（Doris 上限）。
	DefaultStringSize uint
	// DisableDatetimePrecision 为 true 时 information_schema 查询不选 datetime_precision，默认 true。
	DisableDatetimePrecision *bool
}

// Dialector 实现 gorm.Dialector。
type Dialector struct {
	*mysql.Dialector
	Options *Options
}

// Open 用 DSN 创建 dialector（其余选项取默认值）。
func Open(dsn string) gorm.Dialector {
	return New(Options{DSN: dsn})
}

// New 用完整选项创建 dialector。
func New(opts Options) gorm.Dialector {
	if opts.DriverName == "" {
		opts.DriverName = DriverName
	}
	if opts.Buckets <= 0 {
		opts.Buckets = 1
	}
	if opts.ReplicationNum == "" {
		opts.ReplicationNum = "1"
	}
	if opts.OnConflict == "" {
		opts.OnConflict = OnConflictDrop
	}
	if opts.DefaultStringSize == 0 {
		opts.DefaultStringSize = 65533
	}
	disablePrecision := true
	if opts.DisableDatetimePrecision != nil {
		disablePrecision = *opts.DisableDatetimePrecision
	}
	opts.DisableDatetimePrecision = &disablePrecision

	cfg := &mysql.Config{
		DSN:                       opts.DSN,
		Conn:                      opts.Conn,
		DriverName:                opts.DriverName,
		SkipInitializeWithVersion: opts.SkipInitializeWithVersion,
		DisableDatetimePrecision:  disablePrecision,
	}
	return &Dialector{
		Dialector: &mysql.Dialector{Config: cfg},
		Options:   &opts,
	}
}

// Name 返回方言名。
func (d Dialector) Name() string { return "doris" }

// Apply 在 gorm.Open 前补默认值：Doris 的 DATETIME 默认精度是秒，NowFunc 对齐到秒。
func (d Dialector) Apply(config *gorm.Config) error {
	if config.NowFunc != nil {
		return nil
	}
	config.NowFunc = func() time.Time { return time.Now().Local().Round(time.Second) }
	return nil
}

// Initialize 建连接、注册回调，并按 Options.OnConflict 决定 ON CONFLICT 怎么渲染。
func (d Dialector) Initialize(db *gorm.DB) error {
	if d.Options == nil {
		opts := Options{}
		d.Options = &opts
	}
	d.applyDorisFlags()
	d.forceInterpolateParams()
	if err := d.Dialector.Initialize(db); err != nil {
		return err
	}
	d.installOnConflictBuilder(db)
	return nil
}

// forceInterpolateParams 把 DSN 上的 interpolateParams 打开。
//
// 实测（Doris 前端 VERSION() 返回 5.7.99）：走 go-sql-driver 的服务端
// prepared statement 时，gorm 的 First() 生成的
// SELECT ... WHERE name = ? ORDER BY `t`.`id` LIMIT 1
// 会被 Doris 判成 mismatched input 'LIMIT' expecting {<EOF>, ';'}；
// 改成客户端内联参数后全部通过。Doris 前端的 prepare 支持有限，这里直接替用户打开
// （转义仍由驱动负责，不影响防注入）。
func (d Dialector) forceInterpolateParams() {
	cfg := d.Dialector.Config
	if cfg.Conn != nil || cfg.DSN == "" {
		// 注入连接池的路径改不了 DSN，由调用方自己保证。
		return
	}
	dsnCfg := cfg.DSNConfig
	if dsnCfg == nil {
		parsed, err := mysqlDriver.ParseDSN(cfg.DSN)
		if err != nil {
			return
		}
		dsnCfg = parsed
		cfg.DSNConfig = parsed
	}
	if dsnCfg.InterpolateParams {
		return
	}
	dsnCfg.InterpolateParams = true
	cfg.DSN = dsnCfg.FormatDSN()
}

// applyDorisFlags 关掉 Doris 不具备的 MySQL 能力，避免 gorm 生成不支持的语法。
func (d Dialector) applyDorisFlags() {
	cfg := d.Dialector.Config
	cfg.DontSupportRenameIndex = true
	cfg.DontSupportRenameColumn = true
	cfg.DontSupportRenameColumnUnique = true
	cfg.DontSupportForShareClause = true
	cfg.DontSupportDropConstraint = true
	cfg.DontSupportNullAsDefaultValue = true
	cfg.DisableWithReturning = true
}

func (d Dialector) installOnConflictBuilder(db *gorm.DB) {
	if db.ClauseBuilders == nil {
		db.ClauseBuilders = map[string]clause.ClauseBuilder{}
	}
	switch d.Options.OnConflict {
	case OnConflictDuplicateKey:
		// 保留 mysql 方言的 ON DUPLICATE KEY UPDATE 渲染。
		return
	case OnConflictError:
		db.ClauseBuilders[mysql.ClauseOnConflict] = func(c clause.Clause, builder clause.Builder) {
			if stmt, ok := builder.(*gorm.Statement); ok {
				stmt.AddError(ErrOnConflictUnsupported)
			}
		}
	default:
		// 什么都不写：UNIQUE KEY 表的 INSERT 已是 upsert。
		db.ClauseBuilders[mysql.ClauseOnConflict] = func(clause.Clause, clause.Builder) {}
	}
}

// Migrator 返回 Doris 迁移器。
func (d Dialector) Migrator(db *gorm.DB) gorm.Migrator {
	return Migrator{
		Migrator: mysql.Migrator{
			Migrator: migrator.Migrator{
				Config: migrator.Config{DB: db, Dialector: d},
			},
			Dialector: *d.Dialector,
		},
		dialector: d,
	}
}

// BindVarTo / QuoteTo / Explain 沿用 mysql 方言：Doris 吃 `?`、反引号和 MySQL 字面量。
func (d Dialector) BindVarTo(writer clause.Writer, stmt *gorm.Statement, v interface{}) {
	d.Dialector.BindVarTo(writer, stmt, v)
}

func (d Dialector) QuoteTo(writer clause.Writer, str string) {
	d.Dialector.QuoteTo(writer, str)
}

func (d Dialector) Explain(sql string, vars ...interface{}) string {
	return d.Dialector.Explain(sql, vars...)
}

// DefaultValueOf 与 mysql 一致。
func (d Dialector) DefaultValueOf(field *schema.Field) clause.Expression {
	return clause.Expr{SQL: "DEFAULT"}
}

// SavePoint / RollbackTo：Doris 没有事务语义，显式报错而不是静默失败。
// 建议 gorm.Config 里加 SkipDefaultTransaction: true，省掉 gorm 默认包的事务。
func (d Dialector) SavePoint(tx *gorm.DB, name string) error {
	return fmt.Errorf("doris: 不支持 SAVEPOINT（%s）", name)
}

func (d Dialector) RollbackTo(tx *gorm.DB, name string) error {
	return fmt.Errorf("doris: 不支持 ROLLBACK TO SAVEPOINT（%s）", name)
}

// keyType 解析实际使用的表模型。
func (d Dialector) keyType(fields []*schema.Field) string {
	if d.Options != nil && d.Options.KeyType != "" {
		return string(d.Options.KeyType)
	}
	if len(fields) > 0 {
		return string(KeyUnique)
	}
	return string(KeyDuplicate)
}

// defaultStringSize 返回 VARCHAR 默认长度。
func (d Dialector) defaultStringSize() uint {
	if d.Options != nil && d.Options.DefaultStringSize > 0 {
		return d.Options.DefaultStringSize
	}
	return 65533
}

// buckets 返回分桶数。
func (d Dialector) buckets() int {
	if d.Options != nil && d.Options.Buckets > 0 {
		return d.Options.Buckets
	}
	return 1
}

// properties 返回 PROPERTIES 内容（含默认 replication_num）。
// keyType 只在自增列时用到：merge-on-write 是 UNIQUE 模型的专有属性，DUPLICATE 表不能写。
func (d Dialector) properties(hasAutoIncrement bool, keyType string) map[string]string {
	out := map[string]string{}
	replication := "1"
	if d.Options != nil {
		if d.Options.ReplicationNum != "" {
			replication = d.Options.ReplicationNum
		}
		for k, v := range d.Options.Properties {
			out[k] = v
		}
	}
	if _, ok := out["replication_num"]; !ok {
		out["replication_num"] = replication
	}
	if hasAutoIncrement && keyType == string(KeyUnique) {
		// UNIQUE 表的自增列要求 merge-on-write（2.1+ 默认已开，这里显式写死）。
		if _, ok := out["enable_unique_key_merge_on_write"]; !ok {
			out["enable_unique_key_merge_on_write"] = "true"
		}
	}
	return out
}

// 编译期接口约束。
var _ gorm.Dialector = Dialector{}
