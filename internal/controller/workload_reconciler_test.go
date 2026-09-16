package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	testNS        = "media-test"
	injectedLabel = "media.media/injected"

	timeout  = 20 * time.Second
	interval = 250 * time.Millisecond
)

// newTestDeployment builds a Deployment with the given pod annotations.
func newTestDeployment(name string, annotations map[string]string, containerPort int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   testNS,
			Annotations: annotations,
			Labels: map[string]string{
				injectedLabel: "true",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": name},
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "example:latest",
						Ports: []corev1.ContainerPort{{ContainerPort: containerPort}},
					}},
				},
			},
		},
	}
}

// newIngressWithBackend builds an ingress whose backend points at svcName.
func newIngressWithBackend(name, svcName string, class, host string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   testNS,
			Annotations: map[string]string{"cert-manager.io/cluster-issuer": "letsencrypt-dns"},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			TLS: []networkingv1.IngressTLS{{
				Hosts:      []string{host},
				SecretName: host + "-tls",
			}},
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: ptrTo(networkingv1.PathTypePrefix),
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: svcName,
									Port: networkingv1.ServiceBackendPort{Number: 5777},
								},
							},
						}},
					},
				},
			}},
		},
	}
}

func ptrTo[T any](v T) *T { return &v }

// fetchService / fetchIngress read the reconciled objects.
func fetchService(ns, name string) (*corev1.Service, error) {
	var svc corev1.Service
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &svc); err != nil {
		return nil, err
	}
	return &svc, nil
}

func fetchIngress(ns, name string) (*networkingv1.Ingress, error) {
	var ing networkingv1.Ingress
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ing); err != nil {
		return nil, err
	}
	return &ing, nil
}

