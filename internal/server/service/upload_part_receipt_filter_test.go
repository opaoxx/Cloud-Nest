package service

import (
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const receiptFixturePartSize = 16 << 20
const receiptFixtureChecksum = "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="

func uploadPartReceiptFixture(number uint32, size uint64, etag string) domain.UploadPartReceipt {
	return domain.UploadPartReceipt{PartNumber: number, SizeBytes: size, ETag: etag, ChecksumSHA256: receiptFixtureChecksum}
}

func TestFilterUploadPartReceiptsKeepsUsableParts(t *testing.T) {
	const fileSize = 4*receiptFixturePartSize + 1
	first := uploadPartReceiptFixture(1, receiptFixturePartSize, "First")
	last := uploadPartReceiptFixture(5, 1, "Tail")
	receipts := []domain.UploadPartReceipt{
		first,
		uploadPartReceiptFixture(2, receiptFixturePartSize-1, "Short"),
		uploadPartReceiptFixture(4, receiptFixturePartSize+1, "Long"),
		last,
	}
	before := slices.Clone(receipts)

	// 1. 保留完整片和短尾片，缺片与长度不符的片留给客户端补传。
	available, err := FilterUploadPartReceipts(fileSize, receiptFixturePartSize, 5, receipts)
	want := []domain.UploadPartReceipt{first, last}
	if err != nil || !slices.Equal(available, want) || !slices.Equal(receipts, before) {
		t.Fatal("筛选结果、原回执值或输入保护不符", err)
	}

	// 2. 返回值持有独立的清单，输入切片复用后也保留当前结果。
	clear(receipts)
	if !slices.Equal(available, want) {
		t.Fatal("输入切片变化影响了已确认的结果")
	}
}

func TestFilterUploadPartReceiptsLengthBoundaries(t *testing.T) {
	const maximumCount = domain.MaxUploadPartCount
	for _, test := range []struct {
		name     string
		fileSize uint64
		count    uint32
		receipts []domain.UploadPartReceipt
		want     []domain.UploadPartReceipt
	}{
		{name: "no receipts", fileSize: receiptFixturePartSize + 1, count: 2},
		{name: "all lengths mismatch", fileSize: receiptFixturePartSize + 1, count: 2,
			receipts: []domain.UploadPartReceipt{
				uploadPartReceiptFixture(1, 0, "Empty"),
				uploadPartReceiptFixture(2, receiptFixturePartSize, "LongTail"),
			}},
		{name: "exactly divided file keeps a full last part", fileSize: 2 * receiptFixturePartSize, count: 2,
			receipts: []domain.UploadPartReceipt{uploadPartReceiptFixture(2, receiptFixturePartSize, "FullTail")},
			want:     []domain.UploadPartReceipt{uploadPartReceiptFixture(2, receiptFixturePartSize, "FullTail")}},
		{name: "highest supported part number", fileSize: (maximumCount-1)*receiptFixturePartSize + 1, count: maximumCount,
			receipts: []domain.UploadPartReceipt{uploadPartReceiptFixture(maximumCount, 1, "Last")},
			want:     []domain.UploadPartReceipt{uploadPartReceiptFixture(maximumCount, 1, "Last")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := FilterUploadPartReceipts(test.fileSize, receiptFixturePartSize, test.count, test.receipts)
			if err != nil || result == nil || !slices.Equal(result, test.want) {
				t.Fatal("边界片长或空列表处理不符", err)
			}
		})
	}
}

func TestFilterUploadPartReceiptsRejectsWholeBatch(t *testing.T) {
	const fileSize = 2*receiptFixturePartSize + 1
	first := uploadPartReceiptFixture(1, receiptFixturePartSize, "First")
	shortSecond := uploadPartReceiptFixture(2, 1, "ShortSecond")
	second := uploadPartReceiptFixture(2, receiptFixturePartSize, "Second")
	for _, test := range []struct {
		name     string
		receipts []domain.UploadPartReceipt
	}{
		{"zero number after valid receipt", []domain.UploadPartReceipt{first, uploadPartReceiptFixture(0, 1, "Zero")}},
		{"number past plan even when its length is wrong", []domain.UploadPartReceipt{first, uploadPartReceiptFixture(4, 0, "Outside")}},
		{"duplicate of a skipped receipt", []domain.UploadPartReceipt{first, shortSecond, second}},
		{"out of order after a skipped receipt", []domain.UploadPartReceipt{shortSecond, first}},
		{"too many receipts", []domain.UploadPartReceipt{first, first, second, second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := FilterUploadPartReceipts(fileSize, receiptFixturePartSize, 3, test.receipts)
			if result != nil || err != domain.ErrUploadStorageUnavailable {
				t.Fatal("非法编号未使整批失败", err)
			}
		})
	}
}

func TestFilterUploadPartReceiptsRejectsInvalidPlanEvenWhenEmpty(t *testing.T) {
	for _, test := range []struct {
		name               string
		fileSize, partSize uint64
		count              uint32
	}{
		{"single upload", receiptFixturePartSize, receiptFixturePartSize, 1},
		{"missing part size", receiptFixturePartSize + 1, 0, 2},
		{"wrong part count", receiptFixturePartSize + 1, receiptFixturePartSize, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := FilterUploadPartReceipts(test.fileSize, test.partSize, test.count, nil)
			if result != nil || err != domain.ErrInvalidUploadTaskData {
				t.Fatal("无回执时跳过了任务分片计划检查", err)
			}
		})
	}
}
