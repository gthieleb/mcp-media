// Package controller implements the media-controller Reconciler (T3.3):
// for workloads labeled media.media/injected=true it creates the media
// Service + inherited media Ingress (sidecar case) and patches the upstream
// Service targetPort (proxy case).
package controller

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Annotation keys read by the Reconciler (subset of the webhook contract
// that affects Service/Ingress shaping).
const (
	annInjectSidecar    = "media.media/inject-sidecar"
	annInjectProxy      = "media.media/inject-proxy"
	annUpstreamPort     = "media.media/upstream-port"
	annGroup            = "media.media/group"
	annIngressClass     = "media.media/ingress-class"
	annIngressHost      = "media.media/ingress-host"
	annCertIssuer       = "media.media/cert-issuer"
	annIngressTLSSecret = "media.media/ingress-tls-secret"

	lblInjected = "media.media/injected"
	lblGroup    = "media.media/group"

	defaultIngressClass = "tailscale"
	servePort           = 8090
	mintPort            = 8091
	proxyPort           = 5780
	mediaServiceSuffix  = "-media"
	mediaHostSuffix     = "-media"
)

// WorkloadReconciler reconciles Deployment (and StatefulSet) objects whose
// pod template was mutated by the media webhook.
type WorkloadReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the Reconciler on Deployments.
func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		Named("workload").
		Complete(r)
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch

// Reconcile ensures the media Service + Ingress (sidecar) and the proxy
// Service targetPort patch (proxy) for the annotated Deployment.
func (r *WorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &dep); err != nil {
		// Deleted workloads: owner references garbage-collect owned objects.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	annotations := dep.Spec.Template.Annotations
	if dep.Labels[lblInjected] != "true" {
		return ctrl.Result{}, nil
	}

	sidecar := annotations[annInjectSidecar] == "true"
	proxy := annotations[annInjectProxy] == "true"

	if sidecar {
		if err := r.ensureMediaService(ctx, &dep); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure media service: %w", err)
		}
		if err := r.ensureMediaIngress(ctx, &dep); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure media ingress: %w", err)
		}
	}
	if proxy {
		if err := r.patchUpstreamService(ctx, &dep, log); err != nil {
			// A missing Service is not fatal: charts may create it later;
			// requeue to retry the patch (bounded backoff by controller).
			log.Info("proxy service patch deferred, requeueing", "err", err)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}
	return ctrl.Result{}, nil
}

// ensureMediaService creates or updates <workload>-media exposing serve+mint.
func (r *WorkloadReconciler) ensureMediaService(ctx context.Context, dep *appsv1.Deployment) error {
	name := dep.GetName() + mediaServiceSuffix
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dep.GetNamespace()}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := controllerutil.SetControllerReference(dep, svc, r.Scheme); err != nil {
			return err
		}
		svc.Spec.Selector = dep.Spec.Selector.MatchLabels
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "serve", Port: servePort, TargetPort: intstr.FromInt32(servePort)},
			{Name: "mint", Port: mintPort, TargetPort: intstr.FromInt32(mintPort)},
		}
		if g := dep.Spec.Template.Annotations[annGroup]; g != "" {
			if svc.Labels == nil {
				svc.Labels = map[string]string{}
			}
			svc.Labels[lblGroup] = g
		}
		return nil
	})
	return err
}

// patchUpstreamService retargets the upstream Service port(s) at the proxy.
func (r *WorkloadReconciler) patchUpstreamService(ctx context.Context, dep *appsv1.Deployment, log logr.Logger) error {
	ann := dep.Spec.Template.Annotations
	upstream := ann[annUpstreamPort]
	if upstream == "" {
		// Auto-detect: first container port of the main container.
		for _, c := range dep.Spec.Template.Spec.Containers {
			if len(c.Ports) > 0 {
				upstream = strconv.Itoa(int(c.Ports[0].ContainerPort))
				break
			}
		}
	}
	if upstream == "" {
		return fmt.Errorf("no upstream port for deployment %s/%s", dep.GetNamespace(), dep.GetName())
	}
	up, err := strconv.Atoi(upstream)
	if err != nil {
		return fmt.Errorf("upstream-port %q is not a number", upstream)
	}

	svcName := dep.GetName()
	var svc corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: dep.GetNamespace(), Name: svcName}, &svc); err != nil {
		return err
	}
	patched := svc.DeepCopy()
	for i := range patched.Spec.Ports {
		if patched.Spec.Ports[i].Port == int32(up) {
			patched.Spec.Ports[i].TargetPort = intstr.FromInt32(proxyPort)
		}
	}
	if !reflect.DeepEqual(svc.Spec.Ports, patched.Spec.Ports) {
		if err := r.Patch(ctx, patched, client.MergeFrom(&svc)); err != nil {
			return err
		}
		log.Info("patched upstream service targetPort", "service", svcName,
			"from", up, "to", proxyPort)
	}
	return nil
}

// deref returns the string value of a nullable string.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// hasDot reports whether the host contains a dot (FQDN vs short host).
func hasDot(s string) bool {
	return strings.Contains(s, ".")
}

// cutAtFirstDot splits s at its first dot.
func cutAtFirstDot(s string) (before, after string, found bool) {
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+1:], true
}
