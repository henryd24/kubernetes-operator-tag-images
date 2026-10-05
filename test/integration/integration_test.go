// Package integration runs the controllers against a real API server (envtest).
// Run them with "make test-integration"; they are skipped when KUBEBUILDER_ASSETS is unset.
package integration

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/henryd24/kubernetes-operator-tag-images/internal/controller"
	"github.com/henryd24/kubernetes-operator-tag-images/internal/ecr"
)

const (
	watchedNamespace   = "prod"
	unwatchedNamespace = "other"
	image              = "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1.0.0"
	newImage           = "123456789012.dkr.ecr.us-east-1.amazonaws.com/app:v1.1.0"
)

var k8sClient client.Client

// recordingTagger is a thread-safe ecr.Tagger that records every Retag call.
type recordingTagger struct {
	mu    sync.Mutex
	calls []retagCall
}

type retagCall struct {
	repository string
	tags       []string
}

func (r *recordingTagger) Retag(_ context.Context, source ecr.ImageRef, _ string, repository string, tags []string) (ecr.RetagResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, retagCall{repository: repository, tags: slices.Clone(tags)})
	return ecr.RetagResult{SourceDigest: "sha256:" + source.Tag, Applied: tags}, nil
}

func (r *recordingTagger) TagDigest(context.Context, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (r *recordingTagger) taggedWith(tag string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, call := range r.calls {
		if slices.Contains(call.tags, tag) {
			count++
		}
	}
	return count
}

func (r *recordingTagger) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

type recordingFactory struct{ tagger *recordingTagger }

func (f recordingFactory) ForRegion(context.Context, string) (ecr.Tagger, error) {
	return f.tagger, nil
}

var tagger = &recordingTagger{}

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// Integration tests need envtest binaries; see "make test-integration".
		os.Exit(0)
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("testdata")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}

	for _, ns := range []string{watchedNamespace, unwatchedNamespace} {
		if err := k8sClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			panic(err)
		}
	}

	opts := controller.Options{
		WatchNamespaces:   []string{watchedNamespace},
		EnableDeployments: true,
		DefaultRegion:     "us-east-1",
		ECRFactory:        recordingFactory{tagger: tagger},
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Cache:   controller.CacheOptions(opts),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		panic(err)
	}
	if err := controller.SetupControllers(mgr, opts); err != nil {
		panic(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic(err)
		}
	}()

	code := m.Run()
	cancel()
	_ = env.Stop()
	os.Exit(code)
}

func eventually(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", msg)
}

func consistently(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("condition no longer holds: %s", msg)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func newRollout(namespace, name, env, img string) *unstructured.Unstructured {
	rollout := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata": map[string]interface{}{
			"name":        name,
			"namespace":   namespace,
			"annotations": map[string]interface{}{"ecr-tagger.io/environment": env},
		},
		"spec": map[string]interface{}{
			"replicas": int64(1),
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{map[string]interface{}{"name": "app", "image": img}},
				},
			},
		},
	}}
	return rollout
}

// markRolloutHealthy simulates the Argo Rollouts controller finishing a rollout.
func markRolloutHealthy(t *testing.T, rollout *unstructured.Unstructured) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(rollout), rollout); err != nil {
			return err
		}
		rollout.Object["status"] = map[string]interface{}{
			"phase":              "Healthy",
			"observedGeneration": rollout.GetGeneration(),
		}
		return k8sClient.Status().Update(context.Background(), rollout)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// updateObject applies mutate on the latest version, retrying on conflicts caused
// by the operator patching its annotations concurrently.
func updateObject(t *testing.T, obj *unstructured.Unstructured, mutate func(*unstructured.Unstructured)) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			return err
		}
		mutate(obj)
		return k8sClient.Update(context.Background(), obj)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func annotation(t *testing.T, obj *unstructured.Unstructured, key string) string {
	t.Helper()
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(obj.GroupVersionKind())
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), current); err != nil {
		t.Fatal(err)
	}
	return current.GetAnnotations()[key]
}

