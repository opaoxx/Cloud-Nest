package domain

import (
	"errors"
	"math"
	"testing"
)

const testUploadMiB = 1024 * 1024

func TestPlanUpload(t *testing.T) {
	limits := UploadPlanLimits{
		MaxFileSizeBytes:   DefaultMaxUploadSizeBytes,
		MaxObjectSizeBytes: 64 * 1024 * testUploadMiB,
		MaxPartSizeBytes:   1024 * testUploadMiB,
	}
	for _, testCase := range []struct {
		name      string
		sizeBytes uint64
		want      UploadPlan
	}{
		{name: "empty file", want: UploadPlan{Mode: "SINGLE"}},
		{name: "last single byte count", sizeBytes: 16 * testUploadMiB, want: UploadPlan{Mode: "SINGLE"}},
		{
			name: "first multipart byte count", sizeBytes: 16*testUploadMiB + 1,
			want: UploadPlan{Mode: "MULTIPART", PartSizeBytes: 16 * testUploadMiB, PartCount: 2, LastPartSizeBytes: 1},
		},
		{
			name: "full last part", sizeBytes: 32 * testUploadMiB,
			want: UploadPlan{Mode: "MULTIPART", PartSizeBytes: 16 * testUploadMiB, PartCount: 2, LastPartSizeBytes: 16 * testUploadMiB},
		},
		{
			name: "short last part", sizeBytes: 40*testUploadMiB + 1,
			want: UploadPlan{Mode: "MULTIPART", PartSizeBytes: 16 * testUploadMiB, PartCount: 3, LastPartSizeBytes: 8*testUploadMiB + 1},
		},
		{
			name: "default maximum", sizeBytes: 50 * 1024 * testUploadMiB,
			want: UploadPlan{Mode: "MULTIPART", PartSizeBytes: 16 * testUploadMiB, PartCount: 3200, LastPartSizeBytes: 16 * testUploadMiB},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan, err := PlanUpload(testCase.sizeBytes, limits)
			if err != nil || plan != testCase.want {
				t.Fatalf("got %+v, %v, want %+v", plan, err, testCase.want)
			}
		})
	}
}

func TestPlanUploadAdaptsPartSize(t *testing.T) {
	limits := UploadPlanLimits{
		MaxFileSizeBytes:   17 * testUploadMiB * 10_000,
		MaxObjectSizeBytes: 17 * testUploadMiB * 10_000,
		MaxPartSizeBytes:   17 * testUploadMiB,
	}
	for _, testCase := range []struct {
		name      string
		sizeBytes uint64
		want      UploadPlan
	}{
		{
			name: "10000 initial parts", sizeBytes: 16 * testUploadMiB * 10_000,
			want: UploadPlan{Mode: "MULTIPART", PartSizeBytes: 16 * testUploadMiB, PartCount: 10_000, LastPartSizeBytes: 16 * testUploadMiB},
		},
		{
			name: "one more byte rounds part size upward", sizeBytes: 16*testUploadMiB*10_000 + 1,
			want: UploadPlan{Mode: "MULTIPART", PartSizeBytes: 17 * testUploadMiB, PartCount: 9412, LastPartSizeBytes: 13*testUploadMiB + 1},
		},
		{
			name: "10000 adapted parts", sizeBytes: 17 * testUploadMiB * 10_000,
			want: UploadPlan{Mode: "MULTIPART", PartSizeBytes: 17 * testUploadMiB, PartCount: 10_000, LastPartSizeBytes: 17 * testUploadMiB},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan, err := PlanUpload(testCase.sizeBytes, limits)
			if err != nil || plan != testCase.want {
				t.Fatalf("got %+v, %v, want %+v", plan, err, testCase.want)
			}
		})
	}
}

func TestPlanUploadReportsEffectiveLimit(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		limits UploadPlanLimits
		max    uint64
	}{
		{
			name:   "configured maximum",
			limits: UploadPlanLimits{DefaultMaxUploadSizeBytes, 64 * 1024 * testUploadMiB, 1024 * testUploadMiB},
			max:    50 * 1024 * testUploadMiB,
		},
		{
			name:   "storage object maximum",
			limits: UploadPlanLimits{DefaultMaxUploadSizeBytes, 28*testUploadMiB + 13, 1024 * testUploadMiB},
			max:    28*testUploadMiB + 13,
		},
		{
			name:   "capacity of aligned parts",
			limits: UploadPlanLimits{math.MaxUint64, math.MaxUint64, 17*testUploadMiB - 1},
			max:    16 * testUploadMiB * 10_000,
		},
		{
			name:   "storage cannot fit initial multipart part",
			limits: UploadPlanLimits{DefaultMaxUploadSizeBytes, math.MaxUint64, 16*testUploadMiB - 1},
			max:    16 * testUploadMiB,
		},
		{
			name:   "object maximum inside single range",
			limits: UploadPlanLimits{DefaultMaxUploadSizeBytes, 99, 16 * testUploadMiB},
			max:    99,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := PlanUpload(testCase.max, testCase.limits); err != nil {
				t.Fatalf("limit byte count rejected: %v", err)
			}
			for _, sizeBytes := range []uint64{testCase.max + 1, math.MaxUint64} {
				plan, err := PlanUpload(sizeBytes, testCase.limits)
				var limitError *UploadSizeLimitError
				if plan != (UploadPlan{}) || !errors.As(err, &limitError) || limitError.MaxSizeBytes != testCase.max {
					t.Fatalf("got %+v, %v, want empty plan and limit %d", plan, err, testCase.max)
				}
			}
		})
	}
}

func TestPlanUploadRejectsMissingLimits(t *testing.T) {
	for _, limits := range []UploadPlanLimits{
		{MaxObjectSizeBytes: 1, MaxPartSizeBytes: 1},
		{MaxFileSizeBytes: 1, MaxPartSizeBytes: 1},
		{MaxFileSizeBytes: 1, MaxObjectSizeBytes: 1},
	} {
		if plan, err := PlanUpload(0, limits); plan != (UploadPlan{}) || !errors.Is(err, ErrInvalidUploadPlanLimits) {
			t.Fatalf("got %+v, %v, want empty plan and invalid limits error", plan, err)
		}
	}
}

func TestPlanUploadWithUint64Limits(t *testing.T) {
	// 上界夹具验证整数运算，不代表存储供应商支持的文件大小。
	limits := UploadPlanLimits{math.MaxUint64, math.MaxUint64, math.MaxUint64}
	plan, err := PlanUpload(math.MaxUint64, limits)
	want := UploadPlan{
		Mode: "MULTIPART", PartSizeBytes: 1_844_674_407_956_480,
		PartCount: 10_000, LastPartSizeBytes: 1_844_668_552_708_095,
	}
	if err != nil || plan != want {
		t.Fatalf("got %+v, %v, want %+v", plan, err, want)
	}
}
