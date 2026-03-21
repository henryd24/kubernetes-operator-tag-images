package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/henryd24/kubernetes-operator-tag-images/internal/ecr"
)

type fakeTagger struct {
	tagExists      bool
	tagExistsErr   error
	retagErr       error
	checkedRepo    string
	checkedAccount string
	checkedTag     string
	retagTags      []string
	retagRepo      string
	retagSource    ecr.ImageRef
	retagAccount   string
}

func (f *fakeTagger) Retag(_ context.Context, source ecr.ImageRef, destinationAccountId string, destinationRepository string, tags []string) error {
	f.retagSource = source
	f.retagRepo = destinationRepository
	f.retagAccount = destinationAccountId
	f.retagTags = append([]string{}, tags...)
	return f.retagErr
}

func (f *fakeTagger) TagExists(_ context.Context, repository string, accountId string, tag string) (bool, error) {
	f.checkedRepo = repository
	f.checkedAccount = accountId
	f.checkedTag = tag
	if f.tagExistsErr != nil {
		return false, f.tagExistsErr
	}
	return f.tagExists, nil
}

type fakeTaggerFactory struct {
	tagger ecr.Tagger
}

func (f *fakeTaggerFactory) ForRegion(_ context.Context, _ string) (ecr.Tagger, error) {
	return f.tagger, nil
}

func TestRolloutHealthy(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"phase":              "Healthy",
			"observedGeneration": int64(4),
		},
	}}
	obj.SetGeneration(4)

	healthy, observedGeneration := rolloutHealthy(obj)
	if !healthy {
		t.Fatalf("expected rollout to be healthy")
	}
	if observedGeneration != 4 {
		t.Fatalf("unexpected observed generation: %d", observedGeneration)
	}
}

func TestRolloutHealthyObservedGenerationAsString(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"phase":              "Healthy",
			"observedGeneration": "5",
		},
	}}
	obj.SetGeneration(5)

	healthy, observedGeneration := rolloutHealthy(obj)
	if !healthy {
		t.Fatalf("expected rollout to be healthy with string observedGeneration")
	}
	if observedGeneration != 5 {
		t.Fatalf("unexpected observed generation: %d", observedGeneration)
	}
}

func TestRolloutHealthyReturnsFalseWhenPaused(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"paused": true,
		},
		"status": map[string]interface{}{
			"phase":              "Healthy",
			"observedGeneration": int64(2),
		},
	}}
	obj.SetGeneration(2)

	healthy, _ := rolloutHealthy(obj)
	if healthy {
		t.Fatalf("expected rollout to be unhealthy when paused")
	}
}

func TestFirstContainerImage(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1"},
					},
				},
			},
		},
	}}

	image, err := firstContainerImage(obj)
	if err != nil {
		t.Fatalf("firstContainerImage returned error: %v", err)
	}
	if image != "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1" {
		t.Fatalf("unexpected image: %s", image)
	}
}

func TestFirstContainerImageErrors(t *testing.T) {
	tests := []struct {
		name string
		obj  *unstructured.Unstructured
	}{
		{
			name: "no containers",
			obj:  &unstructured.Unstructured{Object: map[string]interface{}{}},
		},
		{
			name: "container without image",
			obj: &unstructured.Unstructured{Object: map[string]interface{}{
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{"name": "app"},
							},
						},
					},
				},
			}},
		},
		{
			name: "image empty",
			obj: &unstructured.Unstructured{Object: map[string]interface{}{
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{"image": "   "},
							},
						},
					},
				},
			}},
		},
	}

	for _, tt := range tests {
		_, err := firstContainerImage(tt.obj)
		if err == nil {
			t.Fatalf("%s: expected error", tt.name)
		}
	}
}

func TestSanitizeTagValue(t *testing.T) {
	value := sanitizeTagValue("Prod_Cluster/Blue")
	if value != "prod-cluster-blue" {
		t.Fatalf("unexpected normalized value: %s", value)
	}
}

func TestSanitizeTagValueEmpty(t *testing.T) {
	value := sanitizeTagValue("   ")
	if value != "unknown" {
		t.Fatalf("unexpected normalized value: %s", value)
	}
}

