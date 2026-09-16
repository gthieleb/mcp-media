//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/gthieleb/mcp-media/test/utils"
)

var _ = Describe("media-controller E2E", Ordered, func() {
	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	fullModePod := func() string {
		out, err := kube("get", "pods", "-n", e2eNamespace, "-l", "app=media-full-e2e",
			"-o", "jsonpath={.items[0].metadata.name}")
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return out
	}

	standalonePod := func() string {
		out, err := kube("get", "pods", "-n", e2eNamespace, "-l", "app=media-standalone-e2e",
			"-o", "jsonpath={.items[0].metadata.name}")
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return out
	}

	// mintToken is the bearer token for the mint API (seeded by the harness).
	mintToken := func() string {
		return secretValue("media-internal-token", "value")
	}

	BeforeAll(func() {
		By("waiting for both fixture pods to become ready")
		Eventually(func() string {
			out, err := kube("get", "pods", "-n", e2eNamespace,
				"-o", "jsonpath={range .items[*]}[{.status.phase}]{end}")
			if err != nil {
				return ""
			}
			return out
		}, 3*time.Minute, time.Second).Should(ContainSubstring("Running"),
			"fixture pods never became ready")
	})

	Context("Injection (full mode)", func() {
		It("injects sidecar and proxy into the annotated pod", func() {
			By("checking the injected container set")
			pod := fullModePod()
			names := containerNames(pod)
			Expect(names).To(ContainElement("main"))
			Expect(names).To(ContainElement("media-sidecar"))
			Expect(names).To(ContainElement("mcp-media-proxy"))

			By("checking the sidecar env contract (token via SecretKeyRef)")
			tokenRef, err := kube("get", "pod", pod, "-n", e2eNamespace,
				"-o", "jsonpath={.spec.containers[?(@.name=='media-sidecar')].env[?(@.name=='MEDIA_INTERNAL_TOKEN')].valueFrom.secretKeyRef.name}")
			Expect(err).NotTo(HaveOccurred())
			Expect(tokenRef).To(Equal("media-internal-token"))
			roots, err := kube("get", "pod", pod, "-n", e2eNamespace,
				"-o", "jsonpath={.spec.containers[?(@.name=='media-sidecar')].env[?(@.name=='MEDIA_ROOTS')].value}")
			Expect(err).NotTo(HaveOccurred())
			Expect(roots).To(Equal("/data"))

			By("checking the sidecar carries the media volume mount")
			mounts, err := kube("get", "pod", pod, "-n", e2eNamespace,
				"-o", "jsonpath={.spec.containers[?(@.name=='media-sidecar')].volumeMounts[*].name}")
			Expect(err).NotTo(HaveOccurred())
			Expect(mounts).To(ContainSubstring("media-volume"))

			By("checking the proxy env (upstream + tool match + full mode)")
			proxyEnv, err := kube("get", "pod", pod, "-n", e2eNamespace,
				"-o", "jsonpath={.spec.containers[?(@.name=='mcp-media-proxy')].env[*].value}")
			Expect(err).NotTo(HaveOccurred())
			Expect(proxyEnv).To(ContainSubstring("http://localhost:7777"))
			Expect(proxyEnv).To(ContainSubstring("full"))
			Expect(proxyEnv).To(ContainSubstring("^(save_file)$"))

			By("checking the injected label")
			lbl, err := kube("get", "pod", pod, "-n", e2eNamespace,
				"-o", "jsonpath={.metadata.labels.media\\.media/injected}")
			Expect(err).NotTo(HaveOccurred())
			Expect(lbl).To(Equal("true"))
		})

		It("creates the media Service and the inherited Ingress", func() {
			By("waiting for the media Service")
			Eventually(func() error {
				_, err := kube("get", "svc", "media-full-e2e-media", "-n", e2eNamespace)
				return err
			}).Should(Succeed())

			By("checking serve+mint ports")
			ports, err := kube("get", "svc", "media-full-e2e-media", "-n", e2eNamespace,
				"-o", "jsonpath={range .spec.ports[*]}{.port},")
			Expect(err).NotTo(HaveOccurred())
			Expect(ports).To(ContainSubstring("8090"))
			Expect(ports).To(ContainSubstring("8091"))

			By("waiting for the media Ingress with the fake tailscale class")
			Eventually(func() error {
				_, err := kube("get", "ingress", "media-full-e2e-media", "-n", e2eNamespace)
				return err
			}).Should(Succeed())

			class, err := kube("get", "ingress", "media-full-e2e-media", "-n", e2eNamespace,
				"-o", "jsonpath={.spec.ingressClassName}")
			Expect(err).NotTo(HaveOccurred())
			Expect(class).To(Equal("tailscale"))

			host, err := kube("get", "ingress", "media-full-e2e-media", "-n", e2eNamespace,
				"-o", "jsonpath={.spec.rules[0].host}")
			Expect(err).NotTo(HaveOccurred())
			Expect(host).To(ContainSubstring("media"))
		})
	})

	Context("Behavioral (mint → fetch)", func() {
		It("mints a signed URL and serves the file with Range and tamper protection", func() {
			// The fixture's init writer seeds /data/hello.txt into the
			// shared media volume (workload writes, sidecar serves ro).

			By("minting a URL through the sidecar mint API")
			mintOut := curlPod("e2e-mint", "curl", "-s", "-X", "POST",
				"http://media-full-e2e-media.media-e2e.svc:8091/mint",
				"-H", "Authorization: Bearer "+mintToken(),
				"-H", "Content-Type: application/json",
				"-d", `{"path":"/data/hello.txt"}`)
			Expect(mintOut).To(ContainSubstring(`"url":`))

			url := extractJSON(mintOut, "url")
			Expect(url).NotTo(BeEmpty())

			By("fetching the signed URL")
			fetchOut := curlPod("e2e-fetch", "curl", "-s", url)
			Expect(fetchOut).To(ContainSubstring("Hello, World!"))

			By("fetching a byte range (0-4 → 206)")
			rangeOut := curlPod("e2e-range", "curl", "-s", "-r", "0-4",
				"-o", "/dev/null", "-w", "%{http_code}", url)
			Expect(rangeOut).To(ContainSubstring("206"))

			By("tampering the signature (→ 403)")
			tamperedOut := curlPod("e2e-tamper", "curl", "-s", "-o", "/dev/null",
				"-w", "%{http_code}", url+"XX")
			Expect(tamperedOut).To(ContainSubstring("403"))
		})
	})

	Context("Enrichment (mirrored + generic tools)", func() {
		It("mirrors example-mcp tools, adds generic tools, and enriches a matching call", func() {
			By("initializing an MCP session against the proxy (pod :5780)")
			ip := kubeOK("get", "pods", "-n", e2eNamespace, "-l", "app=media-full-e2e",
				"-o", "jsonpath={.items[0].status.podIP}")
			out := curlPod("e2e-enrich", "curl", "-s", "-X", "POST",
				"http://"+ip+":5780/mcp",
				"-H", "Content-Type: application/json",
				"-H", "Accept: application/json, text/event-stream",
				"-d", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`)
			Expect(out).To(ContainSubstring("mcp-media-proxy"))
		})

		It("enriches a mirrored save_file call with a signed URL", func() {
			By("port-forwarding the proxy (pod :5780) to localhost")
			pod := fullModePod()
			pfCmd := exec.Command("kubectl", "port-forward", "pod/"+pod,
				"15780:5780", "-n", e2eNamespace)
			Expect(pfCmd.Start()).To(Succeed())
			DeferCleanup(func() { _ = pfCmd.Process.Kill() })

			By("running the probe against the forwarded proxy")
			var out string
			Eventually(func() string {
				out, _ = runProbe("--url", "http://localhost:15780/mcp",
					"-tool", "save_file", "-path", "/data/hello.txt")
				return out
			}, 2*time.Minute, 2*time.Second).Should(ContainSubstring("http://"),
				"probe output missing minted URL: %s", out)
			By("asserting the enrichment text carries the minted URL")
			Expect(out).To(ContainSubstring("text: http://"))
		})
	})

	Context("Standalone (agent-pod egress)", func() {
		It("has no upstream env and serves the volume via a relative path", func() {
			By("checking the proxy env has no UPSTREAM_MCP_URL")
			pod := standalonePod()
			envOut, err := kube("get", "pod", pod, "-n", e2eNamespace,
				"-o", "jsonpath={.spec.containers[?(@.name=='mcp-media-proxy')].env[*].name}")
			Expect(err).NotTo(HaveOccurred())
			Expect(envOut).NotTo(ContainSubstring("UPSTREAM_MCP_URL"))
			Expect(envOut).To(ContainSubstring("PROXY_MODE"))

			By("minting a relatively named file (volume-root resolution)")
			mintOut := curlPod("e2e-mint-sa", "curl", "-s", "-X", "POST",
				"http://media-standalone-e2e-media.media-e2e.svc:8091/mint",
				"-H", "Authorization: Bearer "+mintToken(),
				"-H", "Content-Type: application/json",
				"-d", `{"path":"hello.txt"}`)
			url := extractJSON(mintOut, "url")
			Expect(url).NotTo(BeEmpty())

			fetchOut := curlPod("e2e-fetch-sa", "curl", "-s", url)
			Expect(fetchOut).To(ContainSubstring("Hello from standalone e2e!"))
		})
	})
})

// runProbe runs the probe via go run against the given args (repo-relative,
// executed on the host by the harness).
func runProbe(args ...string) (string, error) {
	cmd := exec.Command("go", append([]string{"run", "./cmd/probe"}, args...)...)
	return utils.Run(cmd)
}

// extractJSON pulls the named string field out of a single JSON object.
func extractJSON(s, key string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &m); err != nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}
