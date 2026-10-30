/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"open-cluster-management.io/dynamic-scoring/test/utils"
)

const (
	loopbackClusterName = "loopback"
	hubNamespace        = "open-cluster-management"
	addonNamespace      = "dynamic-scoring"
	addonName           = "dynamic-scoring-framework"
	helmReleaseName     = "dynamic-scoring-framework-e2e"
)

var _ = Describe("Dynamic Scoring Framework chart", Ordered, func() {
	BeforeAll(func() {
		By("bootstrapping an OCM hub and registering it as a loopback managed cluster")
		Expect(bootstrapOCM()).To(Succeed())

		By("creating the optional hub registry pull secret used by the spoke agent defaults")
		Expect(createHubPullSecret()).To(Succeed())

		By("installing the chart with the locally built controller images")
		Expect(installChart()).To(Succeed())
	})

	AfterAll(func() {
		By("uninstalling the Dynamic Scoring Framework chart")
		helm := os.Getenv("HELM")
		if helm == "" {
			helm = "helm"
		}
		if _, err := utils.Run(exec.Command(
			helm, "uninstall", helmReleaseName, "--namespace", hubNamespace, "--ignore-not-found",
		)); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "warning: failed to uninstall chart: %v\n", err)
		}
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}

		By("collecting cluster state for debugging")
		for _, args := range [][]string{
			{"get", "pods", "--all-namespaces", "-o", "wide"},
			{"get", "managedclusters"},
			{"get", "managedclusteraddons", "--all-namespaces"},
			{"get", "manifestworks", "--all-namespaces"},
			{"get", "dynamicscorers", "--all-namespaces"},
			{"get", "dynamicscoringconfigs", "--all-namespaces"},
			{"get", "addonplacementscores", "--all-namespaces"},
			{"get", "events", "--all-namespaces", "--sort-by=.lastTimestamp"},
		} {
			output, err := utils.Run(exec.Command("kubectl", args...))
			if err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "debug command %q failed: %v\n", args, err)
				continue
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "$ kubectl %s\n%s\n", strings.Join(args, " "), output)
		}
	})

	It("starts both hub controllers", func() {
		for _, deployment := range []string{
			"dynamic-scoring-framework-controller",
			"dynamic-scoring-addon-controller",
		} {
			_, err := utils.Run(exec.Command(
				"kubectl", "rollout", "status", "deployment/"+deployment,
				"--namespace", hubNamespace, "--timeout=5m",
			))
			Expect(err).NotTo(HaveOccurred(), "deployment %s should become available", deployment)
		}
	})

	It("deploys the spoke agent without an AddOnDeploymentConfig", func() {
		By("waiting for OCM to enable the add-on on the loopback managed cluster")
		_, err := utils.Run(exec.Command(
			"kubectl", "wait", "--for=condition=Available",
			"managedclusteraddon/"+addonName, "--namespace", loopbackClusterName, "--timeout=5m",
		))
		Expect(err).NotTo(HaveOccurred(), "ManagedClusterAddOn should become available")

		By("waiting for the agent deployment in its default namespace")
		_, err = utils.Run(exec.Command(
			"kubectl", "rollout", "status", "deployment/dynamic-scoring-agent",
			"--namespace", addonNamespace, "--timeout=5m",
		))
		Expect(err).NotTo(HaveOccurred(), "spoke agent Deployment should become available")

		By("checking that the agent uses the environment-default image")
		image, err := utils.Run(exec.Command(
			"kubectl", "get", "deployment", "dynamic-scoring-agent", "--namespace", addonNamespace,
			"-o", "jsonpath={.spec.template.spec.containers[0].image}",
		))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(image)).To(Equal(e2eAddonImage()))

		By("checking that OCM did not attach any AddOnDeploymentConfig")
		configs, err := utils.Run(exec.Command(
			"kubectl", "get", "managedclusteraddon", addonName, "--namespace", loopbackClusterName,
			"-o", "jsonpath={.spec.configs}",
		))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(configs)).To(BeEmpty())
	})

	It("sends a score from a mock scorer without Prometheus", func() {
		By("deploying a static HTTP scorer in the loopback cluster")
		response := `{"results":[{"metric":{},"score":42}]}`
		_, err := utils.Run(exec.Command(
			"kubectl", "create", "deployment", "dsf-e2e-scorer",
			"--namespace", addonNamespace, "--image", e2eMockScorerImage(),
			"--", "/http-echo", "-listen=:8080", "-text="+response,
		))
		Expect(err).NotTo(HaveOccurred(), "mock scorer Deployment should be created")
		_, err = utils.Run(exec.Command(
			"kubectl", "expose", "deployment", "dsf-e2e-scorer", "--namespace", addonNamespace,
			"--name", "dsf-e2e-scorer", "--port=8080", "--target-port=8080",
		))
		Expect(err).NotTo(HaveOccurred(), "mock scorer Service should be created")
		_, err = utils.Run(exec.Command(
			"kubectl", "rollout", "status", "deployment/dsf-e2e-scorer",
			"--namespace", addonNamespace, "--timeout=5m",
		))
		Expect(err).NotTo(HaveOccurred(), "mock scorer should become ready")

		By("registering a scorer with no source data and an AddOnPlacementScore destination")
		manifest := fmt.Sprintf(`apiVersion: dynamic-scoring.open-cluster-management.io/v1alpha1
kind: DynamicScoringConfig
metadata:
  name: dynamic-scoring-config
  namespace: %s
spec:
  masks: []
---
apiVersion: dynamic-scoring.open-cluster-management.io/v1alpha1
kind: DynamicScorer
metadata:
  name: e2e-static-scorer
  namespace: %s
spec:
  description: Static scorer used by the e2e test
  configURL: http://dsf-e2e-scorer.%s.svc.cluster.local:8080/config
  configSyncMode: None
  location: Internal
  scoreDestination: AddOnPlacementScore
  scoreDimensionFormat: "${scoreName}"
  source:
    type: None
  scoring:
    host: http://dsf-e2e-scorer.%s.svc.cluster.local:8080
    path: /scoring
    params:
      name: e2e-static-score
      interval: 2
`, hubNamespace, hubNamespace, addonNamespace, addonNamespace)
		apply := exec.Command("kubectl", "apply", "-f", "-")
		apply.Stdin = strings.NewReader(manifest)
		_, err = utils.Run(apply)
		Expect(err).NotTo(HaveOccurred(), "DynamicScorer should be applied")

		By("waiting for the hub to mark the scorer active")
		Eventually(func() string {
			return kubectlJSONPath(
				"get", "dynamicscorer", "e2e-static-scorer", "--namespace", hubNamespace,
				"-o", "jsonpath={.status.healthStatus}",
			)
		}).WithTimeout(3 * time.Minute).WithPolling(2 * time.Second).Should(Equal("Active"))

		By("waiting for the static score to reach the managed cluster")
		Eventually(func() string {
			return kubectlJSONPath(
				"get", "addonplacementscores", "e2e-static-score", "--namespace", loopbackClusterName,
				"-o", "jsonpath={.status.scores[0].value}",
			)
		}).WithTimeout(3 * time.Minute).WithPolling(2 * time.Second).Should(Equal("42"))
	})
})

