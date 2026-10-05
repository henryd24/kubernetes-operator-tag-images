package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/henryd24/kubernetes-operator-tag-images/internal/ecr"
)

func newHealthyRollout(annotations map[string]interface{}, containers ...interface{}) *unstructured.Unstructured {
	rollout := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata": map[string]interface{}{
			"name":        "test-app",
			"namespace":   "rollout-test",
			"annotations": annotations,
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{"containers": containers},
			},
		},
		"status": map[string]interface{}{
			"phase":              "Healthy",
			"observedGeneration": int64(1),
		},
	}}
	rollout.SetGroupVersionKind(rolloutGVK)
	rollout.SetGeneration(1)
	return rollout
}

func reconcileRollout(t *testing.T, rollout *unstructured.Unstructured, tagger *fakeTagger) {
	t.Helper()
	scheme := runtime.NewScheme()
	reconciler := &WorkloadReconciler{
		Kind:          RolloutKind{},
		Client:        fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollout).Build(),
		Scheme:        scheme,
		ECRFactory:    &fakeTaggerFactory{tagger: tagger},
		Recorder:      record.NewFakeRecorder(10),
		DefaultRegion: "us-east-1",
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-app", Namespace: "rollout-test"}}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
}

func TestReconcileSkipAnnotation(t *testing.T) {
	rollout := newHealthyRollout(
		map[string]interface{}{skipAnnotationKey: "true"},
		map[string]interface{}{"name": "app", "image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1"},
	)
	tagger := &fakeTagger{}
	reconcileRollout(t, rollout, tagger)
	if tagger.checkedTag != "" || len(tagger.retagTags) != 0 {
		t.Fatalf("expected no ECR calls when skip annotation is set")
	}
}

func TestReconcileContainerAnnotationSelectsContainer(t *testing.T) {
	rollout := newHealthyRollout(
		map[string]interface{}{envAnnotationKey: "dev", containerAnnotationKey: "app"},
		map[string]interface{}{"name": "sidecar", "image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/proxy:v9"},
		map[string]interface{}{"name": "app", "image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v2.0.0"},
	)
	tagger := &fakeTagger{}
	reconcileRollout(t, rollout, tagger)
	if tagger.retagSource.Repository != "app" || tagger.checkedTag != "dev-v2.0.0" {
		t.Fatalf("expected the 'app' container to be tagged, got repo=%q tag=%q", tagger.retagSource.Repository, tagger.checkedTag)
	}
}

func TestReconcileContainerAnnotationMissingContainer(t *testing.T) {
	rollout := newHealthyRollout(
		map[string]interface{}{containerAnnotationKey: "missing"},
		map[string]interface{}{"name": "app", "image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1"},
	)
	tagger := &fakeTagger{}
	reconcileRollout(t, rollout, tagger)
	if len(tagger.retagTags) != 0 {
		t.Fatalf("expected no tagging when the selected container does not exist")
	}
}

func TestReconcileSkipsNonECRImages(t *testing.T) {
	rollout := newHealthyRollout(
		map[string]interface{}{},
		map[string]interface{}{"name": "app", "image": "docker.io/library/nginx:1.27"},
	)
	tagger := &fakeTagger{}
	reconcileRollout(t, rollout, tagger)
	if tagger.checkedTag != "" || len(tagger.retagTags) != 0 {
		t.Fatalf("expected no ECR calls for non-ECR images")
	}
}

func TestReconcileDefaultsToFirstContainer(t *testing.T) {
	rollout := newHealthyRollout(
		map[string]interface{}{envAnnotationKey: "dev"},
		map[string]interface{}{"name": "app", "image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1.0.0"},
		map[string]interface{}{"name": "sidecar", "image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/proxy:v9"},
	)
	tagger := &fakeTagger{}
	reconcileRollout(t, rollout, tagger)
	if tagger.retagSource.Repository != "app" {
		t.Fatalf("expected first container to be used, got %q", tagger.retagSource.Repository)
	}
}

func TestSanitizeTagValueReplacesInvalidCharacters(t *testing.T) {
	if got := sanitizeTagValue("1.0.0+build.5"); got != "1.0.0-build.5" {
		t.Fatalf("unexpected sanitized value: %s", got)
	}
	// Values that were already valid must keep producing the same tag.
	if got := sanitizeTagValue("v1.8.4"); got != "v1.8.4" {
		t.Fatalf("unexpected sanitized value: %s", got)
	}
}

func reconcileRolloutWithClient(t *testing.T, rollout *unstructured.Unstructured, tagger *fakeTagger) (ctrl.Result, *record.FakeRecorder, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	recorder := record.NewFakeRecorder(10)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollout).Build()
	reconciler := &WorkloadReconciler{
		Kind:          RolloutKind{},
		Client:        k8sClient,
		Scheme:        scheme,
		ECRFactory:    &fakeTaggerFactory{tagger: tagger},
		Recorder:      recorder,
		DefaultRegion: "us-east-1",
	}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-app", Namespace: "rollout-test"}})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	return result, recorder, k8sClient
}

func drainEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

