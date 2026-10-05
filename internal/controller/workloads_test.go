package controller

import (
	"context"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testImage = "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1.0.0"

func newDeployment(labels map[string]interface{}, spec, status map[string]interface{}) *unstructured.Unstructured {
	if spec == nil {
		spec = map[string]interface{}{"replicas": int64(2)}
	}
	spec["template"] = map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{"name": "app", "image": testImage}},
		},
	}
	if status == nil {
		status = map[string]interface{}{
			"observedGeneration": int64(1),
			"replicas":           int64(2),
			"updatedReplicas":    int64(2),
			"availableReplicas":  int64(2),
		}
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":        "web",
			"namespace":   "prod",
			"labels":      labels,
			"annotations": map[string]interface{}{envAnnotationKey: "prod"},
		},
		"spec":   spec,
		"status": status,
	}}
	obj.SetGeneration(1)
	return obj
}

func reconcileKind(t *testing.T, kind WorkloadKind, name string, tagger *fakeTagger, objs ...client.Object) (ctrl.Result, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	reconciler := &WorkloadReconciler{
		Kind:          kind,
		Client:        fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Scheme:        scheme,
		ECRFactory:    &fakeTaggerFactory{tagger: tagger},
		Recorder:      record.NewFakeRecorder(10),
		DefaultRegion: "us-east-1",
	}
	return reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "prod"}})
}

func TestDeploymentRolledOut(t *testing.T) {
	tests := []struct {
		name   string
		spec   map[string]interface{}
		status map[string]interface{}
		want   bool
	}{
		{name: "fully rolled out", want: true},
		{
			name: "replicas defaults to 1",
			spec: map[string]interface{}{},
			status: map[string]interface{}{
				"observedGeneration": int64(1), "replicas": int64(1), "updatedReplicas": int64(1), "availableReplicas": int64(1),
			},
			want: true,
		},
		{
			name: "old pods still running",
			status: map[string]interface{}{
				"observedGeneration": int64(1), "replicas": int64(3), "updatedReplicas": int64(2), "availableReplicas": int64(3),
			},
		},
		{
			name: "new pods not available",
			status: map[string]interface{}{
				"observedGeneration": int64(1), "replicas": int64(2), "updatedReplicas": int64(2), "availableReplicas": int64(1),
			},
		},
		{
			name: "spec not observed yet",
			status: map[string]interface{}{
				"observedGeneration": int64(0), "replicas": int64(2), "updatedReplicas": int64(2), "availableReplicas": int64(2),
			},
		},
		{name: "paused", spec: map[string]interface{}{"replicas": int64(2), "paused": true}},
		{name: "scaled to zero", spec: map[string]interface{}{"replicas": int64(0)}},
		{
			name: "progress deadline exceeded",
			status: map[string]interface{}{
				"observedGeneration": int64(1), "replicas": int64(2), "updatedReplicas": int64(2), "availableReplicas": int64(2),
				"conditions": []interface{}{map[string]interface{}{"type": "Progressing", "reason": "ProgressDeadlineExceeded"}},
			},
		},
	}
	for _, tt := range tests {
		if got := deploymentRolledOut(newDeployment(nil, tt.spec, tt.status)); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestReconcileDeploymentWithLabel(t *testing.T) {
	deployment := newDeployment(map[string]interface{}{EnabledLabelKey: "true"}, nil, nil)
	tagger := &fakeTagger{}
	if _, err := reconcileKind(t, DeploymentKind{}, "web", tagger, deployment); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if !slices.Equal(tagger.retagTags, []string{"prod-v1.0.0", "active-prod"}) {
		t.Fatalf("unexpected tags: %v", tagger.retagTags)
	}
}

func TestReconcileDeploymentWithoutLabelIsIgnored(t *testing.T) {
	deployment := newDeployment(nil, nil, nil)
	tagger := &fakeTagger{}
	if _, err := reconcileKind(t, DeploymentKind{}, "web", tagger, deployment); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if tagger.checkedTag != "" || len(tagger.retagTags) != 0 {
		t.Fatalf("expected Deployments without the opt-in label to be ignored")
	}
}

func newWorkloadRefRollout(workloadObservedGeneration interface{}) *unstructured.Unstructured {
	status := map[string]interface{}{"phase": "Healthy", "observedGeneration": int64(1)}
	if workloadObservedGeneration != nil {
		status["workloadObservedGeneration"] = workloadObservedGeneration
	}
	rollout := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata": map[string]interface{}{
			"name":        "web-rollout",
			"namespace":   "prod",
			"annotations": map[string]interface{}{envAnnotationKey: "prod"},
		},
		"spec": map[string]interface{}{
			"workloadRef": map[string]interface{}{"apiVersion": "apps/v1", "kind": "Deployment", "name": "web"},
		},
		"status": status,
	}}
	rollout.SetGeneration(1)
	return rollout
}

func TestReconcileRolloutWorkloadRef(t *testing.T) {
	// The referenced Deployment is scaled to zero by Argo Rollouts and has no opt-in label.
	deployment := newDeployment(nil, map[string]interface{}{"replicas": int64(0)}, map[string]interface{}{})
	deployment.SetGeneration(4)

	tagger := &fakeTagger{}
	if _, err := reconcileKind(t, RolloutKind{}, "web-rollout", tagger, newWorkloadRefRollout("4"), deployment); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if tagger.retagSource.Repository != "app" || !slices.Equal(tagger.retagTags, []string{"prod-v1.0.0", "active-prod"}) {
		t.Fatalf("expected the workloadRef image to be tagged, got repo=%q tags=%v", tagger.retagSource.Repository, tagger.retagTags)
	}
}

func TestReconcileRolloutWorkloadRefWaitsForArgo(t *testing.T) {
	deployment := newDeployment(nil, nil, map[string]interface{}{})
	deployment.SetGeneration(5)

	tagger := &fakeTagger{}
	result, err := reconcileKind(t, RolloutKind{}, "web-rollout", tagger, newWorkloadRefRollout("4"), deployment)
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if result.RequeueAfter != rolloutRequeueAfter || len(tagger.retagTags) != 0 {
		t.Fatalf("expected to wait until Argo observes the new workload generation, got %+v tags=%v", result, tagger.retagTags)
	}
}

func TestReconcileRolloutWorkloadRefMissingTarget(t *testing.T) {
	tagger := &fakeTagger{}
	result, err := reconcileKind(t, RolloutKind{}, "web-rollout", tagger, newWorkloadRefRollout(nil))
	if err != nil || result.RequeueAfter != 0 || len(tagger.retagTags) != 0 {
		t.Fatalf("expected a missing workloadRef target to be skipped, got result=%+v err=%v", result, err)
	}
}

func TestParseNamespaces(t *testing.T) {
	if got := ParseNamespaces(" prod, ,staging ,"); !slices.Equal(got, []string{"prod", "staging"}) {
		t.Fatalf("unexpected namespaces: %v", got)
	}
	if got := ParseNamespaces(""); got != nil {
		t.Fatalf("expected nil for empty input, got %v", got)
	}
}
