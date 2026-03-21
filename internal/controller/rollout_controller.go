package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/henryd24/kubernetes-operator-tag-images/internal/ecr"
)

const (
	envAnnotationKey                = "ecr-tagger.io/environment"
	repositoryOverrideAnnotationKey = "ecr-tagger.io/repository"
	accountIDOverrideAnnotationKey  = "ecr-tagger.io/account-id"
	lastTaggedImageAnnotationKey    = "ecr-tagger.io/last-tagged-image"
	lastTaggedGenAnnotationKey      = "ecr-tagger.io/last-tagged-generation"
)

var rolloutGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Rollout"}

type taggerFactory interface {
	ForRegion(ctx context.Context, region string) (ecr.Tagger, error)
}

type RolloutReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	ECRFactory    taggerFactory
	Recorder      record.EventRecorder
	OperatorName  string
	DefaultRegion string
}

func (r *RolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("rollout", req.NamespacedName.String())

	rollout := &unstructured.Unstructured{}
	rollout.SetGroupVersionKind(rolloutGVK)

	if err := r.Get(ctx, req.NamespacedName, rollout); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	healthy, observedGeneration := rolloutHealthy(rollout)
	if !healthy {
		generation := rollout.GetGeneration()
		if observedGeneration > 0 && observedGeneration < generation {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		return ctrl.Result{}, nil
	}

	generation := rollout.GetGeneration()
	if observedGeneration > 0 && observedGeneration < generation {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	image, err := firstContainerImage(rollout)
	if err != nil {
		log.Error(err, "rollout does not expose an image in spec.template.spec.containers")
		return ctrl.Result{}, nil
	}

	annotations := rollout.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if annotations[lastTaggedImageAnnotationKey] == image && annotations[lastTaggedGenAnnotationKey] == strconv.FormatInt(generation, 10) {
		return ctrl.Result{}, nil
	}

	parsedImage, err := ecr.ParseImageRef(image)
	if err != nil {
		log.Error(err, "image is not a valid ECR image", "image", image)
		return ctrl.Result{}, nil
	}

	environment := annotations[envAnnotationKey]
	if environment == "" {
		environment = rollout.GetNamespace()
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

	var tagSuffix string
	if parsedImage.Tag != "" {
		parts := strings.Split(parsedImage.Tag, "-")
		tagSuffix = parts[len(parts)-1]
	} else if parsedImage.Digest != "" {
		tagSuffix = parsedImage.Digest
		if len(tagSuffix) > 12 {
			tagSuffix = tagSuffix[len(tagSuffix)-12:]
		}
	} else {
		tagSuffix = "unknown"
	}

	deploymentTag := fmt.Sprintf("%s-%s", sanitizeTagValue(environment), sanitizeTagValue(tagSuffix))
	activeTag := fmt.Sprintf("active-%s", sanitizeTagValue(environment))

	deploymentTagExists, err := tagger.TagExists(ctx, destinationRepo, accountID, deploymentTag)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check deployment tag existence: %w", err)
	}

	tagsToApply := []string{activeTag}
	if !deploymentTagExists {
		tagsToApply = append([]string{deploymentTag}, tagsToApply...)
	} else {
		log.Info("deployment tag already exists, skipping deploymentTag update", "deploymentTag", deploymentTag, "repository", destinationRepo)
	}

	if err := tagger.Retag(ctx, parsedImage, accountID, destinationRepo, tagsToApply); err != nil {
		log.Error(err, "unable to tag image in ECR", "image", image, "region", region)
		r.Recorder.Eventf(rollout, "Warning", "ECRTagFailed", "Failed to tag image %s in %s: %v", image, region, err)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	base := rollout.DeepCopy()

	annotations[lastTaggedImageAnnotationKey] = image
	annotations[lastTaggedGenAnnotationKey] = strconv.FormatInt(generation, 10)
	rollout.SetAnnotations(annotations)

	if err := r.Patch(ctx, rollout, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch rollout annotations: %w", err)
	}

	r.Recorder.Eventf(rollout, "Normal", "ECRTagUpdated", "Tagged %s with tags %s in repository %s", image, strings.Join(tagsToApply, ","), destinationRepo)
	log.Info("successfully tagged image in ECR", "image", image, "tagsApplied", tagsToApply, "repository", destinationRepo)

	return ctrl.Result{}, nil
}

func (r *RolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	rollout := &unstructured.Unstructured{}
	rollout.SetGroupVersionKind(rolloutGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(rollout).
		Complete(r)
}
func readObservedGeneration(obj *unstructured.Unstructured) int64 {
	if s, found, _ := unstructured.NestedString(obj.Object, "status", "observedGeneration"); found && s != "" {
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	}
	n, _, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	return n
}

func rolloutHealthy(obj *unstructured.Unstructured) (bool, int64) {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	observedGeneration := readObservedGeneration(obj)
	generation := obj.GetGeneration()

	if !strings.EqualFold(phase, "Healthy") {
		return false, observedGeneration
	}

	if observedGeneration == 0 || observedGeneration != generation {
		return false, observedGeneration
	}

	paused, found, _ := unstructured.NestedBool(obj.Object, "spec", "paused")
	if found && paused {
		return false, observedGeneration
	}

	pauseConditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "pauseConditions")
	if found && len(pauseConditions) > 0 {
		return false, observedGeneration
	}

	return true, observedGeneration
}

func firstContainerImage(obj *unstructured.Unstructured) (string, error) {
	containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return "", err
	}
	if !found || len(containers) == 0 {
		return "", fmt.Errorf("no containers were found")
	}

	containerMap, ok := containers[0].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("container entry has invalid format")
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

func sanitizeTagValue(v string) string {
	normalized := strings.ToLower(strings.TrimSpace(v))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	normalized = strings.ReplaceAll(normalized, "/", "-")
	normalized = strings.ReplaceAll(normalized, " ", "-")

	if normalized == "" {
		return "unknown"
	}
	return normalized
}
