package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var rolloutGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Rollout"}

// rolloutRequeueAfter is how long to wait while the Rollout controller catches up
// with the latest spec.
const rolloutRequeueAfter = 15 * time.Second

// workloadRefTemplatePaths maps the kinds a Rollout's workloadRef may point to,
// to the location of their containers.
var workloadRefTemplatePaths = map[string][]string{
	"Deployment":  {"spec", "template", "spec", "containers"},
	"ReplicaSet":  {"spec", "template", "spec", "containers"},
	"PodTemplate": {"template", "spec", "containers"},
}

// RolloutKind handles Argo Rollouts, either with an inline pod template or with
// a workloadRef pointing to another workload.
type RolloutKind struct{}

func (RolloutKind) GVK() schema.GroupVersionKind { return rolloutGVK }

func (RolloutKind) Inspect(ctx context.Context, reader client.Reader, obj *unstructured.Unstructured) (WorkloadState, error) {
	healthy, observedGeneration := rolloutHealthy(obj)
	if !healthy {
		if observedGeneration > 0 && observedGeneration < obj.GetGeneration() {
			return WorkloadState{RequeueAfter: rolloutRequeueAfter}, nil
		}
		return WorkloadState{}, nil
	}

	workloadRef, hasRef, _ := unstructured.NestedStringMap(obj.Object, "spec", "workloadRef")
	if !hasRef {
		containers, err := podTemplateContainers(obj, "spec", "template", "spec", "containers")
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "rollout does not expose containers in spec.template.spec.containers")
			return WorkloadState{}, nil
		}
		return WorkloadState{Ready: true, Containers: containers}, nil
	}

	return inspectWorkloadRef(ctx, reader, obj, workloadRef)
}

// inspectWorkloadRef reads the containers of the workload referenced by a Rollout.
// The referenced object is read outside the cache so the operator does not need
// to watch every Deployment in the cluster.
func inspectWorkloadRef(ctx context.Context, reader client.Reader, rollout *unstructured.Unstructured, ref map[string]string) (WorkloadState, error) {
	log := ctrl.LoggerFrom(ctx)

	path, supported := workloadRefTemplatePaths[ref["kind"]]
	if !supported || ref["name"] == "" {
		log.Error(fmt.Errorf("unsupported workloadRef %s/%s", ref["kind"], ref["name"]), "cannot resolve rollout workloadRef")
		return WorkloadState{}, nil
	}

	gv, err := schema.ParseGroupVersion(ref["apiVersion"])
	if err != nil {
		log.Error(err, "invalid workloadRef apiVersion", "apiVersion", ref["apiVersion"])
		return WorkloadState{}, nil
	}

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(gv.WithKind(ref["kind"]))
	key := client.ObjectKey{Namespace: rollout.GetNamespace(), Name: ref["name"]}
	if err := reader.Get(ctx, key, target); err != nil {
		if apierrors.IsNotFound(err) {
			// Argo Rollouts reports the Rollout as degraded; a new event will arrive once fixed.
			log.Info("workloadRef target not found", "kind", ref["kind"], "name", ref["name"])
			return WorkloadState{}, nil
		}
		return WorkloadState{}, fmt.Errorf("get workloadRef %s %s: %w", ref["kind"], key, err)
	}

	// Argo Rollouts records which generation of the referenced workload it rolled
	// out. Until it matches, the Healthy phase refers to the previous template.
	workloadObserved := readGeneration(rollout, "status", "workloadObservedGeneration")
	if workloadObserved > 0 && target.GetGeneration() > 0 && workloadObserved != target.GetGeneration() {
		return WorkloadState{RequeueAfter: rolloutRequeueAfter}, nil
	}

	containers, err := podTemplateContainers(target, path...)
	if err != nil {
		log.Error(err, "workloadRef target does not expose containers", "kind", ref["kind"], "name", ref["name"])
		return WorkloadState{}, nil
	}
	return WorkloadState{Ready: true, Containers: containers}, nil
}

func rolloutHealthy(obj *unstructured.Unstructured) (bool, int64) {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	observedGeneration := readGeneration(obj, "status", "observedGeneration")
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