func TestReconcileRollbackSkipsExistingDeploymentTag(t *testing.T) {
	scheme := runtime.NewScheme()

	rollout := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata": map[string]interface{}{
			"name":      "test-app",
			"namespace": "rollout-test",
			"annotations": map[string]interface{}{
				"ecr-tagger.io/environment": "dev",
			},
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/nginx:v3.0.0"},
					},
				},
			},
		},
		"status": map[string]interface{}{
			"phase":              "Healthy",
			"observedGeneration": int64(1),
		},
	}}
	rollout.SetGroupVersionKind(rolloutGVK)
	rollout.SetGeneration(1)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollout).Build()
	mockTagger := &fakeTagger{tagExists: true}
	reconciler := &RolloutReconciler{
		Client:        k8sClient,
		Scheme:        scheme,
		ECRFactory:    &fakeTaggerFactory{tagger: mockTagger},
		Recorder:      record.NewFakeRecorder(10),
		DefaultRegion: "us-east-1",
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-app", Namespace: "rollout-test"}})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if mockTagger.checkedTag != "dev-v3.0.0" {
		t.Fatalf("expected deployment tag check for dev-v3.0.0, got %s", mockTagger.checkedTag)
	}

	if len(mockTagger.retagTags) != 1 || mockTagger.retagTags[0] != "active-dev" {
		t.Fatalf("expected only active-dev tag to be applied, got %v", mockTagger.retagTags)
	}

	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(rolloutGVK)
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "test-app", Namespace: "rollout-test"}, updated); err != nil {
		t.Fatalf("failed to get updated rollout: %v", err)
	}

	annotations := updated.GetAnnotations()
	if annotations[lastTaggedImageAnnotationKey] != "123456789012.dkr.ecr.us-east-1.amazonaws.com/nginx:v3.0.0" {
		t.Fatalf("unexpected %s annotation: %s", lastTaggedImageAnnotationKey, annotations[lastTaggedImageAnnotationKey])
	}
}

func TestReconcileAppliesDeploymentAndActiveTagWhenDeploymentTagDoesNotExist(t *testing.T) {
	scheme := runtime.NewScheme()

	rollout := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata": map[string]interface{}{
			"name":      "test-app",
			"namespace": "rollout-test",
			"annotations": map[string]interface{}{
				"ecr-tagger.io/environment": "dev",
			},
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/nginx:v3.0.0"},
					},
				},
			},
		},
		"status": map[string]interface{}{
			"phase":              "Healthy",
			"observedGeneration": int64(1),
		},
	}}
	rollout.SetGroupVersionKind(rolloutGVK)
	rollout.SetGeneration(1)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollout).Build()
	mockTagger := &fakeTagger{tagExists: false}
	reconciler := &RolloutReconciler{
		Client:        k8sClient,
		Scheme:        scheme,
		ECRFactory:    &fakeTaggerFactory{tagger: mockTagger},
		Recorder:      record.NewFakeRecorder(10),
		DefaultRegion: "us-east-1",
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-app", Namespace: "rollout-test"}})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if len(mockTagger.retagTags) != 2 {
		t.Fatalf("expected two tags to be applied, got %v", mockTagger.retagTags)
	}
	if mockTagger.retagTags[0] != "dev-v3.0.0" || mockTagger.retagTags[1] != "active-dev" {
		t.Fatalf("expected tags [dev-v3.0.0 active-dev], got %v", mockTagger.retagTags)
	}
}

func TestReconcileReturnsErrorWhenDeploymentTagCheckFails(t *testing.T) {
	scheme := runtime.NewScheme()

	rollout := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata": map[string]interface{}{
			"name":      "test-app",
			"namespace": "rollout-test",
			"annotations": map[string]interface{}{
				"ecr-tagger.io/environment": "dev",
			},
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/nginx:v3.0.0"},
					},
				},
			},
		},
		"status": map[string]interface{}{
			"phase":              "Healthy",
			"observedGeneration": int64(1),
		},
	}}
	rollout.SetGroupVersionKind(rolloutGVK)
	rollout.SetGeneration(1)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollout).Build()
	mockTagger := &fakeTagger{tagExistsErr: errors.New("ecr unavailable")}
	reconciler := &RolloutReconciler{
		Client:        k8sClient,
		Scheme:        scheme,
		ECRFactory:    &fakeTaggerFactory{tagger: mockTagger},
		Recorder:      record.NewFakeRecorder(10),
		DefaultRegion: "us-east-1",
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-app", Namespace: "rollout-test"}})
	if err == nil {
		t.Fatalf("expected reconcile to return error when tag existence check fails")
	}
	if len(mockTagger.retagTags) != 0 {
		t.Fatalf("expected no tags to be applied when check fails, got %v", mockTagger.retagTags)
	}
}

