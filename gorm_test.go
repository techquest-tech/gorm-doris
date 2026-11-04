package doris

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// captureLogger 收集 gorm 在 DryRun 下生成但不执行的 SQL。
type captureLogger struct {
	mu   sync.Mutex
	sqls []string
}

func (l *captureLogger) LogMode(logger.LogLevel) logger.Interface      { return l }
func (l *captureLogger) Info(context.Context, string, ...interface{})  {}
func (l *captureLogger) Warn(context.Context, string, ...interface{})  {}
func (l *captureLogger) Error(context.Context, string, ...interface{}) {}

func (l *captureLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sqls = append(l.sqls, sql)
}

func (l *captureLogger) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.sqls, ";\n")
}

// dryRunDB 建一个不连库的 gorm.DB：SkipInitializeWithVersion 跳过 SELECT VERSION()，
// DisableAutomaticPing 跳过握手，DryRun 只生成 SQL。
func dryRunDB(t *testing.T, mutate ...func(*Options)) (*gorm.DB, *captureLogger) {
	t.Helper()
	opts := Options{
		DSN:                       "user:pass@tcp(127.0.0.1:9030)/demo?charset=utf8mb4&parseTime=True",
		SkipInitializeWithVersion: true,
	}
	for _, fn := range mutate {
		fn(&opts)
	}
	lg := &captureLogger{}
	db, err := gorm.Open(New(opts), &gorm.Config{
		DryRun:                 true,
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 lg,
	})
	if err != nil {
		t.Fatalf("gorm.Open 失败: %v", err)
	}
	return db, lg
}

type testOrder struct {
	ID        uint    `gorm:"primaryKey;autoIncrement"`
	OrderNo   string  `gorm:"size:64;comment:订单号"`
	Amount    float64 `gorm:"precision:18;scale:2"`
	Qty       int32
	CreatedAt time.Time
}

func TestCreateTableSQL(t *testing.T) {
	db, lg := dryRunDB(t)
	if err := db.Migrator().CreateTable(&testOrder{}); err != nil {
		t.Fatalf("CreateTable 失败: %v", err)
	}
	sqlText := lg.all()

	wants := []string{
		"CREATE TABLE IF NOT EXISTS `test_orders`",
		"`id` bigint NOT NULL AUTO_INCREMENT",
		"`order_no` varchar(64) NULL COMMENT '订单号'",
		"`amount` decimal(18, 2) NULL",
		"`qty` int NULL",
		"`created_at` datetime NULL",
		"UNIQUE KEY (`id`)",
		"DISTRIBUTED BY HASH(`id`) BUCKETS 1",
		`"replication_num" = "1"`,
		`"enable_unique_key_merge_on_write" = "true"`,
	}
	for _, want := range wants {
		if !strings.Contains(sqlText, want) {
			t.Errorf("建表 SQL 缺少 %q\n实际:\n%s", want, sqlText)
		}
	}
}

func TestCreateTableWithoutPrimaryKeyUsesDuplicateKey(t *testing.T) {
	db, lg := dryRunDB(t)
	type noPK struct {
		Name  string
		Value int64
	}
	if err := db.Migrator().CreateTable(&noPK{}); err != nil {
		t.Fatalf("CreateTable 失败: %v", err)
	}
	sqlText := lg.all()
	if !strings.Contains(sqlText, "DUPLICATE KEY (`name`)") {
		t.Errorf("无主键表应回退 DUPLICATE KEY，实际:\n%s", sqlText)
	}
	if strings.Contains(sqlText, "AUTO_INCREMENT") {
		t.Errorf("无自增列不应该出现 AUTO_INCREMENT:\n%s", sqlText)
	}
}

func TestKeyColumnsOption(t *testing.T) {
	db, lg := dryRunDB(t, func(o *Options) {
		o.KeyType = KeyAggregate
		o.KeyColumns = []string{"OrderNo"}
		o.Buckets = 8
		o.ReplicationNum = "3"
	})
	if err := db.Migrator().CreateTable(&testOrder{}); err != nil {
		t.Fatalf("CreateTable 失败: %v", err)
	}
	sqlText := lg.all()
	for _, want := range []string{
		"AGGREGATE KEY (`order_no`)",
		"DISTRIBUTED BY HASH(`order_no`) BUCKETS 8",
		`"replication_num" = "3"`,
		"`order_no` varchar(64) NOT NULL",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("建表 SQL 缺少 %q\n实际:\n%s", want, sqlText)
		}
	}
}

