package controller

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/henryd24/kubernetes-operator-tag-images/internal/ecr"
)

const (
	envAnnotationKey                = "ecr-tagger.io/environment"
	repositoryOverrideAnnotationKey = "ecr-tagger.io/repository"
	accountIDOverrideAnnotationKey  = "ecr-tagger.io/account-id"
	skipAnnotationKey               = "ecr-tagger.io/skip"
	containerAnnotationKey          = "ecr-tagger.io/container"
	tagSuffixAnnotationKey          = "ecr-tagger.io/tag-suffix"
	lastTaggedImageAnnotationKey    = "ecr-tagger.io/last-tagged-image"
	lastTaggedGenAnnotationKey      = "ecr-tagger.io/last-tagged-generation"

	// EnabledLabelKey opts a Deployment in. A label (not an annotation) is used so
	// the operator can filter Deployments server side and only cache opted-in ones.
	EnabledLabelKey = "ecr-tagger.io/enabled"
)

// invalidTagCharsRegex matches characters ECR rejects in image tags.
var invalidTagCharsRegex = regexp.MustCompile(`[^a-z0-9._-]`)

type taggerFactory interface {
	ForRegion(ctx context.Context, region string) (ecr.Tagger, error)
}

// WorkloadKind captures what differs between the workload types the operator handles.
type WorkloadKind interface {
	GVK() schema.GroupVersionKind
	// Inspect reports whether the workload finished rolling out and, when it did,
	// the containers of the pod template that is now running. Errors are retried
	// with exponential backoff, so they must only be returned for transient failures.
	Inspect(ctx context.Context, reader client.Reader, obj *unstructured.Unstructured) (WorkloadState, error)
}

// WorkloadState is the result of WorkloadKind.Inspect.
type WorkloadState struct {
	Ready        bool
	RequeueAfter time.Duration
	Containers   []interface{}
}

// WorkloadReconciler retags the image of a workload in ECR once it is fully rolled out.
type WorkloadReconciler struct {
	client.Client
	// APIReader reads objects outside the cache, such as the workload referenced by
	// a Rollout's workloadRef. Defaults to Client when nil.
	APIReader     client.Reader
	Scheme        *runtime.Scheme
	ECRFactory    taggerFactory
	Recorder      record.EventRecorder
	OperatorName  string
	DefaultRegion string
	Kind          WorkloadKind
	// MaxConcurrentReconciles defaults to 1 when unset.
	MaxConcurrentReconciles int
	// RateLimiter controls the backoff on errors. Defaults to DefaultRateLimiter.
	RateLimiter workqueue.TypedRateLimiter[reconcile.Request]
}

// DefaultRateLimiter backs off exponentially from 5s up to 15m, so permanent
// errors (AccessDenied, RepositoryNotFound, ...) do not hammer the AWS API.
func DefaultRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Second, 15*time.Minute)
}

