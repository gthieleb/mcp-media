package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// newTestStatefulSet builds a StatefulSet with the given pod annotations.
func newTestStatefulSet(name string, annotations map[string]string, containerPort int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   testNS,
			Annotations: annotations,
			Labels: map[string]string{
				injectedLabel: "true",
			},
		},
		Spec: appsv1.StatefulSetSpec{
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

var _ = Describe("WorkloadReconciler with StatefulSet", func() {
	It("Should create Service and Ingress for an annotated StatefulSet", func() {
		By("creating an annotated sidecar StatefulSet")
		sts := newTestStatefulSet("sts-sidecar-test", map[string]string{
			"media.media/inject-sidecar": "true",
		}, 7777)
		Expect(k8sClient.Create(ctx, sts)).NotTo(HaveOccurred())

		By("waiting for the media Service to appear")
		var svc *corev1.Service
		Eventually(func() error {
			var err error
			svc, err = fetchService(testNS, "sts-sidecar-test-media")
			return err
		}, timeout, interval).Should(Succeed())
		Expect(servicePort(svc, "serve")).To(Equal(int32(8090)))

		By("verifying the owner reference points at the StatefulSet")
		Expect(hasOwnerRefSts(svc, sts)).To(BeTrue())
	})
})

// hasOwnerRefSts reports whether obj is owned by the StatefulSet.
func hasOwnerRefSts(obj interface {
	GetOwnerReferences() []metav1.OwnerReference
}, sts *appsv1.StatefulSet,
) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == sts.UID && ref.Kind == "StatefulSet" {
			return true
		}
	}
	return false
}

var _ = types.NamespacedName{}
