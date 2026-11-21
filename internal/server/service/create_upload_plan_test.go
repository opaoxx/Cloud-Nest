package service_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestValidateCreateUploadInputWithPlan(t *testing.T) {
	const singleLimit = 16 * 1024 * 1024
	limits := domain.UploadPlanLimits{
		MaxFileSizeBytes:   domain.DefaultMaxUploadSizeBytes,
		MaxObjectSizeBytes: 30 * 1024 * 1024,
		MaxPartSizeBytes:   singleLimit,
	}
	digest := sha256.Sum256([]byte("fixture"))
	emptyDigest := [sha256.Size]byte{}
	multipartPlan := domain.UploadPlan{Mode: "MULTIPART", PartSizeBytes: singleLimit, PartCount: 2, LastPartSizeBytes: 1}
	for _, testCase := range []struct {
		name   string
		size   uint64
		digest *[sha256.Size]byte
		plan   domain.UploadPlan
	}{
		{name: "empty single", plan: domain.UploadPlan{Mode: "SINGLE"}},
		{name: "single limit without digest", size: singleLimit, plan: domain.UploadPlan{Mode: "SINGLE"}},
		{name: "single with digest", size: singleLimit, digest: &digest, plan: domain.UploadPlan{Mode: "SINGLE"}},
		{name: "first multipart size", size: singleLimit + 1, digest: &digest, plan: multipartPlan},
		{name: "explicit zero digest is still present", size: singleLimit + 1, digest: &emptyDigest, plan: multipartPlan},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input := domain.CreateUploadInput{ParentID: 12, Name: "  工程文件.bin  ", SizeBytes: testCase.size, ExpectedSHA256: testCase.digest}
			original := input
			prepared, plan, err := service.ValidateCreateUploadInputWithPlan(input, limits)
			if err != nil || prepared == nil || plan != testCase.plan || input != original ||
				prepared.ParentID != input.ParentID || prepared.Name != "工程文件.bin" || prepared.SizeBytes != input.SizeBytes {
				t.Fatalf("unexpected prepared declaration and plan: %+v, %+v, %v", prepared, plan, err)
			}
			if testCase.digest == nil {
				if prepared.ExpectedSHA256 != nil {
					t.Fatal("optional digest was added")
				}
				return
			}
			if prepared.ExpectedSHA256 == nil || prepared.ExpectedSHA256 == input.ExpectedSHA256 ||
				*prepared.ExpectedSHA256 != *testCase.digest {
				t.Fatal("digest was not independently copied")
			}
		})
	}
}

func TestValidateCreateUploadInputWithPlanErrorOrder(t *testing.T) {
	const storageLimit = 20*1024*1024 + 1
	limits := domain.UploadPlanLimits{
		MaxFileSizeBytes: domain.DefaultMaxUploadSizeBytes, MaxObjectSizeBytes: storageLimit, MaxPartSizeBytes: 16 * 1024 * 1024,
	}
	assertError := func(input domain.CreateUploadInput, limits domain.UploadPlanLimits, want error) error {
		t.Helper()
		prepared, plan, err := service.ValidateCreateUploadInputWithPlan(input, limits)
		if prepared != nil || plan != (domain.UploadPlan{}) || err == nil || (want != nil && !errors.Is(err, want)) {
			t.Fatalf("got %+v, %+v, %v, want no result and error %v", prepared, plan, err, want)
		}
		return err
	}

	input := domain.CreateUploadInput{Name: "invalid/path.bin", SizeBytes: storageLimit + 1}
	assertError(input, limits, domain.ErrInvalidUploadParentID)
	input.ParentID = 12
	assertError(input, limits, domain.ErrInvalidEntryName)
	input.Name = "file.bin"
	assertError(input, domain.UploadPlanLimits{}, domain.ErrInvalidUploadPlanLimits)

	err := assertError(input, limits, nil)
	var limitError *domain.UploadSizeLimitError
	if !errors.As(err, &limitError) || limitError.MaxSizeBytes != storageLimit {
		t.Fatalf("got %v, want limit %d", err, storageLimit)
	}
	input.SizeBytes = storageLimit
	assertError(input, limits, domain.ErrMultipartUploadSHA256Required)
}

func TestValidateCreateUploadInputWithPlanDTOFlow(t *testing.T) {
	limits := domain.UploadPlanLimits{
		MaxFileSizeBytes: domain.DefaultMaxUploadSizeBytes, MaxObjectSizeBytes: domain.DefaultMaxUploadSizeBytes, MaxPartSizeBytes: 16 * 1024 * 1024,
	}
	for _, testCase := range []struct {
		name, digestField string
		wantFieldError    bool
		wantMissingDigest bool
	}{
		{name: "omitted", wantMissingDigest: true},
		{name: "null", digestField: `,"sha256":null`, wantMissingDigest: true},
		{name: "invalid hex", digestField: `,"sha256":"` + strings.Repeat("x", 64) + `"`, wantFieldError: true},
		{name: "mixed case hex", digestField: `,"sha256":"` + strings.Repeat("aB", 32) + `"`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := `{"parent_id":"12","name":" File.bin ","size_bytes":16777217` + testCase.digestField + `}`
			var request dto.CreateUploadRequest
			if err := json.Unmarshal([]byte(body), &request); err != nil {
				t.Fatal(err)
			}
			input, fieldError := dto.ParseCreateUploadRequest(request)
			if testCase.wantFieldError {
				if input != nil || fieldError == nil || fieldError.Field != "sha256" {
					t.Fatalf("invalid digest was not rejected by DTO: %+v, %v", input, fieldError)
				}
				return
			}
			if fieldError != nil || input == nil {
				t.Fatalf("valid request shape did not parse: %v", fieldError)
			}
			prepared, plan, err := service.ValidateCreateUploadInputWithPlan(*input, limits)
			if testCase.wantMissingDigest {
				if prepared != nil || plan != (domain.UploadPlan{}) || !errors.Is(err, domain.ErrMultipartUploadSHA256Required) {
					t.Fatalf("missing digest was not rejected: %+v, %+v, %v", prepared, plan, err)
				}
				return
			}
			if err != nil || prepared == nil || prepared.Name != "File.bin" || plan.Mode != "MULTIPART" ||
				prepared.ExpectedSHA256 == nil || prepared.ExpectedSHA256[0] != 0xab {
				t.Fatalf("valid digest or declaration was not preserved: %+v, %+v, %v", prepared, plan, err)
			}
		})
	}
}
