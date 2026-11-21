package service

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const signingFixturePartSize = 16 * 1024 * 1024
const signingFixtureHex = "4e07408562bedb8b60ce05c1decfe3ad16b72230967de01f640b7e4729b49fce"
const signingFixtureBase64 = "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="

func TestPrepareUploadPartSigningInputsPreservesOrder(t *testing.T) {
	parts := []UploadPartDeclaration{
		{PartNumber: 3, SHA256: strings.ToUpper(signingFixtureHex)},
		{PartNumber: 1, SHA256: strings.Repeat("ff", 32)},
		{PartNumber: 2, SHA256: signingFixtureHex},
	}
	result, err := PrepareUploadPartSigningInputs(40*1024*1024+1, signingFixturePartSize, 3, parts)
	want := []UploadPartSigningInput{
		{PartNumber: 3, SizeBytes: 8*1024*1024 + 1, ChecksumSHA256: signingFixtureBase64},
		{PartNumber: 1, SizeBytes: signingFixturePartSize, ChecksumSHA256: strings.Repeat("/", 42) + "8="},
		{PartNumber: 2, SizeBytes: signingFixturePartSize, ChecksumSHA256: signingFixtureBase64},
	}
	if err != nil || !reflect.DeepEqual(result, want) {
		t.Fatalf("got %+v, %v, want %+v", result, err, want)
	}
}

func TestPrepareUploadPartSigningInputsAcceptsBatchAtMaximum(t *testing.T) {
	parts := make([]UploadPartDeclaration, 20)
	for index := range parts {
		parts[index] = UploadPartDeclaration{PartNumber: 10_000 - uint32(index), SHA256: signingFixtureHex}
	}
	result, err := PrepareUploadPartSigningInputs(9999*signingFixturePartSize+1, signingFixturePartSize, 10_000, parts)
	if err != nil || len(result) != len(parts) {
		t.Fatalf("20 valid parts produced %d signing inputs: %v", len(result), err)
	}
	for index, input := range result {
		wantSize := uint64(signingFixturePartSize)
		if index == 0 {
			wantSize = 1
		}
		if input.PartNumber != parts[index].PartNumber || input.SizeBytes != wantSize || input.ChecksumSHA256 != signingFixtureBase64 {
			t.Fatalf("unexpected signing input at index %d: %+v", index, input)
		}
	}
}

func TestPrepareUploadPartSigningInputsRejectsWholeBatch(t *testing.T) {
	first := UploadPartDeclaration{PartNumber: 1, SHA256: signingFixtureHex}
	second := UploadPartDeclaration{PartNumber: 2, SHA256: signingFixtureHex}
	for _, testCase := range []struct {
		name, field string
		parts       []UploadPartDeclaration
	}{
		{name: "missing array", field: "parts"},
		{name: "empty array", field: "parts", parts: []UploadPartDeclaration{}},
		{name: "over 20 entries", field: "parts", parts: make([]UploadPartDeclaration, 21)},
		{name: "duplicate number", field: "parts", parts: []UploadPartDeclaration{first, second, first}},
		{name: "zero number", field: "parts.1.part_number", parts: []UploadPartDeclaration{first, {SHA256: signingFixtureHex}}},
		{name: "above saved count", field: "parts.1.part_number", parts: []UploadPartDeclaration{first, {PartNumber: 3, SHA256: signingFixtureHex}}},
		{name: "above protocol limit", field: "parts.1.part_number", parts: []UploadPartDeclaration{first, {PartNumber: 10001, SHA256: signingFixtureHex}}},
		{name: "missing digest", field: "parts.1.sha256", parts: []UploadPartDeclaration{first, {PartNumber: 2}}},
		{name: "short digest", field: "parts.1.sha256", parts: []UploadPartDeclaration{first, {PartNumber: 2, SHA256: signingFixtureHex[:63]}}},
		{name: "long digest", field: "parts.1.sha256", parts: []UploadPartDeclaration{first, {PartNumber: 2, SHA256: signingFixtureHex + "0"}}},
		{name: "non-hex digest", field: "parts.1.sha256", parts: []UploadPartDeclaration{first, {PartNumber: 2, SHA256: signingFixtureHex[:63] + "g"}}},
		{name: "whitespace in digest", field: "parts.1.sha256", parts: []UploadPartDeclaration{first, {PartNumber: 2, SHA256: signingFixtureHex[:63] + "\n"}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := PrepareUploadPartSigningInputs(signingFixturePartSize+1, signingFixturePartSize, 2, testCase.parts)
			var inputError *UploadPartDeclarationError
			if result != nil || !errors.As(err, &inputError) || inputError.Field != testCase.field {
				t.Fatalf("got %+v, %#v, want no result and error for %q", result, err, testCase.field)
			}
		})
	}
}

func TestPrepareUploadPartSigningInputsRejectsInvalidSavedPlan(t *testing.T) {
	parts := []UploadPartDeclaration{{PartNumber: 1, SHA256: signingFixtureHex}}
	result, err := PrepareUploadPartSigningInputs(signingFixturePartSize+1, signingFixturePartSize, 4, parts)
	var inputError *UploadPartDeclarationError
	if result != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) || errors.As(err, &inputError) {
		t.Fatalf("invalid persisted plan classified as a request field error: %+v, %v", result, err)
	}
}
