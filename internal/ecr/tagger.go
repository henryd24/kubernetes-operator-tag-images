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

var ecrRegistryRegex = regexp.MustCompile(`^([0-9]{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com$`)

// ImageRef contains the parsed components of an image URI.
type ImageRef struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
	Region     string
}

type Tagger interface {
	Retag(ctx context.Context, source ImageRef, destinationRepository string, tags []string) error
	TagExists(ctx context.Context, repository string, tag string) (bool, error)
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

func (a *awsTagger) Retag(ctx context.Context, source ImageRef, destinationRepository string, tags []string) error {
	if len(tags) == 0 {
		return errors.New("no destination tags were provided")
	}

	imageID := types.ImageIdentifier{}
	if source.Digest != "" {
		imageID.ImageDigest = &source.Digest
	} else if source.Tag != "" {
		imageID.ImageTag = &source.Tag
	} else {
		return errors.New("source image does not have tag or digest")
	}

	res, err := a.client.BatchGetImage(ctx, &ecr.BatchGetImageInput{
		RepositoryName: &source.Repository,
		ImageIds:       []types.ImageIdentifier{imageID},
		AcceptedMediaTypes: []string{
			"application/vnd.oci.image.manifest.v1+json",
			"application/vnd.docker.distribution.manifest.v2+json",
		},
	})
	if err != nil {
		return fmt.Errorf("batch get image from ecr: %w", err)
	}
	if len(res.Images) == 0 || res.Images[0].ImageManifest == nil {
		return fmt.Errorf("source image not found in repository %s", source.Repository)
	}

	manifest := res.Images[0].ImageManifest
	for _, tag := range tags {
		tag := strings.TrimSpace(tag)
		if tag == "" {
			continue
		}

		if _, err := a.client.PutImage(ctx, &ecr.PutImageInput{
			RepositoryName: &destinationRepository,
			ImageManifest:  manifest,
			ImageTag:       &tag,
		}); err != nil {
			if isImageAlreadyExists(err) {
				continue
			}
			return fmt.Errorf("put image with tag %s: %w", tag, err)
		}
	}

	return nil
}

func (a *awsTagger) TagExists(ctx context.Context, repository string, tag string) (bool, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return false, errors.New("tag is empty")
	}

	res, err := a.client.BatchGetImage(ctx, &ecr.BatchGetImageInput{
		RepositoryName: &repository,
		ImageIds:       []types.ImageIdentifier{{ImageTag: &tag}},
	})
	if err != nil {
		return false, fmt.Errorf("check existing tag %s: %w", tag, err)
	}

	return len(res.Images) > 0, nil
}

func isImageAlreadyExists(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "ImageAlreadyExistsException"
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
		region, _ := regionFromRegistry(registry)
		return ImageRef{Registry: registry, Repository: repoDigest[0], Digest: repoDigest[1], Region: region}, nil
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

	region, _ := regionFromRegistry(registry)
	return ImageRef{Registry: registry, Repository: repository, Tag: tag, Region: region}, nil
}

func regionFromRegistry(registry string) (string, bool) {
	matches := ecrRegistryRegex.FindStringSubmatch(registry)
	if len(matches) != 3 {
		return "", false
	}
	return matches[2], true
}