func TestDuplicateModelSkipsMergeOnWriteProperty(t *testing.T) {
	db, lg := dryRunDB(t, func(o *Options) { o.KeyType = KeyDuplicate })
	if err := db.Migrator().CreateTable(&testOrder{}); err != nil {
		t.Fatalf("CreateTable 失败: %v", err)
	}
	sqlText := lg.all()
	if !strings.Contains(sqlText, "DUPLICATE KEY (`id`)") {
		t.Errorf("应当使用 DUPLICATE 模型:\n%s", sqlText)
	}
	if strings.Contains(sqlText, "enable_unique_key_merge_on_write") {
		t.Errorf("DUPLICATE 表不该写 merge-on-write 属性:\n%s", sqlText)
	}
}

func TestAddColumnSQL(t *testing.T) {
	db, lg := dryRunDB(t)
	if err := db.Migrator().AddColumn(&testOrder{}, "Qty"); err != nil {
		t.Fatalf("AddColumn 失败: %v", err)
	}
	want := "ALTER TABLE `test_orders` ADD COLUMN `qty` int NULL"
	if sqlText := lg.all(); !strings.Contains(sqlText, want) {
		t.Errorf("补列 SQL 缺少 %q\n实际:\n%s", want, sqlText)
	}
}

func TestOnConflictDropByDefault(t *testing.T) {
	db, lg := dryRunDB(t)
	err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&testOrder{OrderNo: "A1"}).Error
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	sqlText := lg.all()
	if strings.Contains(sqlText, "ON DUPLICATE KEY UPDATE") || strings.Contains(sqlText, "ON CONFLICT") {
		t.Errorf("默认应当丢掉 ON CONFLICT 子句，实际:\n%s", sqlText)
	}
	if !strings.Contains(sqlText, "INSERT INTO `test_orders`") {
		t.Errorf("INSERT 语句不对:\n%s", sqlText)
	}
}

func TestOnConflictDuplicateKeyMode(t *testing.T) {
	db, lg := dryRunDB(t, func(o *Options) { o.OnConflict = OnConflictDuplicateKey })
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&testOrder{OrderNo: "A1"}).Error; err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if sqlText := lg.all(); !strings.Contains(sqlText, "ON DUPLICATE KEY UPDATE") {
		t.Errorf("duplicate-key 模式应渲染 MySQL 语法，实际:\n%s", sqlText)
	}
}

func TestOnConflictErrorMode(t *testing.T) {
	db, _ := dryRunDB(t, func(o *Options) { o.OnConflict = OnConflictError })
	err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&testOrder{OrderNo: "A1"}).Error
	if !errors.Is(err, ErrOnConflictUnsupported) {
		t.Fatalf("error 模式应返回 ErrOnConflictUnsupported，实际: %v", err)
	}
}

