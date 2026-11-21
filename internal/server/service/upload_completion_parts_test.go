package service

import (
	"errors"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMatchUploadCompletionPartsReturnsSortedSnapshot(t *testing.T) {
	for _, tailSize := range []uint64{1, receiptFixturePartSize} {
		// 1. 声明可乱序，输出使用存储回执并保持固定计划的编号顺序。
		claims := []domain.UploadPartCompletionClaim{{PartNumber: 2, ETag: "Tail"}, {PartNumber: 1, ETag: "First"}}
		originalClaims := slices.Clone(claims)
		receipts := []domain.UploadPartReceipt{
			uploadPartReceiptFixture(1, receiptFixturePartSize, "First"),
			uploadPartReceiptFixture(2, tailSize, "Tail"),
		}
		want := slices.Clone(receipts)
		result, err := MatchUploadCompletionParts(receiptFixturePartSize+tailSize, receiptFixturePartSize, 2, claims, receipts)
		if err != nil || !slices.Equal(result, want) || !slices.Equal(claims, originalClaims) || !slices.Equal(receipts, want) {
			t.Fatal("完成快照内容、顺序或输入保护不符", err)
		}

		// 2. 复用原始输入切片后，结果仍保留编号、片长、ETag 和存储摘要。
		clear(receipts)
		clear(claims)
		if !slices.Equal(result, want) {
			t.Fatal("输入切片变化影响了完成快照")
		}
	}
}

func TestMatchUploadCompletionPartsReportsMismatches(t *testing.T) {
	const partSize = receiptFixturePartSize
	// 1. 第 2 片片长短、第 3 片的新回执已替换旧值、第 4 片缺存储回执，第 5 片缺客户端声明。
	claims := []domain.UploadPartCompletionClaim{
		{PartNumber: 4, ETag: "Missing"}, {PartNumber: 2, ETag: "Short"}, {PartNumber: 3, ETag: "Old"},
		{PartNumber: 6, ETag: "Tail"}, {PartNumber: 1, ETag: "First"},
	}
	receipts := []domain.UploadPartReceipt{
		uploadPartReceiptFixture(1, partSize, "First"),
		uploadPartReceiptFixture(2, partSize-1, "Short"),
		uploadPartReceiptFixture(3, partSize, "New"),
		uploadPartReceiptFixture(5, partSize, "Undeclared"),
		uploadPartReceiptFixture(6, 1, "tail"),
	}
	result, err := MatchUploadCompletionParts(5*partSize+1, partSize, 6, claims, receipts)
	var mismatch *domain.UploadPartsMismatchError
	if result != nil || !errors.Is(err, domain.ErrUploadPartsMismatch) || !errors.As(err, &mismatch) ||
		!slices.Equal(mismatch.PartNumbers, []uint32{2, 3, 4, 5, 6}) {
		t.Fatal("未按编号返回全部失配类型，含仅大小写不同的 ETag", mismatch)
	}

	// 2. 对较大计划，只返回编号最小的 20 项，每个编号只出现一次。
	const partCount = 32
	result, err = MatchUploadCompletionParts((partCount-1)*partSize+1, partSize, partCount, claims[len(claims)-1:], receipts[:1])
	want := make([]uint32, 20)
	for index := range want {
		want[index] = uint32(index + 2)
	}
	if result != nil || !errors.As(err, &mismatch) || !slices.Equal(mismatch.PartNumbers, want) {
		t.Fatal("失配编号上限、顺序或首项不符", mismatch)
	}
}

func TestMatchUploadCompletionPartsRejectsInvalidClaims(t *testing.T) {
	first := domain.UploadPartCompletionClaim{PartNumber: 1, ETag: "First"}
	for _, test := range []struct {
		name, field string
		claims      []domain.UploadPartCompletionClaim
	}{
		{name: "empty", field: "parts"},
		{name: "oversized", field: "parts", claims: make([]domain.UploadPartCompletionClaim, domain.MaxUploadPartCount+1)},
		{name: "duplicate", field: "parts", claims: []domain.UploadPartCompletionClaim{first, first}},
		{name: "beyond saved plan", field: "parts.1.part_number", claims: []domain.UploadPartCompletionClaim{first, {PartNumber: 3, ETag: "Third"}}},
		{name: "zero", field: "parts.0.part_number", claims: []domain.UploadPartCompletionClaim{{ETag: "First"}}},
		{name: "invalid ETag", field: "parts.1.etag", claims: []domain.UploadPartCompletionClaim{first, {PartNumber: 2, ETag: " Tail"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := MatchUploadCompletionParts(receiptFixturePartSize+1, receiptFixturePartSize, 2, test.claims, nil)
			var invalid *domain.UploadPartDeclarationError
			if result != nil || !errors.As(err, &invalid) || invalid.Field != test.field {
				t.Fatalf("预期字段错误 %s，得到 %v", test.field, err)
			}
		})
	}
}

func TestMatchUploadCompletionPartsRejectsMalformedPlanAndReceipts(t *testing.T) {
	const partSize = receiptFixturePartSize
	claims := []domain.UploadPartCompletionClaim{{PartNumber: 1, ETag: "First"}}
	first := uploadPartReceiptFixture(1, partSize, "First")
	for _, test := range []struct {
		name     string
		count    uint32
		receipts []domain.UploadPartReceipt
		want     error
	}{
		{"wrong plan", 31, nil, domain.ErrInvalidUploadTaskData},
		{"repeated receipt", 32, []domain.UploadPartReceipt{first, first}, domain.ErrUploadStorageUnavailable},
		{"unsorted receipt", 32, []domain.UploadPartReceipt{uploadPartReceiptFixture(2, partSize, "Second"), first}, domain.ErrUploadStorageUnavailable},
		{"number beyond plan", 32, []domain.UploadPartReceipt{first, uploadPartReceiptFixture(33, 1, "Outside")}, domain.ErrUploadStorageUnavailable},
		{"malformed ETag of a short part", 32, []domain.UploadPartReceipt{first, uploadPartReceiptFixture(30, 1, " NonCanonical")}, domain.ErrUploadStorageUnavailable},
		{"missing SHA beyond mismatch limit", 32, []domain.UploadPartReceipt{first, {PartNumber: 32, SizeBytes: 1, ETag: "Tail"}}, domain.ErrUploadStorageUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := MatchUploadCompletionParts(31*partSize+1, partSize, test.count, claims, test.receipts)
			if result != nil || !errors.Is(err, test.want) {
				t.Fatalf("异常输入应拒绝整批，预期 %v，得到 %v", test.want, err)
			}
		})
	}
}
