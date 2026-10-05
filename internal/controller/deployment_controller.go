package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var deploymentGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

// DeploymentKind handles plain Kubernetes Deployments labeled with
// ecr-tagger.io/enabled=true.
type DeploymentKind struct{}

func (DeploymentKind) GVK() schema.GroupVersionKind { return deploymentGVK }

func (DeploymentKind) Inspect(ctx context.Context, _ client.Reader, obj *unstructured.Unstructured) (WorkloadState, error) {
	// The cache is already filtered by label; this guards against other setups.
	if obj.GetLabels()[EnabledLabelKey] != "true" {
		return WorkloadState{}, nil
	}
	if !deploymentRolledOut(obj) {
		// Every status change of the Deployment triggers a new reconcile, so no requeue is needed.
		return WorkloadState{}, nil
	}

	containers, err := podTemplateContainers(obj, "spec", "template", "spec", "containers")
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "deployment does not expose containers in spec.template.spec.containers")
		return WorkloadState{}, nil
	}
	return WorkloadState{Ready: true, Containers: containers}, nil
}

// deploymentRolledOut mirrors the checks of "kubectl rollout status": the latest
// spec was observed, every replica runs the new template and is available, and
// no pods of older ReplicaSets remain.
func deploymentRolledOut(obj *unstructured.Unstructured) bool {
	if paused, _, _ := unstructured.NestedBool(obj.Object, "spec", "paused"); paused {
		return false
	}

	replicas, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	if !found {
		replicas = 1
	}
	if replicas == 0 {
		// Nothing is running (e.g. a Deployment scaled down by an Argo Rollout workloadRef).
		return false
	}

	if readGeneration(obj, "status", "observedGeneration") < obj.GetGeneration() {
		return false
	}

	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		condition, ok := c.(map[string]interface{})
		if ok && condition["type"] == "Progressing" && condition["reason"] == "ProgressDeadlineExceeded" {
			return false
		}
	}

	updated, _, _ := unstructured.NestedInt64(obj.Object, "status", "updatedReplicas")
	total, _, _ := unstructured.NestedInt64(obj.Object, "status", "replicas")
	available, _, _ := unstructured.NestedInt64(obj.Object, "status", "availableReplicas")

	return updated >= replicas && total <= updated && available >= updated
}