func TestDataTypeOf(t *testing.T) {
	type typeModel struct {
		Big     int64     `gorm:"column:big"`
		Small   int8      `gorm:"column:small"`
		U8      uint8     `gorm:"column:u8"`
		U32     uint32    `gorm:"column:u32"`
		U64     uint64    `gorm:"column:u64"`
		F32     float32   `gorm:"column:f32"`
		F64     float64   `gorm:"column:f64"`
		Price   float64   `gorm:"column:price;precision:18;scale:2"`
		Name    string    `gorm:"column:name;size:64"`
		Unbound string    `gorm:"column:unbound"`
		TextCol string    `gorm:"column:text_col;type:text"`
		Raw     []byte    `gorm:"column:raw"`
		Flag    bool      `gorm:"column:flag"`
		TS      time.Time `gorm:"column:ts"`
		TSMicro time.Time `gorm:"column:ts_micro;precision:3"`
		JSONCol string    `gorm:"column:json_col;type:jsonb"`
		Varchar string    `gorm:"column:varchar_col;type:varchar"`
	}

	s, err := schema.Parse(&typeModel{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("schema.Parse 失败: %v", err)
	}
	dialector := New(Options{}).(*Dialector)
	cases := map[string]string{
		"big":         "bigint",
		"small":       "tinyint",
		"u8":          "smallint",
		"u32":         "bigint",
		"u64":         "largeint",
		"f32":         "float",
		"f64":         "double",
		"price":       "decimal(18, 2)",
		"name":        "varchar(64)",
		"unbound":     "varchar(65533)",
		"text_col":    "string",
		"raw":         "string",
		"flag":        "boolean",
		"ts":          "datetime",
		"ts_micro":    "datetime(3)",
		"json_col":    "json",
		"varchar_col": "varchar(65533)",
	}
	for column, want := range cases {
		field := s.LookUpField(column)
		if field == nil {
			t.Fatalf("字段 %s 不存在", column)
		}
		if got := dialector.DataTypeOf(field); got != want {
			t.Errorf("%s 类型 = %q, want %q", column, got, want)
		}
	}
}

func TestDialectorName(t *testing.T) {
	if got := New(Options{}).Name(); got != "doris" {
		t.Errorf("Name() = %q, want doris", got)
	}
}

type customTable struct {
	ID   uint `gorm:"primaryKey"`
	Name string
}

func (customTable) TableName() string { return "custom_order" }

func TestCustomTableName(t *testing.T) {
	db, lg := dryRunDB(t)
	if err := db.Migrator().CreateTable(&customTable{}); err != nil {
		t.Fatalf("CreateTable 失败: %v", err)
	}
	if sqlText := lg.all(); !strings.Contains(sqlText, "CREATE TABLE IF NOT EXISTS `custom_order`") {
		t.Errorf("没有用 TableName() 指定的表名:\n%s", sqlText)
	}
}

func TestCreateViewSQL(t *testing.T) {
	db, lg := dryRunDB(t)
	sub := db.Model(&testOrder{}).Select("id", "order_no")
	if err := db.Migrator().CreateView("order_view", gorm.ViewOption{Replace: true, Query: sub}); err != nil {
		t.Fatalf("CreateView 失败: %v", err)
	}
	want := "CREATE OR REPLACE VIEW `order_view` AS SELECT `id`,`order_no` FROM `test_orders`"
	if sqlText := lg.all(); !strings.Contains(sqlText, want) {
		t.Errorf("视图 SQL 不符合预期，缺少 %q\n实际:\n%s", want, sqlText)
	}
}

// CheckOption 走 gorm 原生透传；Doris 不支持 WITH CHECK OPTION，服务端会报错，
// 所以这里只断言 SQL 形态（DryRun 不执行）。
func TestCreateViewCheckOptionPassedThrough(t *testing.T) {
	db, lg := dryRunDB(t)
	err := db.Migrator().CreateView("order_view", gorm.ViewOption{
		Query:       db.Model(&testOrder{}),
		CheckOption: "WITH CHECK OPTION",
	})
	if err != nil {
		t.Fatalf("DryRun 下不该报错: %v", err)
	}
	if sqlText := lg.all(); !strings.Contains(sqlText, "WITH CHECK OPTION") {
		t.Errorf("CheckOption 应当原样拼进 SQL:\n%s", sqlText)
	}
}

func TestDropViewSQL(t *testing.T) {
	db, lg := dryRunDB(t)
	if err := db.Migrator().DropView("order_view"); err != nil {
		t.Fatalf("DropView 失败: %v", err)
	}
	if sqlText := lg.all(); !strings.Contains(sqlText, "DROP VIEW IF EXISTS `order_view`") {
		t.Errorf("删视图 SQL 不符合预期:\n%s", sqlText)
	}
}

func TestCreateMultipleViews(t *testing.T) {
	db, lg := dryRunDB(t)
	views := map[string]string{
		"v_b": "SELECT * FROM `test_orders`",
		"v_a": "SELECT 1",
	}
	for name, query := range views {
		if err := db.Migrator().CreateView(name, gorm.ViewOption{Replace: true, Query: db.Raw(query)}); err != nil {
			t.Fatalf("建视图 %s 失败: %v", name, err)
		}
	}
	sqlText := lg.all()
	for _, want := range []string{"CREATE OR REPLACE VIEW `v_a` AS", "CREATE OR REPLACE VIEW `v_b` AS"} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("缺少 %q\n实际:\n%s", want, sqlText)
		}
	}
}