var _ = Describe("WorkloadReconciler", func() {
	BeforeEach(func() {
		// Namespace for all specs.
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}
		err := k8sClient.Create(ctx, ns)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	Context("When a workload carries inject-sidecar", func() {
		It("Should create the media Service with serve+mint ports and an owner reference", func() {
			By("creating an annotated sidecar deployment")
			dep := newTestDeployment("sidecar-svc-test", map[string]string{
				"media.media/inject-sidecar": "true",
				"media.media/volume-name":    "agent-output",
				"media.media/public-url":     "https://media.example.com",
			}, 7777)
			Expect(k8sClient.Create(ctx, dep)).NotTo(HaveOccurred())

			By("waiting for the media Service to appear")
			var svc *corev1.Service
			Eventually(func() error {
				var err error
				svc, err = fetchService(testNS, "sidecar-svc-test-media")
				return err
			}, timeout, interval).Should(Succeed())

			Expect(servicePort(svc, "serve")).To(Equal(int32(8090)))
			Expect(servicePort(svc, "mint")).To(Equal(int32(8091)))
			Expect(svc.Spec.Selector).To(HaveKeyWithValue("app", "sidecar-svc-test"))
			Expect(hasOwnerRef(svc, dep)).To(BeTrue())
		})

		It("Should inherit class, host schema and issuer from the existing ingress", func() {
			By("creating a deployment plus an existing tailscale ingress")
			dep := newTestDeployment("ing-inherit-test", map[string]string{
				"media.media/inject-sidecar": "true",
			}, 7777)
			Expect(k8sClient.Create(ctx, dep)).NotTo(HaveOccurred())
			existing := newIngressWithBackend("ing-inherit-test", "ing-inherit-test", "tailscale", "ing-inherit-test")
			Expect(k8sClient.Create(ctx, existing)).NotTo(HaveOccurred())

			By("waiting for the media ingress to inherit the tailscale class")
			var ing *networkingv1.Ingress
			Eventually(func() error {
				var err error
				ing, err = fetchIngress(testNS, "ing-inherit-test-media")
				return err
			}, timeout, interval).Should(Succeed())

			Expect(*ing.Spec.IngressClassName).To(Equal("tailscale"))
			Expect(ing.Spec.Rules).To(HaveLen(1))
			Expect(ing.Spec.Rules[0].Host).To(Equal("ing-inherit-test-media"))
			Expect(ing.Annotations).To(HaveKeyWithValue("cert-manager.io/cluster-issuer", "letsencrypt-dns"))
			Expect(ing.Spec.TLS).To(HaveLen(1))
			Expect(ing.Spec.TLS[0].Hosts).To(ContainElement("ing-inherit-test-media"))
			Expect(ing.Spec.TLS[0].SecretName).To(Equal("ing-inherit-test-tls"))
			Expect(ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number).To(Equal(int32(8090)))
			Expect(hasOwnerRef(ing, dep)).To(BeTrue())
		})

		It("Should fall back to the tailscale class when no existing ingress exists", func() {
			By("creating an annotated deployment without ingress")
			dep := newTestDeployment("no-ingress-test", map[string]string{
				"media.media/inject-sidecar": "true",
			}, 7777)
			Expect(k8sClient.Create(ctx, dep)).NotTo(HaveOccurred())

			By("waiting for the media ingress with the default class")
			var ing *networkingv1.Ingress
			Eventually(func() error {
				var err error
				ing, err = fetchIngress(testNS, "no-ingress-test-media")
				return err
			}, timeout, interval).Should(Succeed())
			Expect(*ing.Spec.IngressClassName).To(Equal("tailscale"))
		})

		It("Should respect ingress-host and cert-issuer overrides", func() {
			By("creating a deployment with ingress overrides")
			dep := newTestDeployment("ing-override-test", map[string]string{
				"media.media/inject-sidecar":     "true",
				"media.media/ingress-host":       "media.example.com",
				"media.media/cert-issuer":        "letsencrypt-prod",
				"media.media/ingress-class":      "traefik",
				"media.media/ingress-tls-secret": "media-tls-override",
			}, 7777)
			Expect(k8sClient.Create(ctx, dep)).NotTo(HaveOccurred())

			By("waiting for the media ingress honoring the overrides")
			var ing *networkingv1.Ingress
			Eventually(func() error {
				var err error
				ing, err = fetchIngress(testNS, "ing-override-test-media")
				return err
			}, timeout, interval).Should(Succeed())
			Expect(*ing.Spec.IngressClassName).To(Equal("traefik"))
			Expect(ing.Spec.Rules[0].Host).To(Equal("media.example.com"))
			Expect(ing.Annotations).To(HaveKeyWithValue("cert-manager.io/cluster-issuer", "letsencrypt-prod"))
			Expect(ing.Spec.TLS[0].SecretName).To(Equal("media-tls-override"))
		})
	})

	Context("When a workload carries inject-proxy", func() {
		It("Should patch the existing Service targetPort to the proxy port", func() {
			By("creating a proxy deployment plus its upstream Service")
			dep := newTestDeployment("proxy-patch-test", map[string]string{
				"media.media/inject-proxy":  "true",
				"media.media/upstream-port": "5777",
			}, 5777)
			Expect(k8sClient.Create(ctx, dep)).NotTo(HaveOccurred())
			svcIn := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "proxy-patch-test", Namespace: testNS},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"app": "proxy-patch-test"},
					Ports: []corev1.ServicePort{{
						Name:       "mcp",
						Port:       5777,
						TargetPort: intstr.FromInt32(5777),
					}},
				},
			}
			Expect(k8sClient.Create(ctx, svcIn)).NotTo(HaveOccurred())

			By("waiting for the targetPort patch to land")
			Eventually(func() int32 {
				svc, err := fetchService(testNS, "proxy-patch-test")
				if err != nil {
					return -1
				}
				for _, p := range svc.Spec.Ports {
					if p.Port == 5777 {
						return p.TargetPort.IntVal
					}
				}
				return -1
			}, timeout, interval).Should(Equal(int32(5780)))
		})

		It("Should label a group sidecar service for cross-pod mint resolution", func() {
			By("creating a sidecar deployment with group=whatsapp")
			dep := newTestDeployment("group-svc-test", map[string]string{
				"media.media/inject-sidecar": "true",
				"media.media/group":          "whatsapp",
			}, 7777)
			Expect(k8sClient.Create(ctx, dep)).NotTo(HaveOccurred())

			By("waiting for the media Service to carry the group label")
			var svc *corev1.Service
			Eventually(func() string {
				var err error
				svc, err = fetchService(testNS, "group-svc-test-media")
				if err != nil {
					return ""
				}
				return svc.Labels["media.media/group"]
			}, timeout, interval).Should(Equal("whatsapp"))
		})
	})
})

// servicePort returns the target port with the given name.
func servicePort(svc *corev1.Service, name string) int32 {
	for _, p := range svc.Spec.Ports {
		if p.Name == name {
			return p.Port
		}
	}
	return -1
}

// hasOwnerRef reports whether obj is owned by the deployment.
func hasOwnerRef(obj interface {
	GetOwnerReferences() []metav1.OwnerReference
}, dep *appsv1.Deployment,
) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == dep.UID && ref.Kind == "Deployment" {
			return true
		}
	}
	return false
}
