package doris

import (
	"fmt"
	"strings"

	"gorm.io/gorm/schema"
)

// DataTypeOf 把 GORM 字段映射成 Doris 列类型。
//
// 与 MySQL 方言的关键差异：
//   - Doris 没有 BOOLEAN 存储差异，boolean 就是 tinyint(1) 的别名；
//   - 无符号整数按「加宽到有符号」处理（uint8→smallint、uint32→bigint、uint64→largeint），
//     避免依赖不同版本对 UNSIGNED 的支持差异；
//   - VARCHAR 必须带长度：没有 size 标签时用 DefaultStringSize（默认 65533，Doris 上限）；
//     超过 65533 走 STRING；
//   - 大文本是 STRING，不是 TEXT；二进制没有原生类型，一律 STRING。
func (d Dialector) DataTypeOf(field *schema.Field) string {
	switch field.DataType {
	case schema.Bool:
		return "boolean"
	case schema.Int:
		return d.intType(field, false)
	case schema.Uint:
		return d.intType(field, true)
	case schema.Float:
		return d.floatType(field)
	case schema.String:
		return d.stringType(field)
	case schema.Time:
		return d.timeType(field)
	case schema.Bytes:
		return "string"
	default:
		return d.customType(field)
	}
}

func (d Dialector) intType(field *schema.Field, unsigned bool) string {
	// Doris 的自增列类型必须是 BIGINT，所以自增字段一律落到 bigint
	// （uint64 取值 > 2^63-1 时别用自增，改用外部生成的 id）。
	if field.AutoIncrement {
		return "bigint"
	}
	size := field.Size
	if unsigned {
		switch {
		case size <= 8:
			return "smallint"
		case size <= 16:
			return "int"
		case size <= 32:
			return "bigint"
		default:
			return "largeint"
		}
	}
	switch {
	case size <= 8:
		return "tinyint"
	case size <= 16:
		return "smallint"
	case size <= 32:
		return "int"
	case size <= 64:
		return "bigint"
	default:
		return "largeint"
	}
}

func (d Dialector) floatType(field *schema.Field) string {
	if field.Precision > 0 {
		if field.Scale > 0 {
			return fmt.Sprintf("decimal(%d, %d)", field.Precision, field.Scale)
		}
		return fmt.Sprintf("decimal(%d)", field.Precision)
	}
	if field.Size > 0 && field.Size <= 32 {
		return "float"
	}
	return "double"
}

func (d Dialector) stringType(field *schema.Field) string {
	size := field.Size
	if size <= 0 {
		size = int(d.defaultStringSize())
	}
	if size > 65533 {
		return "string"
	}
	return fmt.Sprintf("varchar(%d)", size)
}

func (d Dialector) timeType(field *schema.Field) string {
	if field.Precision > 0 {
		if field.Precision > 6 {
			field.Precision = 6
		}
		return fmt.Sprintf("datetime(%d)", field.Precision)
	}
	return "datetime"
}

// customType 处理 tag `type:xxx`：把 MySQL 的叫法翻成 Doris 的等价类型。
func (d Dialector) customType(field *schema.Field) string {
	value := strings.TrimSpace(string(field.DataType))
	if value == "" {
		return d.stringType(field)
	}
	lower := strings.ToLower(value)
	base := lower
	if i := strings.IndexByte(base, '('); i > 0 {
		base = strings.TrimSpace(base[:i])
	}

	switch base {
	case "text", "tinytext", "mediumtext", "longtext", "clob", "string":
		return "string"
	case "json", "jsonb":
		return "json"
	case "blob", "tinyblob", "mediumblob", "longblob", "varbinary", "binary", "bytea":
		return "string"
	case "bool", "boolean":
		return "boolean"
	case "datetime", "timestamp":
		if strings.Contains(lower, "(") {
			return value
		}
		return "datetime"
	case "varchar", "char", "character varying", "nvarchar":
		if strings.Contains(lower, "(") {
			return value
		}
		return fmt.Sprintf("varchar(%d)", d.defaultStringSize())
	case "numeric":
		if strings.Contains(lower, "(") {
			return value
		}
		return "decimal"
	}
	return value
}