func TestReconcileSkipsWhenOnlyGenerationChanged(t *testing.T) {
	image := "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1.0.0"
	rollout := newHealthyRollout(
		map[string]interface{}{
			envAnnotationKey:             "dev",
			lastTaggedImageAnnotationKey: image,
			lastTaggedGenAnnotationKey:   "1",
		},
		map[string]interface{}{"name": "app", "image": image},
	)
	// Simulates a scale or restart: new generation, same image.
	rollout.SetGeneration(3)
	_ = unstructured.SetNestedField(rollout.Object, int64(3), "status", "observedGeneration")

	tagger := &fakeTagger{}
	reconcileRollout(t, rollout, tagger)
	if tagger.checkedTag != "" || len(tagger.retagTags) != 0 {
		t.Fatalf("expected no ECR calls when only the generation changed")
	}
}

func TestReconcileRetagsWhenImageChanged(t *testing.T) {
	rollout := newHealthyRollout(
		map[string]interface{}{
			envAnnotationKey:             "dev",
			lastTaggedImageAnnotationKey: "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1.0.0",
			lastTaggedGenAnnotationKey:   "1",
		},
		map[string]interface{}{"name": "app", "image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1.1.0"},
	)
	tagger := &fakeTagger{}
	reconcileRollout(t, rollout, tagger)
	if !slices.Equal(tagger.retagTags, []string{"dev-v1.1.0", "active-dev"}) {
		t.Fatalf("expected retag on image change, got %v", tagger.retagTags)
	}
}

func TestReconcileImmutableRepositoryDoesNotRequeue(t *testing.T) {
	image := "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v2.0.0"
	rollout := newHealthyRollout(
		map[string]interface{}{envAnnotationKey: "dev"},
		map[string]interface{}{"name": "app", "image": image},
	)
	tagger := &fakeTagger{immutableTags: []string{"active-dev"}}
	result, recorder, k8sClient := reconcileRolloutWithClient(t, rollout, tagger)

	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for immutable repositories, got %s", result.RequeueAfter)
	}

	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(rolloutGVK)
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "test-app", Namespace: "rollout-test"}, updated); err != nil {
		t.Fatalf("failed to get rollout: %v", err)
	}
	if updated.GetAnnotations()[lastTaggedImageAnnotationKey] != image {
		t.Fatalf("expected image to be marked as processed")
	}

	events := strings.Join(drainEvents(recorder), "\n")
	if !strings.Contains(events, "ECRTagImmutable") || !strings.Contains(events, "ECRTagUpdated") {
		t.Fatalf("expected ECRTagImmutable and ECRTagUpdated events, got:\n%s", events)
	}
	if strings.Contains(events, "Tagged "+image+" with tags dev-v2.0.0,active-dev") {
		t.Fatalf("immutable tag must not be reported as applied:\n%s", events)
	}
}

func TestReconcileWarnsOnDeploymentTagCollision(t *testing.T) {
	rollout := newHealthyRollout(
		map[string]interface{}{envAnnotationKey: "prod"},
		map[string]interface{}{"name": "app", "image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1.9.0-alpha"},
	)
	tagger := &fakeTagger{tagExists: true, existingDigest: "sha256:old", sourceDigest: "sha256:new"}
	_, recorder, _ := reconcileRolloutWithClient(t, rollout, tagger)

	if tagger.checkedTag != "prod-alpha" {
		t.Fatalf("unexpected deployment tag: %s", tagger.checkedTag)
	}
	if events := strings.Join(drainEvents(recorder), "\n"); !strings.Contains(events, "DeploymentTagCollision") {
		t.Fatalf("expected DeploymentTagCollision event, got:\n%s", events)
	}
}

func TestReconcileNoCollisionWarningForSameImage(t *testing.T) {
	rollout := newHealthyRollout(
		map[string]interface{}{envAnnotationKey: "prod"},
		map[string]interface{}{"name": "app", "image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1.9.0"},
	)
	tagger := &fakeTagger{tagExists: true, existingDigest: "sha256:same", sourceDigest: "sha256:same"}
	_, recorder, _ := reconcileRolloutWithClient(t, rollout, tagger)

	if events := strings.Join(drainEvents(recorder), "\n"); strings.Contains(events, "DeploymentTagCollision") {
		t.Fatalf("did not expect a collision warning (rollback to the same image):\n%s", events)
	}
}

func TestReconcileTagSuffixFull(t *testing.T) {
	rollout := newHealthyRollout(
		map[string]interface{}{envAnnotationKey: "prod", tagSuffixAnnotationKey: "full"},
		map[string]interface{}{"name": "app", "image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1.9.0-alpha"},
	)
	tagger := &fakeTagger{}
	reconcileRollout(t, rollout, tagger)
	if !slices.Equal(tagger.retagTags, []string{"prod-v1.9.0-alpha", "active-prod"}) {
		t.Fatalf("unexpected tags with full suffix: %v", tagger.retagTags)
	}
}

func TestDeploymentTagSuffixDefaultUnchanged(t *testing.T) {
	ref := ecr.ImageRef{Tag: "v1.8.4-alpha"}
	if got := deploymentTagSuffix(ref, ""); got != "alpha" {
		t.Fatalf("default suffix changed: %s", got)
	}
	if got := deploymentTagSuffix(ref, "last-segment"); got != "alpha" {
		t.Fatalf("last-segment suffix: %s", got)
	}
}