func bootstrapOCM() error {
	joinFile, err := os.CreateTemp("", "dsf-e2e-join-*.sh")
	if err != nil {
		return fmt.Errorf("create clusteradm join command file: %w", err)
	}
	joinPath := joinFile.Name()
	if err := joinFile.Close(); err != nil {
		_ = os.Remove(joinPath)
		return fmt.Errorf("close clusteradm join command file: %w", err)
	}
	defer func() { _ = os.Remove(joinPath) }()

	bundleVersion := e2eOCMBundleVersion()
	if err := runClusteradm(
		"init", "--bundle-version", bundleVersion, "--output-join-command-file", joinPath, "--wait",
	); err != nil {
		return err
	}
	if _, err := utils.Run(exec.Command(
		"kubectl", "wait", "--for=condition=Available", "deployment", "--all", "--all-namespaces", "--timeout=5m",
	)); err != nil {
		return fmt.Errorf("wait for OCM hub deployments: %w", err)
	}

	credentials, err := os.CreateTemp("", "dsf-e2e-pull-credentials-*.json")
	if err != nil {
		return fmt.Errorf("create test pull-credential file: %w", err)
	}
	credentialsPath := credentials.Name()
	defer func() { _ = os.Remove(credentialsPath) }()
	if _, err := credentials.WriteString(`{"auths":{}}`); err != nil {
		_ = credentials.Close()
		return fmt.Errorf("write test pull-credential file: %w", err)
	}
	if err := credentials.Close(); err != nil {
		return fmt.Errorf("close test pull-credential file: %w", err)
	}

	joinCommand, err := os.ReadFile(joinPath)
	if err != nil {
		return fmt.Errorf("read clusteradm join command: %w", err)
	}
	joinWrapper, err := os.CreateTemp("", "dsf-e2e-join-wrapper-*.sh")
	if err != nil {
		return fmt.Errorf("create clusteradm join wrapper: %w", err)
	}
	joinWrapperPath := joinWrapper.Name()
	defer func() { _ = os.Remove(joinWrapperPath) }()
	wrapper := strings.TrimSpace(string(joinCommand)) +
		fmt.Sprintf(" loopback --force-internal-endpoint-lookup --bundle-version=%q --image-pull-credential-file=%q\n",
			bundleVersion, credentialsPath)
	if _, err := joinWrapper.WriteString(wrapper); err != nil {
		_ = joinWrapper.Close()
		return fmt.Errorf("write clusteradm join wrapper: %w", err)
	}
	if err := joinWrapper.Close(); err != nil {
		return fmt.Errorf("close clusteradm join wrapper: %w", err)
	}
	if _, err := utils.Run(exec.Command("sh", joinWrapperPath)); err != nil {
		return fmt.Errorf("join loopback managed cluster: %w", err)
	}
	if err := runClusteradm("accept", "--clusters", loopbackClusterName, "--wait"); err != nil {
		return err
	}
	if _, err := utils.Run(exec.Command(
		"kubectl", "wait", "--for=condition=ManagedClusterConditionAvailable",
		"managedcluster/"+loopbackClusterName, "--timeout=5m",
	)); err != nil {
		return fmt.Errorf("wait for loopback ManagedCluster: %w", err)
	}

	return nil
}

