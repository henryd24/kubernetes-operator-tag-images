package ecr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// ecrRegistryRegex matches ECR private registry hostnames, including FIPS
// (dkr.ecr-fips), China (amazonaws.com.cn) and dual-stack (dkr-ecr.<region>.on.aws) endpoints.
var ecrRegistryRegex = regexp.MustCompile(`^([0-9]{12})\.(?:dkr\.ecr(?:-fips)?\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?|dkr-ecr\.([a-z0-9-]+)\.on\.aws)$`)

// acceptedManifestMediaTypes lists the manifest types the tagger can copy. Manifest
// lists / OCI indexes are included so multi-arch images can be retagged.
var acceptedManifestMediaTypes = []string{
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
}

// ImageRef contains the parsed components of an image URI.
type ImageRef struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
	Region     string
	AccountId  string
}

// IsECR reports whether the image registry is an ECR private registry.
func (r ImageRef) IsECR() bool {
	return r.AccountId != ""
}

// RetagResult describes the outcome of a Retag call.
type RetagResult struct {
	// SourceDigest is the digest of the source image manifest.
	SourceDigest string
	// Applied lists the tags that now point to the source image.
	Applied []string
	// Immutable lists the tags that already exist in a repository with tag
	// immutability enabled. They cannot be updated, so retrying is pointless.
	Immutable []string
}

type Tagger interface {
	Retag(ctx context.Context, source ImageRef, destinationAccountId string, destinationRepository string, tags []string) (RetagResult, error)
	// TagDigest reports whether the tag exists and, when known, the digest it points to.
	TagDigest(ctx context.Context, repository string, accountId string, tag string) (digest string, exists bool, err error)
}

type client interface {
	BatchGetImage(ctx context.Context, params *ecr.BatchGetImageInput, optFns ...func(*ecr.Options)) (*ecr.BatchGetImageOutput, error)
	PutImage(ctx context.Context, params *ecr.PutImageInput, optFns ...func(*ecr.Options)) (*ecr.PutImageOutput, error)
}

type awsTagger struct {
	client client
}

func New(client client) Tagger {
	return &awsTagger{client: client}
}

func (a *awsTagger) Retag(ctx context.Context, source ImageRef, destinationAccountId string, destinationRepository string, tags []string) (RetagResult, error) {
	result := RetagResult{}
	if len(tags) == 0 {
		return result, errors.New("no destination tags were provided")
	}

	imageID := types.ImageIdentifier{}
	if source.Digest != "" {
		imageID.ImageDigest = &source.Digest
	} else if source.Tag != "" {
		imageID.ImageTag = &source.Tag
	} else {
		return result, errors.New("source image does not have tag or digest")
	}

	res, err := a.client.BatchGetImage(ctx, &ecr.BatchGetImageInput{
		RepositoryName:     &source.Repository,
		ImageIds:           []types.ImageIdentifier{imageID},
		AcceptedMediaTypes: acceptedManifestMediaTypes,
		RegistryId:         &source.AccountId,
	})
	if err != nil {
		return result, fmt.Errorf("batch get image from ecr: %w", err)
	}
	if len(res.Images) == 0 || res.Images[0].ImageManifest == nil {
		return result, fmt.Errorf("source image not found in repository %s%s", source.Repository, describeFailures(res.Failures))
	}

	manifest := res.Images[0].ImageManifest
	mediaType := res.Images[0].ImageManifestMediaType
	if res.Images[0].ImageId != nil {
		result.SourceDigest = aws.ToString(res.Images[0].ImageId.ImageDigest)
	}
	for _, tag := range tags {
		tag := strings.TrimSpace(tag)
		if tag == "" {
			continue
		}

		if _, err := a.client.PutImage(ctx, &ecr.PutImageInput{
			RepositoryName:         &destinationRepository,
			ImageManifest:          manifest,
			ImageManifestMediaType: mediaType,
			ImageTag:               &tag,
			RegistryId:             &destinationAccountId,
		}); err != nil {
			switch {
			case isAPIError(err, "ImageAlreadyExistsException"):
				// The tag already points to this exact manifest.
			case isAPIError(err, "ImageTagAlreadyExistsException"):
				// The repository is immutable and the tag points to another image.
				result.Immutable = append(result.Immutable, tag)
				continue
			default:
				return result, fmt.Errorf("put image with tag %s: %w", tag, err)
			}
		}
		result.Applied = append(result.Applied, tag)
	}

	return result, nil
}

