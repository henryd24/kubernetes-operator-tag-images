package ecr

import "testing"

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