func runClusteradm(args ...string) error {
	cmd := exec.Command("clusteradm", args...)
	if _, err := cmd.CombinedOutput(); err != nil {
		// Avoid including clusteradm output in logs because init output can contain bootstrap tokens.
		return fmt.Errorf("clusteradm %s failed: %w", args[0], err)
	}
	return nil
}

func createHubPullSecret() error {
	config, err := os.CreateTemp("", "dsf-e2e-hub-pull-*.json")
	if err != nil {
		return fmt.Errorf("create hub pull-secret config: %w", err)
	}
	configPath := config.Name()
	defer func() { _ = os.Remove(configPath) }()
	if _, err := config.WriteString(`{"auths":{}}`); err != nil {
		_ = config.Close()
		return fmt.Errorf("write hub pull-secret config: %w", err)
	}
	if err := config.Close(); err != nil {
		return fmt.Errorf("close hub pull-secret config: %w", err)
	}
	_, err = utils.Run(exec.Command(
		"kubectl", "create", "secret", "generic", "dynamic-scoring-addon-pull-secret",
		"--namespace", hubNamespace,
		"--from-file=.dockerconfigjson="+configPath,
		"--type=kubernetes.io/dockerconfigjson",
	))
	if err != nil {
		return fmt.Errorf("create DSF hub pull secret: %w", err)
	}
	return nil
}

func installChart() error {
	helm := os.Getenv("HELM")
	if helm == "" {
		helm = "helm"
	}
	chartPath := os.Getenv("E2E_CHART_PATH")
	if chartPath == "" {
		var err error
		chartPath, err = filepath.Abs("../../charts/dynamic-scoring-framework")
		if err != nil {
			return fmt.Errorf("resolve chart path: %w", err)
		}
	}
	controllerName, controllerTag, err := imageNameAndTag(e2eControllerImage())
	if err != nil {
		return fmt.Errorf("parse e2e controller image: %w", err)
	}
	addonNameImage, addonTag, err := imageNameAndTag(e2eAddonImage())
	if err != nil {
		return fmt.Errorf("parse e2e add-on image: %w", err)
	}
	args := []string{
		"upgrade", "--install", helmReleaseName, chartPath,
		"--namespace", hubNamespace, "--create-namespace",
		"--set-string", "hubSetting.resourceController.image.name=" + controllerName,
		"--set-string", "hubSetting.resourceController.image.tag=" + controllerTag,
		"--set-string", "hubSetting.addonController.image.name=" + addonNameImage,
		"--set-string", "hubSetting.addonController.image.tag=" + addonTag,
		"--set-string", "spokeSetting.agent.image.name=" + addonNameImage,
		"--set-string", "spokeSetting.agent.image.tag=" + addonTag,
	}
	if _, err := utils.Run(exec.Command(helm, args...)); err != nil {
		return fmt.Errorf("install DSF Helm chart: %w", err)
	}
	return nil
}

func e2eControllerImage() string {
	if image := os.Getenv("E2E_CONTROLLER_IMAGE"); image != "" {
		return image
	}
	return "quay.io/open-cluster-management/dynamic-scoring-controller:e2e"
}

func e2eAddonImage() string {
	if image := os.Getenv("E2E_ADDON_IMAGE"); image != "" {
		return image
	}
	return "quay.io/open-cluster-management/dynamic-scoring-addon:e2e"
}

func e2eMockScorerImage() string {
	if image := os.Getenv("E2E_MOCK_SCORER_IMAGE"); image != "" {
		return image
	}
	return "hashicorp/http-echo:1.0"
}

func kubectlJSONPath(args ...string) string {
	output, err := exec.Command("kubectl", args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func e2eOCMBundleVersion() string {
	if version := os.Getenv("E2E_OCM_BUNDLE_VERSION"); version != "" {
		return version
	}
	return "v1.4.0"
}

func imageNameAndTag(image string) (string, string, error) {
	separator := strings.LastIndex(image, ":")
	if separator <= strings.LastIndex(image, "/") || separator == len(image)-1 {
		return "", "", fmt.Errorf("image %q must include a tag", image)
	}
	return image[:separator], image[separator+1:], nil
}