func (a *awsTagger) TagDigest(ctx context.Context, repository string, accountId string, tag string) (string, bool, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "", false, errors.New("tag is empty")
	}

	res, err := a.client.BatchGetImage(ctx, &ecr.BatchGetImageInput{
		RepositoryName:     &repository,
		ImageIds:           []types.ImageIdentifier{{ImageTag: &tag}},
		AcceptedMediaTypes: acceptedManifestMediaTypes,
		RegistryId:         &accountId,
	})
	if err != nil {
		return "", false, fmt.Errorf("check existing tag %s: %w", tag, err)
	}
	if len(res.Images) == 0 {
		return "", false, nil
	}
	var digest string
	if res.Images[0].ImageId != nil {
		digest = aws.ToString(res.Images[0].ImageId.ImageDigest)
	}
	return digest, true, nil
}

func describeFailures(failures []types.ImageFailure) string {
	if len(failures) == 0 {
		return ""
	}
	reasons := make([]string, 0, len(failures))
	for _, f := range failures {
		reasons = append(reasons, fmt.Sprintf("%s: %s", f.FailureCode, aws.ToString(f.FailureReason)))
	}
	return " (" + strings.Join(reasons, "; ") + ")"
}

func isAPIError(err error, code string) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}

type Factory struct {
	mu      sync.Mutex
	clients map[string]Tagger
}

func NewFactory() *Factory {
	return &Factory{clients: map[string]Tagger{}}
}

func (f *Factory) ForRegion(ctx context.Context, region string) (Tagger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	targetRoleARN := os.Getenv("TARGET_ROLE_ARN")
	cacheKey := region + "-" + targetRoleARN
	if existing, ok := f.clients[cacheKey]; ok {
		return existing, nil
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config for region %s: %w", region, err)
	}

	if targetRoleARN != "" {
		stsClient := sts.NewFromConfig(cfg)
		provider := stscreds.NewAssumeRoleProvider(stsClient, targetRoleARN)
		cfg.Credentials = aws.NewCredentialsCache(provider)
	}

	tagger := New(ecr.NewFromConfig(cfg))
	f.clients[cacheKey] = tagger
	return tagger, nil
}

func ParseImageRef(image string) (ImageRef, error) {
	parts := strings.SplitN(image, "/", 2)
	if len(parts) != 2 {
		return ImageRef{}, fmt.Errorf("image %q is invalid", image)
	}

	registry := parts[0]
	repoAndRef := parts[1]

	if strings.Contains(repoAndRef, "@") {
		repoDigest := strings.SplitN(repoAndRef, "@", 2)
		if len(repoDigest) != 2 {
			return ImageRef{}, fmt.Errorf("image %q has invalid digest format", image)
		}
		repository, tag := repoDigest[0], ""
		// Support "repo:tag@sha256:..." references: the digest wins, the tag is kept as metadata.
		if index := strings.LastIndex(repository, ":"); index >= 0 {
			repository, tag = repository[:index], repository[index+1:]
		}
		if repository == "" || repoDigest[1] == "" {
			return ImageRef{}, fmt.Errorf("image %q has empty repository or digest", image)
		}
		accountId, region := regionFromRegistry(registry)
		return ImageRef{Registry: registry, Repository: repository, Tag: tag, Digest: repoDigest[1], AccountId: accountId, Region: region}, nil
	}

	index := strings.LastIndex(repoAndRef, ":")
	if index < 0 {
		return ImageRef{}, fmt.Errorf("image %q must include a tag", image)
	}

	repository := repoAndRef[:index]
	tag := repoAndRef[index+1:]
	if repository == "" || tag == "" {
		return ImageRef{}, fmt.Errorf("image %q has empty repository or tag", image)
	}

	accountId, region := regionFromRegistry(registry)
	return ImageRef{Registry: registry, Repository: repository, Tag: tag, AccountId: accountId, Region: region}, nil
}

func regionFromRegistry(registry string) (string, string) {
	matches := ecrRegistryRegex.FindStringSubmatch(registry)
	if matches == nil {
		return "", ""
	}
	if matches[2] != "" {
		return matches[1], matches[2]
	}
	return matches[1], matches[3]
}
