package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mental1104/pam/internal/domain"
	"testing"
)

// TestProposalReviewLifecycle 在隔离数据库验证预检不入账、重叠增量、跨来源待核和更正历史。
func TestProposalReviewLifecycle(t *testing.T) {
	s, pool := testService(t)
	ctx := context.Background()
	source := uniqueSource(t)
	row := fixtureRow()
	row.Merchant = source
	row.RowKey = "r1"
	row.ExternalID = ""
	req := domain.ImportRequest{Source: source, ReviewCrossSource: true, IdempotencyKey: "image-one", SourceFileHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Records: []domain.ImportRecord{row}}
	proposal, err := s.Propose(ctx, req)
	if err != nil || proposal.Preview.Created != 1 {
		t.Fatalf("preview: %+v %v", proposal, err)
	}
	var count int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM pam.transactions WHERE source=$1", source).Scan(&count); err != nil || count != 0 {
		t.Fatalf("preview wrote ledger: %d %v", count, err)
	}
	restored, err := New(pool).Proposal(ctx, proposal.ID)
	if err != nil || restored.ID != proposal.ID {
		t.Fatalf("restore: %v", err)
	}
	imported, err := s.Confirm(ctx, proposal.ID)
	if err != nil || imported.Created != 1 {
		t.Fatalf("confirm: %+v %v", imported, err)
	}
	replay, err := s.Confirm(ctx, proposal.ID)
	if err != nil || replay.Created != 0 || replay.Skipped != 1 {
		t.Fatalf("confirm replay: %+v %v", replay, err)
	}
	req.IdempotencyKey = "overlap"
	newRow := row
	newRow.RowKey = "r2"
	newRow.OccurredAt = "2025-01-02T12:05:00+08:00"
	newRow.Amount = "999.99"
	req.Records = append(req.Records, newRow)
	proposal, err = s.Propose(ctx, req)
	if err != nil || proposal.Preview.Created != 1 || proposal.Preview.Skipped != 1 {
		t.Fatalf("overlap preview: %+v %v", proposal, err)
	}
	imported, err = s.Confirm(ctx, proposal.ID)
	if err != nil || imported.Created != 1 || imported.Skipped != 1 {
		t.Fatalf("overlap import: %+v %v", imported, err)
	}
	// 相同可见字段来自另一张图或另一平台，仅持久待核，不自动合并或删除。
	req.Source = uniqueSource(t)
	req.IdempotencyKey = "cross-source"
	req.SourceFileHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	req.Records = []domain.ImportRecord{row}
	imported, err = s.Import(ctx, req)
	if err != nil || imported.Duplicated != 1 {
		t.Fatalf("cross source: %+v %v", imported, err)
	}
	reviews, err := New(pool).Reviews(ctx, "pending", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	var review domain.ImportReview
	for _, v := range reviews.Items {
		if v.Source == req.Source {
			review = v
			break
		}
	}
	if review.ID == "" {
		t.Fatal("persistent review missing")
	}
	decision := domain.ReviewDecision{Action: "link", Reason: "Human checked original image and confirmed same payment"}
	if _, err = s.Resolve(ctx, review.ID, decision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Resolve(ctx, review.ID, decision); err != nil {
		t.Fatalf("decision replay: %v", err)
	}
	// 同一图同一行修改金额应成为历史更正候选，人工确认才改账本。
	req.Source = source
	req.SourceFileHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	req.IdempotencyKey = "corrected-image"
	req.Records[0].Amount = "18.25"
	imported, err = s.Import(ctx, req)
	if err != nil || imported.Duplicated != 1 || imported.Rows[0].Reason != "source_row_changed" {
		t.Fatalf("correction: %+v %v", imported, err)
	}
	reviews, err = s.Reviews(ctx, "pending", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range reviews.Items {
		if v.Source == source && v.Kind == "correction" {
			review = v
			break
		}
	}
	decision = domain.ReviewDecision{Action: "correct", Reason: "Original screenshot amount manually verified", Record: &req.Records[0]}
	if _, err = s.Resolve(ctx, review.ID, decision); err != nil {
		t.Fatal(err)
	}
	var before, after json.RawMessage
	err = pool.QueryRow(ctx, "SELECT before_content,after_content FROM pam.transaction_revisions WHERE review_id=$1", review.ID).Scan(&before, &after)
	if err != nil || string(before) == string(after) {
		t.Fatalf("revision missing: %v", err)
	}
	var amount string
	err = pool.QueryRow(ctx, "SELECT amount::text FROM pam.transactions WHERE id=$1", review.TransactionID).Scan(&amount)
	if err != nil || amount != "18.2500" {
		t.Fatalf("correction result: %s %v", amount, err)
	}
	// 解析异常保留原始诊断和待核，不成为交易。
	invalid := row
	invalid.RowKey = "invalid"
	invalid.Original = json.RawMessage(`{"pam_parse_issues":["missing_date"],"pam_human_reviewed":false}`)
	req.IdempotencyKey = "invalid-parser"
	req.Records = []domain.ImportRecord{invalid}
	imported, err = s.Import(ctx, req)
	if err != nil || imported.Failed != 1 {
		t.Fatalf("parser errors: %+v %v", imported, err)
	}
	ranges, err := s.SourceRanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range ranges {
		if len(v.ConfirmedCoverage) != 0 {
			t.Fatal("invented complete coverage")
		}
	}
}

// TestReviewCreateAndConcurrentDecision 证明同额独立交易可经人工确认新增，重复决定及竞争决定不会多写账本。
func TestReviewCreateAndConcurrentDecision(t *testing.T) {
	s, pool := testService(t)
	ctx := context.Background()
	source := uniqueSource(t)
	row := fixtureRow()
	row.Merchant = source
	row.RowKey = "one"
	other := row
	other.RowKey = "two"
	req := domain.ImportRequest{Source: source, IdempotencyKey: "two-identical", Records: []domain.ImportRecord{row, other}}
	proposal, err := s.Propose(ctx, req)
	if err != nil || proposal.Preview.Created != 1 || proposal.Preview.Duplicated != 1 {
		t.Fatalf("within proposal: %+v %v", proposal, err)
	}
	imported, err := s.Confirm(ctx, proposal.ID)
	if err != nil || imported.Created != 1 || imported.Duplicated != 1 {
		t.Fatalf("within batch: %+v %v", imported, err)
	}
	page, err := s.Reviews(ctx, "pending", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	id := ""
	for _, v := range page.Items {
		if v.Source == source {
			id = v.ID
		}
	}
	if id == "" {
		t.Fatal("duplicate review missing")
	}
	decision := domain.ReviewDecision{Action: "create", Reason: "Distinct image positions and independent balance changes checked", Record: &other}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := s.Resolve(ctx, id, decision); results <- err }()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM pam.transactions WHERE source=$1", source).Scan(&count); err != nil || count != 2 {
		t.Fatalf("concurrent confirmation: %d %v", count, err)
	}
	decision.Action = "dismiss"
	decision.Record = nil
	if _, err = s.Resolve(ctx, id, decision); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed decision should conflict: %v", err)
	}
	// 提案保存后另一导入抢先写入，确认必须重新查重。
	fresh := row
	fresh.Amount = "123.45"
	fresh.OccurredAt = "2026-01-02T12:30:00+08:00"
	req = domain.ImportRequest{Source: source, IdempotencyKey: "stale-preview", Records: []domain.ImportRecord{fresh}}
	proposal, err = s.Propose(ctx, req)
	if err != nil || proposal.Preview.Created != 1 {
		t.Fatal("stale preview setup failed")
	}
	req.IdempotencyKey = "intervening-import"
	if _, err = s.Import(ctx, req); err != nil {
		t.Fatal(err)
	}
	imported, err = s.Confirm(ctx, proposal.ID)
	if err != nil || imported.Created != 0 || imported.Duplicated != 1 {
		t.Fatalf("stale preview bypassed recheck: %+v %v", imported, err)
	}
}
