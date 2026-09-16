package controller

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Annotation keys read by the Reconciler (subset of the webhook contract
// that affects Service/Ingress shaping).
const (
	annInjectSidecar = "media.media/inject-sidecar"
	annInjectProxy   = "media.media/inject-proxy"
	annUpstreamPort  = "media.media/upstream-port"
	annGroup         = "media.media/group"

	lblInjected = "media.media/injected"
	lblGroup    = "media.media/group"

	defaultIngressClass = "tailscale"
	servePort           = 8090
	mintPort            = 8091
	proxyPort           = 5780
	mediaServiceSuffix  = "-media"
	mediaHostSuffix     = "-media"
)

// workloads is the generic accessor the reconciler uses to treat Deployments
// and StatefulSets uniformly: both expose annotations, selector labels and
// are valid owner references.
type workloads interface {
	GetName() string
	GetNamespace() string
	GetPodTemplateAnnotations() map[string]string
	GetSelectorMatchLabels() map[string]string
	GetFirstContainerPort() string
	// Owner returns the concrete workload object for controller references.
	Owner() client.Object
}

// DeploymentAdapter adapts an appsv1.Deployment to the workloads interface.
type DeploymentAdapter struct{ D *appsv1.Deployment }

func (a DeploymentAdapter) GetName() string { return a.D.GetName() }
func (a DeploymentAdapter) GetNamespace() string {
	return a.D.GetNamespace()
}

func (a DeploymentAdapter) GetOwnerReferences() []metav1.OwnerReference {
	return a.D.GetOwnerReferences()
}

func (a DeploymentAdapter) GetPodTemplateAnnotations() map[string]string {
	return a.D.Spec.Template.Annotations
}

func (a DeploymentAdapter) GetSelectorMatchLabels() map[string]string {
	return a.D.Spec.Selector.MatchLabels
}

func (a DeploymentAdapter) GetFirstContainerPort() string {
	for _, c := range a.D.Spec.Template.Spec.Containers {
		if len(c.Ports) > 0 {
			return strconv.Itoa(int(c.Ports[0].ContainerPort))
		}
	}
	return ""
}
func (a DeploymentAdapter) Owner() client.Object { return a.D }

// StatefulSetAdapter adapts an appsv1.StatefulSet to the workloads interface.
type StatefulSetAdapter struct{ S *appsv1.StatefulSet }

func (a StatefulSetAdapter) GetName() string { return a.S.GetName() }
func (a StatefulSetAdapter) GetNamespace() string {
	return a.S.GetNamespace()
}

func (a StatefulSetAdapter) GetOwnerReferences() []metav1.OwnerReference {
	return a.S.GetOwnerReferences()
}

func (a StatefulSetAdapter) GetPodTemplateAnnotations() map[string]string {
	return a.S.Spec.Template.Annotations
}

func (a StatefulSetAdapter) GetSelectorMatchLabels() map[string]string {
	return a.S.Spec.Selector.MatchLabels
}

func (a StatefulSetAdapter) GetFirstContainerPort() string {
	for _, c := range a.S.Spec.Template.Spec.Containers {
		if len(c.Ports) > 0 {
			return strconv.Itoa(int(c.Ports[0].ContainerPort))
		}
	}
	return ""
}
func (a StatefulSetAdapter) Owner() client.Object { return a.S }

// WorkloadReconciler reconciles Deployment and StatefulSet objects whose
// pod template was mutated by the media webhook.
type WorkloadReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the Reconciler on Deployments and StatefulSets.
// controller-runtime allows a single For(); the StatefulSet watch is wired
// as a secondary source without owner projection.
func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		Watches(
			&appsv1.StatefulSet{},
			&handler.EnqueueRequestForObject{},
		).
		Named("workload").
		Complete(r)
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch

// Reconcile ensures the media Service + Ingress (sidecar) and the proxy
// Service targetPort patch (proxy) for the annotated workload.
func (r *WorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var dep appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &dep)
	switch {
	case err == nil:
		return r.reconcileWorkload(ctx, DeploymentAdapter{D: &dep}, log)
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	var sts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &sts); err != nil {
		// Deleted workloads: owner references garbage-collect owned objects.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return r.reconcileWorkload(ctx, StatefulSetAdapter{S: &sts}, log)
}

// reconcileWorkload applies the media shaping to any supported workload.
func (r *WorkloadReconciler) reconcileWorkload(ctx context.Context, w workloads, log logr.Logger) (ctrl.Result, error) {
	annotations := w.GetPodTemplateAnnotations()
	// The injected label lives on the workload's own metadata (set by the
	// webhook on the pod template, which for these workloads is the
	// template metadata).
	sidecar := annotations[annInjectSidecar] == "true"
	proxy := annotations[annInjectProxy] == "true"
	if !sidecar && !proxy {
		return ctrl.Result{}, nil
	}

	if sidecar {
		if err := r.ensureMediaService(ctx, w); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure media service: %w", err)
		}
		if err := r.ensureMediaIngress(ctx, w); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure media ingress: %w", err)
		}
	}
	if proxy {
		if err := r.patchUpstreamService(ctx, w, log); err != nil {
			// A missing Service is not fatal: charts may create it later;
			// requeue to retry the patch (bounded backoff by controller).
			log.Info("proxy service patch deferred, requeueing", "err", err)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}
	return ctrl.Result{}, nil
}

// ensureMediaService creates or updates <workload>-media exposing serve+mint.
func (r *WorkloadReconciler) ensureMediaService(ctx context.Context, w workloads) error {
	name := w.GetName() + mediaServiceSuffix
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.GetNamespace()}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := controllerutil.SetControllerReference(w.Owner(), svc, r.Scheme); err != nil {
			return err
		}
		svc.Spec.Selector = w.GetSelectorMatchLabels()
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "serve", Port: servePort, TargetPort: intstr.FromInt32(servePort)},
			{Name: "mint", Port: mintPort, TargetPort: intstr.FromInt32(mintPort)},
		}
		if g := w.GetPodTemplateAnnotations()[annGroup]; g != "" {
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
func (r *WorkloadReconciler) patchUpstreamService(ctx context.Context, w workloads, log logr.Logger) error {
	ann := w.GetPodTemplateAnnotations()
	upstream := ann[annUpstreamPort]
	if upstream == "" {
		// Auto-detect via the pod template: first container port.
		upstream = w.GetFirstContainerPort()
	}
	if upstream == "" {
		return fmt.Errorf("no upstream port for workload %s/%s", w.GetNamespace(), w.GetName())
	}
	up, err := strconv.Atoi(upstream)
	if err != nil {
		return fmt.Errorf("upstream-port %q is not a number", upstream)
	}

	svcName := w.GetName()
	var svc corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: w.GetNamespace(), Name: svcName}, &svc); err != nil {
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
