//go:build e2e
// +build e2e

package e2e

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/gthieleb/mcp-media/test/utils"
)

var (
	// controllerImage is the media-controller image built and loaded by this
	// suite (tagged :e2e by the harness — locally loaded, never pulled).
	controllerImage = envOr("CONTROLLER_IMG", "media-controller:e2e")
	// sidecarImage / proxyImage are injected by the webhook; they are passed
	// to the Helm release via --set in the harness so both point at locally
	// loaded :e2e images.
	sidecarImage = envOr("SIDECAR_IMG", "media-sidecar:e2e")
	proxyImage   = envOr("PROXY_IMG", "media-proxy:e2e")
	helmRelease  = "media-controller"

	// shouldCleanupCertManager tracks whether CertManager was installed by this suite.
	shouldCleanupCertManager = false
)

// envOr reads a named env var, falling back to def.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// TestE2E runs the e2e test suite against a pre-provisioned kind cluster.
// The harness (CI workflow / make setup-test-e2e) creates the cluster, loads
// the :e2e images, installs cert-manager, deploys the media-controller Helm
// release and seeds the media-e2e namespace. This suite only asserts.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting media-controller e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	// The harness (workflow or make) provisions the cluster and the release.
	// A lightweight readiness check keeps the suite honest when run against
	// a cluster the harness has not finished wiring yet.
	By("waiting for the media-controller deployment to be ready")
	verifyDeployed := func(g Gomega) {
		out, err := kube("rollout", "status", "deploy/"+helmRelease+"-controller-manager",
			"-n", controllerNamespace, "--timeout=10s")
		g.Expect(err).NotTo(HaveOccurred(), "media-controller not deployed: %s", out)
	}
	EventuallyWithOffset(1, verifyDeployed, 5*time.Minute, time.Second).Should(Succeed())
})

// controllerNamespace is where the Helm release deploys the manager.
const controllerNamespace = "media-controller-system"

// e2eNamespace is the namespace the fixtures run in (labeled
// media-injection=enabled by the harness).
const e2eNamespace = "media-e2e"

// kube runs kubectl and returns its combined output.
func kube(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	return utils.Run(cmd)
}

// kubeOK runs kubectl and fails the spec on error.
func kubeOK(args ...string) string {
	out, err := kube(args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "kubectl %v failed: %s", args, out)
	return out
}

// secretValue reads a Secret's data key and returns the decoded value.
func secretValue(name, key string) string {
	out := kubeOK("get", "secret", name, "-n", e2eNamespace,
		"-o", "jsonpath={.data."+key+"}")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "decode secret %s/%s", name, key)
	return string(raw)
}

// curlPod runs a curl command in an ephemeral pod, waits for completion
// and returns the container output (logs). The pod is deleted afterwards.
func curlPod(name string, args ...string) string {
	// Best-effort cleanup of a previous run with the same name.
	_, _ = kube("delete", "pod", name, "-n", e2eNamespace, "--ignore-not-found", "--wait=false")
	runArgs := append([]string{
		"run", name, "--restart=Never", "--image=curlimages/curl:latest",
		"-n", e2eNamespace, "--",
	}, args...)
	_, err := kube(runArgs...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "run pod %s failed", name)
	DeferCleanup(func() {
		_, _ = kube("delete", "pod", name, "-n", e2eNamespace, "--ignore-not-found")
	})
	EventuallyWithOffset(1, func() string {
		out, err := kube("get", "pod", name, "-n", e2eNamespace,
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return ""
		}
		return out
	}, 2*time.Minute, time.Second).Should(Or(Equal("Succeeded"), Equal("Failed")),
		"curl pod %s never completed", name)
	out, err := kube("logs", name, "-n", e2eNamespace)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "curl pod %s logs failed", name)
	return out
}

// containerNames lists the container names of a pod.
func containerNames(pod string) []string {
	out := kubeOK("get", "pod", pod, "-n", e2eNamespace,
		"-o", "jsonpath={.spec.containers[*].name}")
	return strings.Fields(out)
}
