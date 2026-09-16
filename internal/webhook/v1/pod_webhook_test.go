package v1

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	sidecarContainerName = "media-sidecar"
	proxyContainerName   = "mcp-media-proxy"

	timeout  = 20 * time.Second
	interval = 250 * time.Millisecond
)

// annotatedPod builds a minimal Pod carrying media injection annotations
// in the given namespace.
func annotatedPod(ns, name string, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "example:latest",
				Ports: []corev1.ContainerPort{{ContainerPort: 7777}},
			}},
		},
	}
}

// fetchPod re-reads the pod from the API.
func fetchPod(pod *corev1.Pod) (*corev1.Pod, error) {
	var got corev1.Pod
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: pod.Namespace, Name: pod.Name,
	}, &got); err != nil {
		return nil, err
	}
	return &got, nil
}

// findContainer returns the named container or nil.
func findContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

var _ = Describe("Pod Webhook", func() {
	AfterEach(func() {
		// Best-effort cleanup of pods created by the specs below.
		for _, spec := range [][2]string{
			{"default", "ws-sidecar"},
			{"default", "ws-proxy"},
			{"default", "ws-proxy-standalone"},
			{"default", "ws-both"},
			{"media-disabled", "plain-pod"},
		} {
			var pod corev1.Pod
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: spec[0], Name: spec[1]}, &pod)
			if err == nil {
				_ = k8sClient.Delete(ctx, &pod)
			}
		}
	})

	Context("When the namespace is not enabled for media injection", func() {
		It("Should leave plain pods untouched", func() {
			By("creating a pod with injection annotations in the media-disabled namespace")
			pod := annotatedPod("media-disabled", "plain-pod", map[string]string{InjectSidecar: "true"})
			Expect(k8sClient.Create(ctx, pod)).NotTo(HaveOccurred())

			By("verifying no sidecar container was injected")
			got, err := fetchPod(pod)
			Expect(err).NotTo(HaveOccurred())
			Expect(findContainer(got, sidecarContainerName)).To(BeNil())
		})
	})

	Context("When the namespace is enabled for media injection", func() {
		It("Should inject the sidecar for inject-sidecar=true with readOnly PVC volume mount", func() {
			By("creating a pod annotated with inject-sidecar and a PVC volume")
			pod := annotatedPod("default", "ws-sidecar", map[string]string{
				InjectSidecar: "true",
				VolumeName:    "whatsapp-mcp-go",
				VolumePath:    "/project/store",
				PublicURL:     "https://whatsapp-media.example.com",
			})
			Expect(k8sClient.Create(ctx, pod)).NotTo(HaveOccurred())

			By("verifying the sidecar container exists with ports 8090/8091 and a readOnly mount")
			var got *corev1.Pod
			Eventually(func() bool {
				var err error
				got, err = fetchPod(pod)
				if err != nil {
					return false
				}
				return findContainer(got, sidecarContainerName) != nil
			}, timeout, interval).Should(BeTrue())

			sc := findContainer(got, sidecarContainerName)
			ports := map[int32]bool{}
			for _, p := range sc.Ports {
				ports[p.ContainerPort] = true
			}
			Expect(ports).To(HaveKey(int32(8090)))
			Expect(ports).To(HaveKey(int32(8091)))
			Expect(sc.VolumeMounts).To(ContainElement(corev1.VolumeMount{
				Name:      "media-volume",
				MountPath: "/project/store",
				ReadOnly:  true,
			}))
			// The sidecar volume must reference the annotated PVC.
			Expect(hasPVCVolume(got, "media-volume", "whatsapp-mcp-go")).To(BeTrue())
			// Env: signing secret + internal token via secret refs, public base URL literal.
			Expect(hasEnvFromSecret(sc, "media-signing")).To(BeTrue())
			Expect(hasEnvFromSecret(sc, "media-internal-token")).To(BeTrue())
			Expect(envValue(sc, "MEDIA_PUBLIC_BASE_URL")).To(Equal("https://whatsapp-media.example.com"))
			// The main container gains a read-write mount at the media root
			// (the workload writes the volume).
			main := findContainer(got, "main")
			Expect(main.VolumeMounts).To(ContainElement(corev1.VolumeMount{
				Name:      "media-volume",
				MountPath: "/project/store",
				ReadOnly:  false,
			}))
		})

		It("Should inject the proxy for inject-proxy=true with upstream env from annotations", func() {
			By("creating a pod annotated with inject-proxy and upstream port")
			pod := annotatedPod("default", "ws-proxy", map[string]string{
				InjectProxy:  "true",
				UpstreamPort: "5777",
				ToolMatch:    "^(download_media|send_audio_message)$",
			})
			Expect(k8sClient.Create(ctx, pod)).NotTo(HaveOccurred())

			By("verifying the proxy container exists with UPSTREAM_MCP_URL + TOOL_MATCH and no volume")
			var got *corev1.Pod
			Eventually(func() bool {
				var err error
				got, err = fetchPod(pod)
				if err != nil {
					return false
				}
				return findContainer(got, proxyContainerName) != nil
			}, timeout, interval).Should(BeTrue())

			pc := findContainer(got, proxyContainerName)
			Expect(envValue(pc, "UPSTREAM_MCP_URL")).To(Equal("http://localhost:5777"))
			Expect(envValue(pc, "TOOL_MATCH")).To(Equal("^(download_media|send_audio_message)$"))
			Expect(envValue(pc, "PROXY_MODE")).To(Equal("full"))
			Expect(envValue(pc, "PROXY_LISTEN_ADDR")).To(Equal(":5780"))
			Expect(hasEnvFromSecret(pc, "media-internal-token")).To(BeTrue())
		})

		It("Should inject the proxy in standalone mode when proxy-mode=standalone", func() {
			By("creating a pod annotated with inject-proxy in standalone mode (no upstream)")
			pod := annotatedPod("default", "ws-proxy-standalone", map[string]string{
				InjectProxy: "true",
				ProxyMode:   "standalone",
			})
			Expect(k8sClient.Create(ctx, pod)).NotTo(HaveOccurred())

			By("verifying the proxy container has PROXY_MODE=standalone and no UPSTREAM_MCP_URL")
			var got *corev1.Pod
			Eventually(func() bool {
				var err error
				got, err = fetchPod(pod)
				if err != nil {
					return false
				}
				return findContainer(got, proxyContainerName) != nil
			}, timeout, interval).Should(BeTrue())

			pc := findContainer(got, proxyContainerName)
			Expect(envValue(pc, "PROXY_MODE")).To(Equal("standalone"))
			Expect(hasEnvKey(pc, "UPSTREAM_MCP_URL")).To(BeFalse())
		})

		It("Should inject both sidecar and proxy when both annotations are set", func() {
			By("creating a pod annotated with inject-sidecar AND inject-proxy")
			pod := annotatedPod("default", "ws-both", map[string]string{
				InjectSidecar: "true",
				VolumeName:    "agent-output",
				InjectProxy:   "true",
				ProxyMode:     "standalone",
			})
			Expect(k8sClient.Create(ctx, pod)).NotTo(HaveOccurred())

			By("verifying both containers are injected")
			var got *corev1.Pod
			Eventually(func() bool {
				var err error
				got, err = fetchPod(pod)
				if err != nil {
					return false
				}
				return findContainer(got, sidecarContainerName) != nil &&
					findContainer(got, proxyContainerName) != nil
			}, timeout, interval).Should(BeTrue())

			By("verifying the pod got the media.media/injected label")
			Expect(got.Labels[InjectedLabel]).To(Equal("true"))
			By("verifying the proxy mints via localhost (single-pod default)")
			Expect(envValue(findContainer(got, proxyContainerName), "MINT_URL")).To(Equal("http://localhost:8091"))
		})
	})
})

// hasPVCVolume reports whether pod carries a volume name backed by claimName.
func hasPVCVolume(pod *corev1.Pod, name, claimName string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == name && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claimName {
			return true
		}
	}
	return false
}

// envValue returns the literal value of an env var on the container ("" if unset).
func envValue(c *corev1.Container, key string) string {
	for _, e := range c.Env {
		if e.Name == key {
			return e.Value
		}
	}
	return ""
}

// hasEnvKey reports whether the container sets the env var at all.
func hasEnvKey(c *corev1.Container, key string) bool {
	for _, e := range c.Env {
		if e.Name == key {
			return true
		}
	}
	return false
}

// hasEnvFromSecret reports whether the container sources an env var from a
// secretRef naming the given secret.
func hasEnvFromSecret(c *corev1.Container, secret string) bool {
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == secret {
			return true
		}
	}
	return false
}
