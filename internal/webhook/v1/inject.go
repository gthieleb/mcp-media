package v1

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// injectPod mutates pod in place according to req: appends the requested
// containers and the media volume, and labels the pod as injected.
// Idempotent: containers that already exist are left untouched.
func injectPod(pod *corev1.Pod, req injectionRequest) {
	changed := false
	if req.InjectSidecar {
		addSidecar(pod, req)
		changed = true
	}
	if req.InjectProxy {
		addProxy(pod, req)
		changed = true
	}
	if !changed {
		return
	}
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[InjectedLabel] = "true"
}

// addSidecar appends the media-sidecar container and the media volume.
func addSidecar(pod *corev1.Pod, req injectionRequest) {
	if hasContainer(pod, sidecarName) {
		return
	}
	path := req.VolumePath
	if path == "" {
		path = defaultVolumePath
	}

	env := []corev1.EnvVar{
		{Name: "MEDIA_INTERNAL_TOKEN", ValueFrom: secretKeyRef(internalTokenSecret)},
		{Name: "MEDIA_SIGNING_SECRET", ValueFrom: secretKeyRef(signingSecretName)},
		{Name: "MEDIA_ROOTS", Value: path},
	}
	// The sidecar refuses to start without MEDIA_PUBLIC_BASE_URL; default to
	// the in-cluster media Service (the Reconciler guarantees
	// <workload>-media exists) — external URLs come from the public-url
	// annotation or the Reconciler's inherited host override.
	publicURL := req.PublicURL
	if publicURL == "" {
		publicURL = fmt.Sprintf("http://%s-media.%s.svc:%d",
			injectTargetName(pod), pod.Namespace, sidecarServePort)
	}
	env = append([]corev1.EnvVar{{Name: "MEDIA_PUBLIC_BASE_URL", Value: publicURL}}, env...)

	sc := corev1.Container{
		Name:  sidecarName,
		Image: sidecarImage,
		Ports: []corev1.ContainerPort{
			{Name: "serve", ContainerPort: sidecarServePort},
			{Name: "mint", ContainerPort: sidecarMintPort},
		},
		Env:             env,
		ImagePullPolicy: pullPolicyFor(sidecarImage),
		VolumeMounts: []corev1.VolumeMount{{
			Name:      mediaVolumeName,
			MountPath: path,
			ReadOnly:  true,
		}},
	}
	pod.Spec.Containers = append(pod.Spec.Containers, sc)
	addMediaVolume(pod, req)
	mountWorkloadContainers(pod, req, path)
}

// addProxy appends the mcp-media-proxy container with its env contract.
func addProxy(pod *corev1.Pod, req injectionRequest) {
	if hasContainer(pod, proxyName) {
		return
	}
	mode := req.ProxyMode
	if mode == "" {
		mode = "full"
	}
	env := []corev1.EnvVar{
		{Name: "PROXY_MODE", Value: mode},
		{Name: "PROXY_LISTEN_ADDR", Value: ":" + strconv.Itoa(proxyListenPort)},
		{Name: "MEDIA_INTERNAL_TOKEN", ValueFrom: secretKeyRef(internalTokenSecret)},
	}
	if mode == "standalone" {
		// Standalone never forwards upstream; a leftover UPSTREAM env would
		// make the proxy refuse to start (T6.2 contract).
		if req.UpstreamPort != "" {
			env = append(env, corev1.EnvVar{
				Name:  "MEDIA_INJECTION_NOTE",
				Value: "upstream-port annotation ignored in standalone mode",
			})
		}
	} else if req.UpstreamPort != "" {
		env = append(env, corev1.EnvVar{
			Name:  "UPSTREAM_MCP_URL",
			Value: fmt.Sprintf("http://localhost:%s", req.UpstreamPort),
		})
	}
	if req.ToolMatch != "" && mode == "full" {
		env = append(env, corev1.EnvVar{Name: "TOOL_MATCH", Value: req.ToolMatch})
	}
	if req.InlineMaxBytes != "" {
		env = append(env, corev1.EnvVar{Name: "INLINE_MAX_BYTES", Value: req.InlineMaxBytes})
	}
	if mint := resolveMintURL(req); mint != "" {
		env = append(env, corev1.EnvVar{Name: "MINT_URL", Value: mint})
	}

	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:  proxyName,
		Image: proxyImage,
		Ports: []corev1.ContainerPort{{Name: "mcp", ContainerPort: proxyListenPort}},
		Env:   env,
	})
}

// resolveMintURL derives the proxy MINT_URL: explicit override first, then
// the group sidecar service, then localhost (single-pod sidecar present).
func resolveMintURL(req injectionRequest) string {
	if req.MintURL != "" {
		return req.MintURL
	}
	if req.Group != "" {
		return fmt.Sprintf("http://%s-sidecar-service:%d", req.Group, sidecarMintPort)
	}
	if req.InjectSidecar {
		return fmt.Sprintf("http://localhost:%d", sidecarMintPort)
	}
	return ""
}

// addMediaVolume appends the media volume: a readOnly-consumed PVC or an
// emptyDir fallback.
func addMediaVolume(pod *corev1.Pod, req injectionRequest) {
	for _, v := range pod.Spec.Volumes {
		if v.Name == mediaVolumeName {
			return
		}
	}
	vol := corev1.Volume{Name: mediaVolumeName}
	if req.VolumeName != "" && req.VolumeName != "empty" {
		vol.VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: req.VolumeName,
				ReadOnly:  true,
			},
		}
	} else {
		vol.VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, vol)
}

// mountWorkloadContainers gives the workload's own containers (init and
// main, skipping injected ones) a read-write mount of the media volume at
// its media root — per plan: the workload writes the volume, the sidecar
// consumes it read-only. A mount at the same path or of the same volume is
// left untouched (charts like whatsapp-mcp-go already mount their PVC).
func mountWorkloadContainers(pod *corev1.Pod, req injectionRequest, path string) {
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if c.Name == sidecarName || c.Name == proxyName {
			continue
		}
		mountIfAbsent(c, mediaVolumeName, path)
	}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name == sidecarName || c.Name == proxyName {
			continue
		}
		mountIfAbsent(c, mediaVolumeName, path)
	}
}

// mountIfAbsent adds a rw volume mount unless the container already mounts
// the volume name or already uses the mount path.
func mountIfAbsent(c *corev1.Container, volumeName, path string) {
	for _, m := range c.VolumeMounts {
		if m.Name == volumeName || m.MountPath == path {
			return
		}
	}
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      volumeName,
		MountPath: path,
		ReadOnly:  false,
	})
}

// hasContainer reports whether the pod already carries a container name.
func hasContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	return false
}

// injectTargetName derives the workload name from pod labels
// (app.kubernetes.io/instance, then app.kubernetes.io/name, then pod name).
func injectTargetName(pod *corev1.Pod) string {
	for _, key := range []string{"app.kubernetes.io/instance", "app.kubernetes.io/name", "app"} {
		if v := pod.Labels[key]; v != "" {
			return v
		}
	}
	return pod.Name
}

// pullPolicyFor returns IfNotPresent for pinned image references (any tag
// other than "latest") so locally loaded images in kind-style clusters are
// used without registry access. The :latest default (Always semantics)
// stays untouched.
func pullPolicyFor(image string) corev1.PullPolicy {
	if strings.HasSuffix(image, ":latest") || !strings.Contains(image, ":") {
		return ""
	}
	return corev1.PullIfNotPresent
}

// secretKeyRef builds a whole-secret env source (single-key secret layout:
// key "value").
func secretKeyRef(name string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
			Key:                  "value",
		},
	}
}
