package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	reconciler := &RolloutReconciler{
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
