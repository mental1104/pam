package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mental1104/pam/internal/domain"
)

// testService 使用 PAM_TEST_DATABASE_URL 中已经迁移的隔离数据库；缺失时明确跳过，不连接生产库。
// t 管理连接池清理；返回的 Service 在该测试结束前有效。
func testService(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("PAM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PAM_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return New(pool), pool
}

// uniqueSource 返回随机的非敏感测试来源，避免并发测试或重复运行互相污染。
// t 用于报告系统随机源故障。
func uniqueSource(t *testing.T) string {
	t.Helper()
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatal(err)
	}
	return "fixture-" + hex.EncodeToString(bytes[:])
}

// TestImportIdempotencyAndTraceability 实测批次重放、外部编号去重、可疑重复确认和来源链；t 使用隔离 PostgreSQL。
func TestImportIdempotencyAndTraceability(t *testing.T) {
	s, pool := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	row := fixtureRow()
	row.ExternalID = "external:1"
	req := domain.ImportRequest{Source: uniqueSource(t), IdempotencyKey: "first", Records: []domain.ImportRecord{row}}
	first, err := s.Import(ctx, req)
	if err != nil || first.Created != 1 {
		t.Fatalf("initial import: %+v %v", first, err)
	}
	replay, err := s.Import(ctx, req)
	if err != nil || replay.Created != 0 || replay.Skipped != 1 || !replay.Replayed {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	req.IdempotencyKey = "second"
	second, err := s.Import(ctx, req)
	if err != nil || second.Skipped != 1 {
		t.Fatalf("stable external ID: %+v %v", second, err)
	}
	req.Records[0].Amount = "1000"
	req.IdempotencyKey = "conflict"
	conflict, err := s.Import(ctx, req)
	if err != nil || conflict.Duplicated != 1 {
		t.Fatalf("changed external record: %+v %v", conflict, err)
	}
	req.Records[0].ExternalID = ""
	req.Records[0].Amount = row.Amount
	req.IdempotencyKey = "candidate"
	candidate, err := s.Import(ctx, req)
	if err != nil || candidate.Duplicated != 1 {
		t.Fatalf("fingerprint candidate: %+v %v", candidate, err)
	}
	req.Records[0].ConfirmNew = true
	req.IdempotencyKey = "confirmed"
	confirmed, err := s.Import(ctx, req)
	if err != nil || confirmed.Created != 1 {
		t.Fatalf("confirmed repeated purchase: %+v %v", confirmed, err)
	}
	var count int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM pam.import_records r JOIN pam.import_batches b ON b.id=r.batch_id WHERE b.source=$1", req.Source).Scan(&count); err != nil || count != 5 {
		t.Fatalf("original provenance count=%d: %v", count, err)
	}
	got, err := s.GetTransaction(ctx, *first.Rows[0].TransactionID)
	if err != nil || got.Amount != "999.0000" || !strings.Contains(string(got.Original), "Yifang City") {
		t.Fatalf("transaction provenance: %+v %v", got, err)
	}
	req.IdempotencyKey = "first"
	if _, err = s.Import(ctx, req); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("reused key changed payload must conflict: %v", err)
	}
}

// TestImportConcurrentAndPartialFailures 实测并发相同批次只创建一次，并在单行错误后保留其他合法行；t 使用隔离数据库。
func TestImportConcurrentAndPartialFailures(t *testing.T) {
	s, _ := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	valid := fixtureRow()
	valid.RowKey = "line:1"
	invalid := valid
	invalid.Amount = "-5"
	invalid.RowKey = "line:2"
	req := domain.ImportRequest{Source: uniqueSource(t), IdempotencyKey: "concurrent", SourceFileHash: strings.Repeat("a", 64), Records: []domain.ImportRecord{valid, invalid}}
	var group sync.WaitGroup
	results := make(chan domain.ImportResult, 2)
	errs := make(chan error, 2)
	// 两个调用同时竞争同一数据库幂等边界，必须只有一个报告创建。
	for range 2 {
		group.Add(1)
		go func() { defer group.Done(); result, err := s.Import(ctx, req); results <- result; errs <- err }()
	}
	group.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	created := 0
	for result := range results {
		created += result.Created
		if result.Failed != 1 {
			t.Fatalf("expected failed source row: %+v", result)
		}
	}
	if created != 1 {
		t.Fatalf("concurrent import created %d records", created)
	}
	req.IdempotencyKey = "same-file"
	again, err := s.Import(ctx, req)
	if err != nil || again.Skipped != 1 || again.Failed != 1 {
		t.Fatalf("same file row identity: %+v %v", again, err)
	}
}

// TestAssetCRUDAndTransactionFilters 实测精确金额库存增改删与搜索/日期/金额分页；t 使用隔离数据库。
func TestAssetCRUDAndTransactionFilters(t *testing.T) {
	s, _ := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req := domain.ImportRequest{Source: uniqueSource(t), IdempotencyKey: "filter", Records: []domain.ImportRecord{fixtureRow()}}
	result, err := s.Import(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ListTransactions(ctx, domain.TransactionFilter{Source: req.Source, Search: "Yifang", Direction: "expense", From: "2025-01-01T00:00:00Z", To: "2026-01-01T00:00:00Z", MinAmount: "998.99", MaxAmount: "999", Limit: 1})
	if err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("filtered transactions: %+v %v", page, err)
	}
	price := "999.00"
	input := domain.AssetInput{Name: "Synthetic Gadget candidate", Currency: "CNY", PurchasePrice: &price, RelatedTransactionID: result.Rows[0].TransactionID, Notes: "User-confirmed association required"}
	asset, err := s.CreateAsset(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DeleteAsset(context.Background(), asset.ID)
	input.Status = "stored"
	updated, err := s.UpdateAsset(ctx, asset.ID, input)
	if err != nil || updated.Status != "stored" || updated.PurchasePrice == nil || *updated.PurchasePrice != "999.0000" {
		t.Fatalf("updated inventory: %+v %v", updated, err)
	}
	if err = s.DeleteAsset(ctx, asset.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetAsset(ctx, asset.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted inventory lookup: %v", err)
	}
}