func (r *WorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	kind := strings.ToLower(r.Kind.GVK().Kind)
	log := ctrl.LoggerFrom(ctx).WithValues(kind, req.NamespacedName.String())

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.Kind.GVK())

	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if skip, _ := strconv.ParseBool(annotations[skipAnnotationKey]); skip {
		return ctrl.Result{}, nil
	}

	state, err := r.Kind.Inspect(ctx, r.reader(), obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !state.Ready {
		return ctrl.Result{RequeueAfter: state.RequeueAfter}, nil
	}

	image, err := selectContainerImage(state.Containers, annotations[containerAnnotationKey])
	if err != nil {
		log.Error(err, "workload does not expose a usable container image")
		return ctrl.Result{}, nil
	}

	// Only an image change requires retagging: spec changes that keep the image
	// (scaling, restarts, resource tweaks) bump the generation but not the image.
	if annotations[lastTaggedImageAnnotationKey] == image {
		return ctrl.Result{}, nil
	}

	parsedImage, err := ecr.ParseImageRef(image)
	if err != nil {
		log.Error(err, "image is not a valid ECR image", "image", image)
		return ctrl.Result{}, nil
	}
	if !parsedImage.IsECR() {
		// Retrying would never succeed: the source registry is not ECR.
		log.V(1).Info("image registry is not ECR, skipping", "image", image)
		return ctrl.Result{}, nil
	}

	environment := annotations[envAnnotationKey]
	if environment == "" {
		environment = obj.GetNamespace()
	}

	destinationRepo := annotations[repositoryOverrideAnnotationKey]
	if destinationRepo == "" {
		destinationRepo = parsedImage.Repository
	}

	accountID := annotations[accountIDOverrideAnnotationKey]
	if accountID == "" {
		accountID = parsedImage.AccountId
	}

	region := parsedImage.Region
	if region == "" {
		region = r.DefaultRegion
	}
	if region == "" {
		return ctrl.Result{}, fmt.Errorf("could not infer AWS region from image %q and AWS_REGION is empty", image)
	}

	tagger, err := r.ECRFactory.ForRegion(ctx, region)
	if err != nil {
		return ctrl.Result{}, err
	}

	tagSuffix := deploymentTagSuffix(parsedImage, annotations[tagSuffixAnnotationKey])
	deploymentTag := fmt.Sprintf("%s-%s", sanitizeTagValue(environment), sanitizeTagValue(tagSuffix))
	activeTag := fmt.Sprintf("active-%s", sanitizeTagValue(environment))

	existingDigest, deploymentTagExists, err := tagger.TagDigest(ctx, destinationRepo, accountID, deploymentTag)
	if err != nil {
		tagOperationsTotal.WithLabelValues(kind, obj.GetNamespace(), resultFailure).Inc()
		return ctrl.Result{}, fmt.Errorf("check deployment tag existence: %w", err)
	}

	tagsToApply := []string{activeTag}
	if !deploymentTagExists {
		tagsToApply = append([]string{deploymentTag}, tagsToApply...)
	} else {
		log.Info("deployment tag already exists, skipping deploymentTag update", "deploymentTag", deploymentTag, "repository", destinationRepo)
	}

	result, err := tagger.Retag(ctx, parsedImage, accountID, destinationRepo, tagsToApply)
	if err != nil {
		r.Recorder.Eventf(obj, "Warning", "ECRTagFailed", "Failed to tag image %s in %s: %v", image, region, err)
		tagOperationsTotal.WithLabelValues(kind, obj.GetNamespace(), resultFailure).Inc()
		// Returning the error retries with exponential backoff (see DefaultRateLimiter).
		return ctrl.Result{}, fmt.Errorf("tag image %s in %s: %w", image, region, err)
	}

	if deploymentTagExists && existingDigest != "" && result.SourceDigest != "" && existingDigest != result.SourceDigest {
		log.Info("deployment tag already points to a different image", "deploymentTag", deploymentTag, "existingDigest", existingDigest, "imageDigest", result.SourceDigest)
		r.Recorder.Eventf(obj, "Warning", "DeploymentTagCollision",
			"Tag %s in repository %s already points to another image (%s); it was kept unchanged. Consider annotation %s=full",
			deploymentTag, destinationRepo, existingDigest, tagSuffixAnnotationKey)
	}

	if len(result.Immutable) > 0 {
		// Retrying cannot succeed while the repository is immutable, so the image is
		// marked as processed below instead of requeueing.
		log.Info("tags not updated because the repository has tag immutability enabled", "tags", result.Immutable, "repository", destinationRepo)
		r.Recorder.Eventf(obj, "Warning", "ECRTagImmutable",
			"Tags %s were not updated because repository %s has tag immutability enabled",
			strings.Join(result.Immutable, ","), destinationRepo)
		tagOperationsTotal.WithLabelValues(kind, obj.GetNamespace(), resultImmutable).Inc()
	}

	base := obj.DeepCopy()

	annotations[lastTaggedImageAnnotationKey] = image
	annotations[lastTaggedGenAnnotationKey] = strconv.FormatInt(obj.GetGeneration(), 10)
	obj.SetAnnotations(annotations)

	if err := r.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch %s annotations: %w", kind, err)
	}

	if len(result.Applied) > 0 {
		tagOperationsTotal.WithLabelValues(kind, obj.GetNamespace(), resultSuccess).Inc()
		r.Recorder.Eventf(obj, "Normal", "ECRTagUpdated", "Tagged %s with tags %s in repository %s", image, strings.Join(result.Applied, ","), destinationRepo)
		log.Info("successfully tagged image in ECR", "image", image, "tagsApplied", result.Applied, "repository", destinationRepo)
	}

	return ctrl.Result{}, nil
}

