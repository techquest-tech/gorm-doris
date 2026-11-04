package doris

import (
	"fmt"
	"os"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type liveRow struct {
	ID        uint `gorm:"primaryKey;autoIncrement"`
	Name      string
	N         int64
	CreatedAt time.Time
}

// TestLive 打真机：需要 DORIS_DSN 环境变量，未设置则跳过。
//
//	DORIS_DSN='user:pass@tcp(fe.host:9030)/demo?charset=utf8mb4&parseTime=True&loc=Local' \
//	  go test -run TestLive -v ./...
//
// 会建一张 gorm_doris_smoke_<时间戳> 的 UNIQUE KEY 表，跑完删除。
func TestLive(t *testing.T) {
	dsn := os.Getenv("DORIS_DSN")
	if dsn == "" {
		t.Skip("未设置 DORIS_DSN，跳过真机测试")
	}

	db, err := gorm.Open(New(Options{DSN: dsn}), &gorm.Config{
		SkipDefaultTransaction: true, // Doris 没有多语句事务
		Logger:                 logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}

	table := fmt.Sprintf("gorm_doris_smoke_%d", time.Now().Unix())
	// 每条语句用独立 session：gorm 的 Table() 会话会累积 Where 条件与上一次的错误。
	tableDB := func() *gorm.DB { return db.Table(table) }

	if err := tableDB().Migrator().CreateTable(&liveRow{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	defer func() {
		if err := tableDB().Migrator().DropTable(&liveRow{}); err != nil {
			t.Logf("清理表失败（可手工删 %s）: %v", table, err)
		}
	}()

	row := liveRow{Name: "alice", N: 7}
	if err := tableDB().Create(&row).Error; err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	var got liveRow
	if err := tableDB().Where("name = ?", "alice").First(&got).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.N != 7 {
		t.Fatalf("查询结果不对: %+v", got)
	}

	// Doris UNIQUE KEY 表：同一 key 再 INSERT 就是覆盖（upsert）。
	again := liveRow{ID: got.ID, Name: "alice", N: 8}
	if err := tableDB().Create(&again).Error; err != nil {
		t.Fatalf("覆盖插入失败: %v", err)
	}
	var upserted liveRow
	if err := tableDB().Where("id = ?", got.ID).First(&upserted).Error; err != nil {
		t.Fatalf("覆盖后查询失败: %v", err)
	}
	if upserted.N != 8 {
		t.Fatalf("UNIQUE KEY 覆盖没生效: %+v", upserted)
	}

	if err := tableDB().Model(&liveRow{}).Where("id = ?", got.ID).Update("n", 9).Error; err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	var count int64
	if err := tableDB().Model(&liveRow{}).Count(&count).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("应当只有 1 行，实际 %d 行", count)
	}

	if err := tableDB().Where("id = ?", got.ID).Delete(&liveRow{}).Error; err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	// 视图：CREATE OR REPLACE VIEW → 查视图 → 删视图（defer 后进先出，先删视图再删表）
	view := table + "_v"
	if err := db.Migrator().CreateView(view, gorm.ViewOption{
		Replace: true,
		Query:   db.Raw("SELECT `id`,`name` FROM `" + table + "`"),
	}); err != nil {
		t.Fatalf("建视图失败: %v", err)
	}
	defer func() {
		if err := db.Migrator().DropView(view); err != nil {
			t.Logf("清理视图失败（可手工删 %s）: %v", view, err)
		}
	}()

	var vrows []liveRow
	if err := db.Table(view).Find(&vrows).Error; err != nil {
		t.Fatalf("查视图失败: %v", err)
	}
	t.Logf("视图 %s 建好并可查询，行数=%d", view, len(vrows))
}

// TestLiveDefaultTransaction 单独探一次：Doris 能否接受 gorm 默认包的事务
// （go-sql-driver 会发 START TRANSACTION / COMMIT）。失败不影响主流程，只用于记录结论。
func TestLiveDefaultTransaction(t *testing.T) {
	dsn := os.Getenv("DORIS_DSN")
	if dsn == "" {
		t.Skip("未设置 DORIS_DSN，跳过真机测试")
	}

	db, err := gorm.Open(New(Options{DSN: dsn}), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}

	table := fmt.Sprintf("gorm_doris_tx_probe_%d", time.Now().Unix())
	type probeRow struct {
		ID   uint `gorm:"primaryKey;autoIncrement"`
		Name string
	}
	if err := db.Table(table).Migrator().CreateTable(&probeRow{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	defer func() {
		if err := db.Table(table).Migrator().DropTable(&probeRow{}); err != nil {
			t.Logf("清理表失败（可手工删 %s）: %v", table, err)
		}
	}()

	err = db.Table(table).Create(&probeRow{Name: "tx-probe"}).Error
	if err != nil {
		t.Logf("结论：Doris 不接受 gorm 默认事务，必须 SkipDefaultTransaction=true；错误: %v", err)
		return
	}
	t.Log("结论：Doris 接受 gorm 默认事务（START TRANSACTION/COMMIT 被接受）")
}

// TestLiveAutoMigrate 验证 AutoMigrate 的补列路径：
// 先用瘦模型建表，再用全模型 AutoMigrate，应当补出缺的列。
func TestLiveAutoMigrate(t *testing.T) {
	dsn := os.Getenv("DORIS_DSN")
	if dsn == "" {
		t.Skip("未设置 DORIS_DSN，跳过真机测试")
	}
	db, err := gorm.Open(New(Options{DSN: dsn}), &gorm.Config{
		SkipDefaultTransaction: true,
		Logger:                 logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}

	type slim struct {
		ID   uint `gorm:"primaryKey;autoIncrement"`
		Name string
	}
	type full struct {
		ID      uint `gorm:"primaryKey;autoIncrement"`
		Name    string
		Extra   int64
		Comment string `gorm:"comment:补出来的列"`
	}

	table := fmt.Sprintf("gorm_doris_automigrate_%d", time.Now().Unix())
	tableDB := func() *gorm.DB { return db.Table(table) }

	if err := tableDB().Migrator().CreateTable(&slim{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	defer func() {
		if err := tableDB().Migrator().DropTable(&full{}); err != nil {
			t.Logf("清理表失败（可手工删 %s）: %v", table, err)
		}
	}()

	if err := tableDB().AutoMigrate(&full{}); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}
	for _, col := range []string{"Extra", "Comment"} {
		if !tableDB().Migrator().HasColumn(&full{}, col) {
			t.Errorf("AutoMigrate 没有补出列 %s", col)
		}
	}
}
