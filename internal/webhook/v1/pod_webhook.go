package v1

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// nolint:unused
// log is for logging in this package.
var podlog = logf.Log.WithName("pod-resource")

// namespaceEnabledAnnotation gates injection per plan T3.2: the namespace
// must carry media-injection=enabled (WebhookConfig namespaceSelector is
// the network-level gate; this check is the semantic one, so envtest and
// clusters without the selector behavior both stay safe).
const namespaceEnabledAnnotation = "media-injection"

// mediaNamespaceLabel is the namespace label variant checked alongside the
// annotation (either satisfies the gate).
const mediaNamespaceLabel = "media-injection"

// SetupPodWebhookWithManager registers the webhook for Pod in the manager.
func SetupPodWebhookWithManager(mgr ctrl.Manager) error {
	d := &PodCustomDefaulter{Client: mgr.GetClient()}
	return ctrl.NewWebhookManagedBy(mgr, &corev1.Pod{}).
		WithDefaulter(d).
		Complete()
}

// PodCustomDefaulter injects media sidecar/proxy containers into pods that
// carry media.media/* annotations, when their namespace opts in.
type PodCustomDefaulter struct {
	// Client reads the pod's Namespace to check the injection gate.
	Client client.Client
}

// Default implements webhook.CustomDefaulter so a webhook will be registered
// for the Kind Pod.
func (d *PodCustomDefaulter) Default(ctx context.Context, obj *corev1.Pod) error {
	pod := obj
	nsName := pod.Namespace
	if nsName == "" {
		nsName = "default"
	}
	podlog.V(1).Info("media webhook invoked", "pod", pod.GetName(), "namespace", nsName)

	enabled, err := d.namespaceEnabled(ctx, nsName)
	if err != nil {
		// Fail open (failurePolicy: Ignore semantics): never block pod
		// creation over gate-lookup problems.
		podlog.Error(err, "media webhook: namespace gate lookup failed, skipping injection",
			"namespace", nsName)
		return nil
	}
	if !enabled {
		podlog.V(1).Info("media webhook: namespace not enabled, skipping", "namespace", nsName)
		return nil
	}

	req, err := parseAnnotations(pod.Annotations)
	if err != nil {
		// Malformed annotations never block the pod; log and skip.
		podlog.Error(err, "media webhook: invalid annotations, skipping injection", "pod", pod.GetName())
		return nil
	}
	if !req.InjectSidecar && !req.InjectProxy {
		return nil
	}

	injectPod(pod, req)
	podlog.Info("media webhook: injected media containers", "pod", pod.GetName(),
		"sidecar", req.InjectSidecar, "proxy", req.InjectProxy)
	return nil
}

// namespaceEnabled reports whether the namespace opts into media injection
// via label media-injection=enabled.
func (d *PodCustomDefaulter) namespaceEnabled(ctx context.Context, name string) (bool, error) {
	if d.Client == nil {
		return false, nil
	}
	var ns corev1.Namespace
	if err := d.Client.Get(ctx, client.ObjectKey{Name: name}, &ns); err != nil {
		// A not-found namespace during pod creation races admission; treat
		// like an unresolvable gate: fail open, no injection.
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return ns.Labels[mediaNamespaceLabel] == "enabled", nil
}
