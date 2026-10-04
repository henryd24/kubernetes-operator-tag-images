package ecr

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

func TestParseImageRefWithTag(t *testing.T) {
	image := "123456789012.dkr.ecr.us-east-1.amazonaws.com/my-service/backend:v1.2.3"
	ref, err := ParseImageRef(image)
	if err != nil {
		t.Fatalf("ParseImageRef returned error: %v", err)
	}

	if ref.Registry != "123456789012.dkr.ecr.us-east-1.amazonaws.com" {
		t.Fatalf("unexpected registry: %s", ref.Registry)
	}
	if ref.Repository != "my-service/backend" {
		t.Fatalf("unexpected repository: %s", ref.Repository)
	}
	if ref.Tag != "v1.2.3" {
		t.Fatalf("unexpected tag: %s", ref.Tag)
	}
	if ref.Region != "us-east-1" {
		t.Fatalf("unexpected region: %s", ref.Region)
	}
}

func TestParseImageRefWithDigest(t *testing.T) {
	image := "123456789012.dkr.ecr.us-west-2.amazonaws.com/my-service/backend@sha256:deadbeef"
	ref, err := ParseImageRef(image)
	if err != nil {
		t.Fatalf("ParseImageRef returned error: %v", err)
	}

	if ref.Digest != "sha256:deadbeef" {
		t.Fatalf("unexpected digest: %s", ref.Digest)
	}
	if ref.Region != "us-west-2" {
		t.Fatalf("unexpected region: %s", ref.Region)
	}
}

func TestParseImageRefFailsWithoutTagOrDigest(t *testing.T) {
	_, err := ParseImageRef("123456789012.dkr.ecr.us-east-1.amazonaws.com/my-service/backend")
	if err == nil {
		t.Fatalf("expected error for image without tag or digest")
	}
}

func TestParseImageRefWithTagAndDigest(t *testing.T) {
	ref, err := ParseImageRef("123456789012.dkr.ecr.us-east-1.amazonaws.com/team/app:v1.2.3@sha256:deadbeef")
	if err != nil {
		t.Fatalf("ParseImageRef returned error: %v", err)
	}
	if ref.Repository != "team/app" || ref.Tag != "v1.2.3" || ref.Digest != "sha256:deadbeef" {
		t.Fatalf("unexpected ref: %+v", ref)
	}
}

func TestParseImageRefRegistryVariants(t *testing.T) {
	tests := []struct {
		image   string
		account string
		region  string
	}{
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1", "123456789012", "us-east-1"},
		{"123456789012.dkr.ecr-fips.us-gov-west-1.amazonaws.com/app:v1", "123456789012", "us-gov-west-1"},
		{"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn/app:v1", "123456789012", "cn-north-1"},
		{"123456789012.dkr-ecr.eu-west-1.on.aws/app:v1", "123456789012", "eu-west-1"},
		{"docker.io/library/nginx:1.27", "", ""},
		{"ghcr.io/org/app:v1", "", ""},
	}
	for _, tt := range tests {
		ref, err := ParseImageRef(tt.image)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tt.image, err)
		}
		if ref.AccountId != tt.account || ref.Region != tt.region {
			t.Fatalf("%s: got account=%q region=%q", tt.image, ref.AccountId, ref.Region)
		}
		if ref.IsECR() != (tt.account != "") {
			t.Fatalf("%s: unexpected IsECR=%v", tt.image, ref.IsECR())
		}
	}
}

type fakeECRClient struct {
	getOutput *ecr.BatchGetImageOutput
	getInputs []*ecr.BatchGetImageInput
	putInputs []*ecr.PutImageInput
	putErr    error
}

func (f *fakeECRClient) BatchGetImage(_ context.Context, params *ecr.BatchGetImageInput, _ ...func(*ecr.Options)) (*ecr.BatchGetImageOutput, error) {
	f.getInputs = append(f.getInputs, params)
	return f.getOutput, nil
}

func (f *fakeECRClient) PutImage(_ context.Context, params *ecr.PutImageInput, _ ...func(*ecr.Options)) (*ecr.PutImageOutput, error) {
	f.putInputs = append(f.putInputs, params)
	return &ecr.PutImageOutput{}, f.putErr
}

func TestRetagCopiesManifestAndMediaType(t *testing.T) {
	indexType := "application/vnd.oci.image.index.v1+json"
	fake := &fakeECRClient{getOutput: &ecr.BatchGetImageOutput{Images: []types.Image{{
		ImageManifest:          aws.String(`{"manifests":[]}`),
		ImageManifestMediaType: aws.String(indexType),
	}}}}
	source, _ := ParseImageRef("123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1")

	if err := New(fake).Retag(context.Background(), source, "123456789012", "app", []string{"dev-v1", "active-dev"}); err != nil {
		t.Fatalf("Retag returned error: %v", err)
	}

	if !slices.Contains(fake.getInputs[0].AcceptedMediaTypes, indexType) {
		t.Fatalf("expected multi-arch index media type to be accepted, got %v", fake.getInputs[0].AcceptedMediaTypes)
	}
	if len(fake.putInputs) != 2 {
		t.Fatalf("expected 2 PutImage calls, got %d", len(fake.putInputs))
	}
	for _, in := range fake.putInputs {
		if aws.ToString(in.ImageManifestMediaType) != indexType {
			t.Fatalf("expected media type %s, got %s", indexType, aws.ToString(in.ImageManifestMediaType))
		}
	}
}

func TestRetagReportsBatchGetFailures(t *testing.T) {
	fake := &fakeECRClient{getOutput: &ecr.BatchGetImageOutput{Failures: []types.ImageFailure{{
		FailureCode:   types.ImageFailureCodeImageNotFound,
		FailureReason: aws.String("Requested image not found"),
	}}}}
	source, _ := ParseImageRef("123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1")

	err := New(fake).Retag(context.Background(), source, "123456789012", "app", []string{"active-dev"})
	if err == nil || !strings.Contains(err.Error(), "ImageNotFound") {
		t.Fatalf("expected error mentioning ImageNotFound, got %v", err)
	}
}