func TestReconcileSkipsWhenAlreadyTaggedForGeneration(t *testing.T) {
	scheme := runtime.NewScheme()

	image := "123456789012.dkr.ecr.us-east-1.amazonaws.com/nginx:v3.0.0"
	rollout := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata": map[string]interface{}{
			"name":      "test-app",
			"namespace": "rollout-test",
			"annotations": map[string]interface{}{
				"ecr-tagger.io/environment":              "dev",
				lastTaggedImageAnnotationKey:             image,
				lastTaggedGenAnnotationKey:               "1",
				repositoryOverrideAnnotationKey:          "nginx",
				accountIDOverrideAnnotationKey:           "123456789012",
				"some-other-annotation-that-should-stay": "true",
			},
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"image": image},
					},
				},
			},
		},
		"status": map[string]interface{}{
			"phase":              "Healthy",
			"observedGeneration": int64(1),
		},
	}}
	rollout.SetGroupVersionKind(rolloutGVK)
	rollout.SetGeneration(1)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollout).Build()
	mockTagger := &fakeTagger{}
	reconciler := &RolloutReconciler{
		Client:        k8sClient,
		Scheme:        scheme,
		ECRFactory:    &fakeTaggerFactory{tagger: mockTagger},
		Recorder:      record.NewFakeRecorder(10),
		DefaultRegion: "us-east-1",
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-app", Namespace: "rollout-test"}})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if mockTagger.checkedTag != "" || len(mockTagger.retagTags) != 0 {
		t.Fatalf("expected no ECR calls when already tagged, checkedTag=%q retagTags=%v", mockTagger.checkedTag, mockTagger.retagTags)
	}
}

func TestReconcileRequeuesWhenObservedGenerationIsBehind(t *testing.T) {
	scheme := runtime.NewScheme()

	rollout := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata": map[string]interface{}{
			"name":      "test-app",
			"namespace": "rollout-test",
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/nginx:v3.0.0"},
					},
				},
			},
		},
		"status": map[string]interface{}{
			"phase":              "Healthy",
			"observedGeneration": int64(1),
		},
	}}
	rollout.SetGroupVersionKind(rolloutGVK)
	rollout.SetGeneration(2)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollout).Build()
	mockTagger := &fakeTagger{}
	reconciler := &RolloutReconciler{
		Client:        k8sClient,
		Scheme:        scheme,
		ECRFactory:    &fakeTaggerFactory{tagger: mockTagger},
		Recorder:      record.NewFakeRecorder(10),
		DefaultRegion: "us-east-1",
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-app", Namespace: "rollout-test"}})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Fatalf("expected requeue after 15s, got %s", result.RequeueAfter)
	}
	if mockTagger.checkedTag != "" || len(mockTagger.retagTags) != 0 {
		t.Fatalf("expected no ECR calls while observedGeneration is behind")
	}
}

func TestReconcileRetagFailureRequeues(t *testing.T) {
	scheme := runtime.NewScheme()

	rollout := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata": map[string]interface{}{
			"name":      "test-app",
			"namespace": "rollout-test",
			"annotations": map[string]interface{}{
				"ecr-tagger.io/environment": "dev",
			},
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"image": "123456789012.dkr.ecr.us-east-1.amazonaws.com/nginx:v3.0.0"},
					},
				},
			},
		},
		"status": map[string]interface{}{
			"phase":              "Healthy",
			"observedGeneration": int64(1),
		},
	}}
	rollout.SetGroupVersionKind(rolloutGVK)
	rollout.SetGeneration(1)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollout).Build()
	mockTagger := &fakeTagger{tagExists: false, retagErr: errors.New("retag failed")}
	reconciler := &RolloutReconciler{
		Client:        k8sClient,
		Scheme:        scheme,
		ECRFactory:    &fakeTaggerFactory{tagger: mockTagger},
		Recorder:      record.NewFakeRecorder(10),
		DefaultRegion: "us-east-1",
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-app", Namespace: "rollout-test"}})
	if err != nil {
		t.Fatalf("expected nil error on retag failure path, got %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("expected requeue after 30s, got %s", result.RequeueAfter)
	}

	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(rolloutGVK)
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "test-app", Namespace: "rollout-test"}, updated); err != nil {
		t.Fatalf("failed to get rollout: %v", err)
	}
	annotations := updated.GetAnnotations()
	if annotations[lastTaggedImageAnnotationKey] != "" || annotations[lastTaggedGenAnnotationKey] != "" {
		t.Fatalf("did not expect last-tagged annotations to be updated on retag failure, got %v", annotations)
	}
}
