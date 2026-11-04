package doris

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Migrator 在 mysql 迁移器之上做 Doris 适配：
//
//	CreateTable / AddColumn  生成 Doris DDL（KEY 子句 + DISTRIBUTED BY + PROPERTIES，
//	                         key 列强制 NOT NULL，AUTO_INCREMENT 自动加 merge-on-write 属性）
//	MigrateColumn            空实现：Doris 的列类型变更限制多，交人工处理
//	索引与约束                Doris 没有二级索引/ALTER 期约束，全部空实现或返回 false
type Migrator struct {
	mysql.Migrator
	dialector Dialector
}

// CreateTable 生成 Doris 建表语句。
func (m Migrator) CreateTable(values ...interface{}) error {
	for _, value := range m.ReorderModels(values, false) {
		if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
			if stmt.Schema == nil {
				return errors.New("failed to get schema")
			}
			sqlText, err := m.buildCreateTableSQL(stmt)
			if err != nil {
				return err
			}
			return m.DB.Exec(sqlText).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// DropTable 用 Doris 语法删表。
// mysql 方言的实现会先发 `SET FOREIGN_KEY_CHECKS = 0`（Doris 没有这个系统变量），
// 并且拼 `DROP TABLE ... CASCADE`（Doris 的 DROP TABLE 不接受 CASCADE）。
func (m Migrator) DropTable(values ...interface{}) error {
	values = m.ReorderModels(values, false)
	for i := len(values) - 1; i >= 0; i-- {
		if err := m.RunWithValue(values[i], func(stmt *gorm.Statement) error {
			return m.DB.Exec("DROP TABLE IF EXISTS " + stmt.Quote(stmt.Table)).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// RenameTable 用 Doris 语法：`ALTER TABLE old RENAME new`（没有 MySQL 的 TO）。
func (m Migrator) RenameTable(oldName, newName interface{}) error {
	resolve := func(name interface{}) (string, error) {
		if v, ok := name.(string); ok {
			return v, nil
		}
		stmt := &gorm.Statement{DB: m.DB}
		if err := stmt.Parse(name); err != nil {
			return "", err
		}
		return stmt.Table, nil
	}
	oldTable, err := resolve(oldName)
	if err != nil {
		return err
	}
	newTable, err := resolve(newName)
	if err != nil {
		return err
	}
	q := &gorm.Statement{DB: m.DB}
	return m.DB.Exec("ALTER TABLE " + q.Quote(oldTable) + " RENAME " + q.Quote(newTable)).Error
}

// AddColumn 用 Doris 的 `ALTER TABLE ... ADD COLUMN` 语法补列。
func (m Migrator) AddColumn(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema == nil {
			return errors.New("failed to get schema")
		}
		field := stmt.Schema.LookUpField(name)
		if field == nil {
			return fmt.Errorf("failed to look up field with name: %s", name)
		}
		if field.IgnoreMigration {
			return nil
		}
		keyFields, _ := m.resolveKey(stmt)
		sqlText := "ALTER TABLE " + stmt.Quote(stmt.Table) +
			" ADD COLUMN " + stmt.Quote(field.DBName) + " " + m.columnDefinition(field, isKeyField(keyFields, field))
		return m.DB.Exec(sqlText).Error
	})
}

// MigrateColumn 空实现：Doris 的列变更（换类型、改 null、加聚合语义）都可能重写数据，
// 让 gorm 按类型字符串自动改列风险太大，需要变更时手写 ALTER TABLE。
func (m Migrator) MigrateColumn(value interface{}, field *schema.Field, columnType gorm.ColumnType) error {
	return nil
}

// 约束：Doris 的 key/唯一性由 KEY 子句与表模型表达，没有 ALTER 期约束。
func (m Migrator) CreateConstraint(value interface{}, name string) error { return nil }
func (m Migrator) DropConstraint(value interface{}, name string) error   { return nil }
func (m Migrator) HasConstraint(value interface{}, name string) bool     { return false }

// CreateIndex：Doris 没有 MySQL 式的二级索引，跳过并告警。
// 需要加速过滤请用倒排索引/BloomFilter：建表时写 `INDEX idx (col) USING INVERTED`，
// 或 `ALTER TABLE t SET ("bloom_filter_columns" = "col")`。
func (m Migrator) CreateIndex(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if m.DB.Logger != nil {
			m.DB.Logger.Warn(context.Background(),
				"doris: 跳过索引 %s.%s（Doris 无二级索引；倒排索引/BloomFilter 请手写 DDL）", stmt.Table, name)
		}
		return nil
	})
}

func (m Migrator) DropIndex(value interface{}, name string) error { return nil }
func (m Migrator) HasIndex(value interface{}, name string) bool   { return false }

func (m Migrator) RenameIndex(value interface{}, oldName, newName string) error {
	return fmt.Errorf("doris: 不支持重命名索引（%s → %s）", oldName, newName)
}

// GetTypeAliases 显式转发到 mysql 方言，避免与核心迁移器的同名方法产生歧义。
func (m Migrator) GetTypeAliases(databaseTypeName string) []string {
	return m.Migrator.Migrator.GetTypeAliases(databaseTypeName)
}

// resolveKey 决定表模型与 key 列。
//
// 优先级：Options.KeyColumns > 主键字段 > 第一个非忽略字段。
// 表模型：Options.KeyType 显式指定优先；否则有主键为 UNIQUE，其余为 DUPLICATE。
// Doris 要求 key 列必须是列定义的前缀，所以建表时会把 key 列排到最前。
func (m Migrator) resolveKey(stmt *gorm.Statement) ([]*schema.Field, string) {
	var explicit KeyType
	var keyColumns []string
	if m.dialector.Options != nil {
		explicit = m.dialector.Options.KeyType
		keyColumns = m.dialector.Options.KeyColumns
	}

	if len(keyColumns) > 0 {
		fields := make([]*schema.Field, 0, len(keyColumns))
		for _, name := range keyColumns {
			if f := stmt.Schema.LookUpField(name); f != nil {
				fields = append(fields, f)
			}
		}
		if len(fields) > 0 {
			return fields, string(keyTypeOr(explicit, KeyDuplicate))
		}
	}

	if len(stmt.Schema.PrimaryFields) > 0 {
		return stmt.Schema.PrimaryFields, string(keyTypeOr(explicit, KeyUnique))
	}

	for _, dbName := range stmt.Schema.DBNames {
		if f := stmt.Schema.FieldsByDBName[dbName]; f != nil && !f.IgnoreMigration {
			return []*schema.Field{f}, string(keyTypeOr(explicit, KeyDuplicate))
		}
	}
	return nil, ""
}

func keyTypeOr(explicit KeyType, fallback KeyType) KeyType {
	if explicit != "" {
		return explicit
	}
	return fallback
}

func isKeyField(keyFields []*schema.Field, field *schema.Field) bool {
	for _, k := range keyFields {
		if k == field || k.DBName == field.DBName {
			return true
		}
	}
	return false
}

// buildCreateTableSQL 拼 Doris 建表语句。key 列排到最前，其余列保持模型顺序。
func (m Migrator) buildCreateTableSQL(stmt *gorm.Statement) (string, error) {
	keyFields, keyType := m.resolveKey(stmt)
	if len(keyFields) == 0 || keyType == "" {
		return "", errors.New("doris: 模型没有可用的列，无法生成建表语句")
	}

	ordered := make([]*schema.Field, 0, len(stmt.Schema.Fields))
	ordered = append(ordered, keyFields...)
	for _, dbName := range stmt.Schema.DBNames {
		field := stmt.Schema.FieldsByDBName[dbName]
		if field == nil || field.IgnoreMigration || isKeyField(keyFields, field) {
			continue
		}
		ordered = append(ordered, field)
	}

	hasAutoIncrement := false
	for _, field := range ordered {
		if field.AutoIncrement {
			hasAutoIncrement = true
			break
		}
	}

	var b strings.Builder
	b.WriteString("CREATE TABLE IF NOT EXISTS ")
	b.WriteString(stmt.Quote(stmt.Table))
	b.WriteString(" (\n")
	for i, field := range ordered {
		b.WriteString("  ")
		b.WriteString(stmt.Quote(field.DBName))
		b.WriteByte(' ')
		b.WriteString(m.columnDefinition(field, isKeyField(keyFields, field)))
		if i < len(ordered)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString(")\n")

	b.WriteString(keyType)
	b.WriteString(" KEY (")
	for i, field := range keyFields {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(stmt.Quote(field.DBName))
	}
	b.WriteString(")\n")

	b.WriteString("DISTRIBUTED BY HASH(")
	b.WriteString(stmt.Quote(keyFields[0].DBName))
	b.WriteString(") BUCKETS ")
	b.WriteString(strconv.Itoa(m.dialector.buckets()))
	b.WriteString("\n")

	b.WriteString("PROPERTIES (\n")
	props := m.dialector.properties(hasAutoIncrement, keyType)
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		b.WriteString("  \"")
		b.WriteString(k)
		b.WriteString("\" = \"")
		b.WriteString(props[k])
		b.WriteString("\"")
		if i < len(keys)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString(")")
	return b.String(), nil
}

// columnDefinition 生成单个列定义：类型 + NULL 约束 + 自增/默认值 + 注释。
// Doris 要求 key 列 NOT NULL。
func (m Migrator) columnDefinition(field *schema.Field, key bool) string {
	def := m.dialector.DataTypeOf(field)
	if def == "" {
		def = fmt.Sprintf("varchar(%d)", m.dialector.defaultStringSize())
	}
	if key || field.NotNull || field.PrimaryKey || field.AutoIncrement {
		def += " NOT NULL"
	} else {
		def += " NULL"
	}
	switch {
	case field.AutoIncrement:
		// 自增列不能同时带 DEFAULT；Doris 2.1+ 需要 UNIQUE KEY + merge-on-write。
		def += " AUTO_INCREMENT"
	case field.HasDefaultValue && (field.DefaultValueInterface != nil || field.DefaultValue != ""):
		def += " DEFAULT " + defaultValueSQL(field)
	}
	if field.Comment != "" {
		def += " COMMENT " + sqlString(field.Comment)
	}
	return def
}

// defaultValueSQL 渲染列默认值：字面量走引号，其余当裸 SQL（如 CURRENT_TIMESTAMP）。
func defaultValueSQL(field *schema.Field) string {
	if field.DefaultValueInterface != nil {
		switch v := field.DefaultValueInterface.(type) {
		case string:
			return sqlString(v)
		case bool:
			if v {
				return "TRUE"
			}
			return "FALSE"
		default:
			return fmt.Sprint(v)
		}
	}
	if field.DefaultValue == "(-)" {
		return "NULL"
	}
	return field.DefaultValue
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// 编译期接口约束。
var _ gorm.Migrator = Migrator{}