func (r *WorkloadReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.Kind.GVK())

	rateLimiter := r.RateLimiter
	if rateLimiter == nil {
		rateLimiter = DefaultRateLimiter()
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(obj).
		Named(strings.ToLower(r.Kind.GVK().Kind)).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.MaxConcurrentReconciles,
			RateLimiter:             rateLimiter,
		}).
		Complete(r)
}

// readGeneration reads an int64 generation field that may be stored as a number
// or as a string (Argo Rollouts uses strings for some of them).
func readGeneration(obj *unstructured.Unstructured, fields ...string) int64 {
	if s, found, _ := unstructured.NestedString(obj.Object, fields...); found && s != "" {
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	}
	n, _, _ := unstructured.NestedInt64(obj.Object, fields...)
	return n
}

// podTemplateContainers returns the containers found at the given path.
func podTemplateContainers(obj *unstructured.Unstructured, fields ...string) ([]interface{}, error) {
	containers, found, err := unstructured.NestedSlice(obj.Object, fields...)
	if err != nil {
		return nil, err
	}
	if !found || len(containers) == 0 {
		return nil, fmt.Errorf("no containers were found")
	}
	return containers, nil
}

// selectContainerImage returns the image of the container named containerName,
// or of the first container when containerName is empty.
func selectContainerImage(containers []interface{}, containerName string) (string, error) {
	if len(containers) == 0 {
		return "", fmt.Errorf("no containers were found")
	}

	var containerMap map[string]interface{}
	if containerName == "" {
		first, ok := containers[0].(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("container entry has invalid format")
		}
		containerMap = first
	} else {
		for _, c := range containers {
			candidate, ok := c.(map[string]interface{})
			if ok && candidate["name"] == containerName {
				containerMap = candidate
				break
			}
		}
		if containerMap == nil {
			return "", fmt.Errorf("container %q was not found", containerName)
		}
	}

	imageRaw, ok := containerMap["image"]
	if !ok {
		return "", fmt.Errorf("container does not have image field")
	}

	image, ok := imageRaw.(string)
	if !ok || strings.TrimSpace(image) == "" {
		return "", fmt.Errorf("container image is empty")
	}
	return image, nil
}

// deploymentTagSuffix returns the part of the source image reference used in the
// deployment tag. By default it is the last "-" separated segment of the tag; with
// mode "full" the whole tag is used, which avoids collisions such as
// v1.8.4-alpha and v1.9.0-alpha both producing "alpha".
func deploymentTagSuffix(ref ecr.ImageRef, mode string) string {
	switch {
	case ref.Tag != "" && strings.EqualFold(strings.TrimSpace(mode), "full"):
		return ref.Tag
	case ref.Tag != "":
		parts := strings.Split(ref.Tag, "-")
		return parts[len(parts)-1]
	case ref.Digest != "":
		suffix := ref.Digest
		if len(suffix) > 12 {
			suffix = suffix[len(suffix)-12:]
		}
		return suffix
	default:
		return "unknown"
	}
}

func sanitizeTagValue(v string) string {
	normalized := strings.ToLower(strings.TrimSpace(v))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	normalized = strings.ReplaceAll(normalized, "/", "-")
	normalized = strings.ReplaceAll(normalized, " ", "-")
	normalized = invalidTagCharsRegex.ReplaceAllString(normalized, "-")

	if normalized == "" {
		return "unknown"
	}
	return normalized
}
