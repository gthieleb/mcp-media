package controller

import (
	"context"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ensureMediaIngress creates the media Ingress: annotation overrides first,
// then inheritance from the workload's existing Ingress, then the fallback
// class.
func (r *WorkloadReconciler) ensureMediaIngress(ctx context.Context, dep *appsv1.Deployment) error {
	log := logf.FromContext(ctx)
	ann := dep.Spec.Template.Annotations
	name := dep.GetName() + mediaServiceSuffix

	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dep.GetNamespace()}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		if err := controllerutil.SetControllerReference(dep, ing, r.Scheme); err != nil {
			return err
		}

		class := ann[annIngressClass]
		host := ann[annIngressHost]
		issuer := ann[annCertIssuer]
		tlsSecret := ann[annIngressTLSSecret]

		if class == "" || host == "" || issuer == "" {
			src := r.findSourceIngress(ctx, dep)
			if src != nil {
				if class == "" {
					class = deref(src.Spec.IngressClassName)
				}
				if issuer == "" {
					issuer = src.Annotations["cert-manager.io/cluster-issuer"]
				}
				if host == "" && len(src.Spec.Rules) > 0 {
					host = inheritedHost(src.Spec.Rules[0].Host, dep.GetName())
				}
				if tlsSecret == "" {
					tlsSecret = tlsSecretOf(src)
				}
			}
		}
		if class == "" {
			class = defaultIngressClass
		}
		if host == "" {
			host = dep.GetName() + mediaHostSuffix
		}

		ing.Spec = networkingv1.IngressSpec{
			IngressClassName: &class,
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: pathTypePrefix,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: name,
									Port: networkingv1.ServiceBackendPort{Number: servePort},
								},
							},
						}},
					},
				},
			}},
		}
		if tlsSecret != "" {
			ing.Spec.TLS = []networkingv1.IngressTLS{{
				Hosts:      []string{host},
				SecretName: tlsSecret,
			}}
		}
		if issuer != "" {
			if ing.Annotations == nil {
				ing.Annotations = map[string]string{}
			}
			ing.Annotations["cert-manager.io/cluster-issuer"] = issuer
		}
		return nil
	})
	if err != nil {
		return err
	}
	log.V(1).Info("media ingress ensured", "ingress", name, "class", deref(ing.Spec.IngressClassName))
	return nil
}

// findSourceIngress locates the workload's existing Ingress by matching the
// backend service name against the deployment name or its label suffixes
// (charts like whatsapp-mcp-go split service names, e.g. <name>-wa-mcp).
func (r *WorkloadReconciler) findSourceIngress(ctx context.Context, dep *appsv1.Deployment) *networkingv1.Ingress {
	var list networkingv1.IngressList
	if err := r.List(ctx, &list, client.InNamespace(dep.GetNamespace())); err != nil {
		return nil
	}
	// Prefer the ingress whose backend service routes to this workload's
	// selector labels.
	for i := range list.Items {
		ing := &list.Items[i]
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				backend := p.Backend.Service.Name
				if backend == dep.GetName() || r.backendMatchesLabels(ctx, dep, backend) {
					return ing
				}
			}
		}
	}
	return nil
}

// backendMatchesLabels reports whether a Service named backend selects the
// deployment's pod labels.
func (r *WorkloadReconciler) backendMatchesLabels(ctx context.Context, dep *appsv1.Deployment, backend string) bool {
	var svc corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: dep.GetNamespace(), Name: backend}, &svc); err != nil {
		return false
	}
	return reflect.DeepEqual(svc.Spec.Selector, dep.Spec.Selector.MatchLabels)
}

// inheritedHost derives the media host from the source host: a full host
// ("example.tailnet.ts.net") gains the -media suffix on its first label;
// a short host ("whatsapp-mcp-go") becomes "whatsapp-mcp-go-media".
func inheritedHost(srcHost, name string) string {
	if srcHost == "" {
		return name + mediaHostSuffix
	}
	if hasDot(srcHost) {
		first, rest, _ := cutAtFirstDot(srcHost)
		return first + mediaHostSuffix + "." + rest
	}
	return srcHost + mediaHostSuffix
}

// tlsSecretOf returns the source ingress' TLS secret when it hosts the
// inherited host shape (tailscale manages its own certs; then no TLS block
// is inherited — the class handles TLS).
func tlsSecretOf(src *networkingv1.Ingress) string {
	if len(src.Spec.TLS) == 0 {
		return ""
	}
	return src.Spec.TLS[0].SecretName
}

// pathTypePrefix is a pointer to the standard Prefix path type.
var pathTypePrefix = func() *networkingv1.PathType {
	t := networkingv1.PathTypePrefix
	return &t
}()