func TestRolloutIsTaggedOnlyWhenImageChanges(t *testing.T) {
	ctx := context.Background()
	rollout := newRollout(watchedNamespace, "api", "rollout-env", image)
	if err := k8sClient.Create(ctx, rollout); err != nil {
		t.Fatal(err)
	}
	markRolloutHealthy(t, rollout)

	eventually(t, "rollout tagged", func() bool { return tagger.taggedWith("active-rollout-env") == 1 })
	eventually(t, "last-tagged-image annotation written", func() bool {
		return annotation(t, rollout, "ecr-tagger.io/last-tagged-image") == image
	})

	// Scaling bumps the generation but keeps the image: no new ECR calls.
	updateObject(t, rollout, func(obj *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(obj.Object, int64(3), "spec", "replicas")
	})
	markRolloutHealthy(t, rollout)
	consistently(t, "no retag after scaling", func() bool { return tagger.taggedWith("active-rollout-env") == 1 })

	// A new image is tagged again.
	updateObject(t, rollout, func(obj *unstructured.Unstructured) {
		_ = unstructured.SetNestedSlice(obj.Object, []interface{}{map[string]interface{}{"name": "app", "image": newImage}}, "spec", "template", "spec", "containers")
	})
	markRolloutHealthy(t, rollout)
	eventually(t, "new image tagged", func() bool { return tagger.taggedWith("rollout-env-v1.1.0") == 1 })
}

func TestRolloutOutsideWatchedNamespacesIsIgnored(t *testing.T) {
	rollout := newRollout(unwatchedNamespace, "api", "ignored-env", image)
	if err := k8sClient.Create(context.Background(), rollout); err != nil {
		t.Fatal(err)
	}
	markRolloutHealthy(t, rollout)
	consistently(t, "rollout in unwatched namespace not tagged", func() bool { return tagger.taggedWith("active-ignored-env") == 0 })
}

func newDeployment(name, env string, labels map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":        name,
			"namespace":   watchedNamespace,
			"labels":      labels,
			"annotations": map[string]interface{}{"ecr-tagger.io/environment": env},
		},
		"spec": map[string]interface{}{
			"replicas": int64(2),
			"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": name}},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": name}},
				"spec": map[string]interface{}{
					"containers": []interface{}{map[string]interface{}{"name": "app", "image": image}},
				},
			},
		},
	}}
}

// markDeploymentAvailable simulates the Deployment controller (not part of envtest).
func markDeploymentAvailable(t *testing.T, deployment *unstructured.Unstructured) {
	t.Helper()
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(deployment), deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Object["status"] = map[string]interface{}{
		"observedGeneration": deployment.GetGeneration(),
		"replicas":           int64(2),
		"updatedReplicas":    int64(2),
		"readyReplicas":      int64(2),
		"availableReplicas":  int64(2),
	}
	if err := k8sClient.Status().Update(context.Background(), deployment); err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentsRequireOptInLabel(t *testing.T) {
	ctx := context.Background()
	labeled := newDeployment("labeled", "deploy-env", map[string]interface{}{controller.EnabledLabelKey: "true"})
	unlabeled := newDeployment("unlabeled", "unlabeled-env", nil)
	for _, d := range []*unstructured.Unstructured{labeled, unlabeled} {
		if err := k8sClient.Create(ctx, d); err != nil {
			t.Fatal(err)
		}
		markDeploymentAvailable(t, d)
	}

	eventually(t, "labeled deployment tagged", func() bool { return tagger.taggedWith("active-deploy-env") == 1 })
	consistently(t, "unlabeled deployment not tagged", func() bool { return tagger.taggedWith("active-unlabeled-env") == 0 })
}

func TestRolloutWorkloadRef(t *testing.T) {
	ctx := context.Background()
	// The referenced Deployment is not labeled, so it is not cached: it must be read
	// through the API reader.
	target := newDeployment("ref-target", "unused", nil)
	if err := k8sClient.Create(ctx, target); err != nil {
		t.Fatal(err)
	}

	rollout := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata": map[string]interface{}{
			"name":        "ref-rollout",
			"namespace":   watchedNamespace,
			"annotations": map[string]interface{}{"ecr-tagger.io/environment": "ref-env"},
		},
		"spec": map[string]interface{}{
			"workloadRef": map[string]interface{}{"apiVersion": "apps/v1", "kind": "Deployment", "name": "ref-target"},
		},
	}}
	if err := k8sClient.Create(ctx, rollout); err != nil {
		t.Fatal(err)
	}
	markRolloutHealthy(t, rollout)

	eventually(t, "workloadRef image tagged", func() bool { return tagger.taggedWith("active-ref-env") == 1 })
	if tagger.total() == 0 {
		t.Fatal("expected tagger calls")
	}
}
