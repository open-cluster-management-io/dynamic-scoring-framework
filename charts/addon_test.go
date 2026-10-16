package charts

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"

	"open-cluster-management.io/dynamic-scoring/pkg/dynamic_scoring"
)

const addonAPIGroupVersion = "addon.open-cluster-management.io/v1beta1"

type resource struct {
	APIVersion string
	Kind       string
	Metadata   struct{ Name, Namespace string }
	Spec       map[string]interface{}
	Raw        string `json:"-"`
}

func TestAddonChart(t *testing.T) {
	helm := os.Getenv("HELM")
	if helm == "" {
		helm = "helm"
	}
	chart := os.Getenv("CHART_PATH")
	if chart == "" {
		chart = "dynamic-scoring-framework"
	}
	for _, tc := range []struct {
		name, image, hub, spoke string
		imagePullSecrets        []string
		values                  []string
	}{
		{
			"defaults", "quay.io/open-cluster-management/dynamic-scoring-addon:latest",
			"open-cluster-management", "dynamic-scoring",
			[]string{"open-cluster-management-image-pull-credentials", "hub-registry-secret"}, nil,
		},
		{"custom", "registry.example.com/team/agent:v1.2.3", "custom-hub", "custom-spoke",
			[]string{"private-pull", "mirror-pull"}, []string{
				"--set-string", "spokeSetting.agent.image.name=registry.example.com/team/agent",
				"--set-string", "spokeSetting.agent.image.tag=v1.2.3",
				"--set-string", "hubSetting.namespace=custom-hub",
				"--set-string", "spokeSetting.namespace=custom-spoke",
				"--set-string", "spokeSetting.agent.imagePullSecrets[0]=private-pull",
				"--set-string", "spokeSetting.agent.imagePullSecrets[1]=mirror-pull",
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"template", "test", chart}, tc.values...)
			output, err := exec.Command(helm, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}
			resources := decodeResources(t, output)
			for _, key := range []string{
				"AddOnDeploymentConfig/dynamic-scoring-addon-config",
				"AddOnDeploymentConfig/dynamic-scoring-addon-config-aarch64",
				"ClusterManagementAddOn/dynamic-scoring-framework",
			} {
				if got := resources[key].APIVersion; got != addonAPIGroupVersion {
					t.Errorf("%s apiVersion=%q; want %s", key, got, addonAPIGroupVersion)
				}
			}
			imagePullSecretsJSON, err := json.Marshal(tc.imagePullSecrets)
			if err != nil {
				t.Fatal(err)
			}
			addonController, ok := resources["Deployment/dynamic-scoring-addon-controller"]
			if !ok {
				t.Fatal("missing addon-controller Deployment")
			}
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal([]byte(addonController.Raw), &deployment); err != nil {
				t.Fatal(err)
			}
			var env map[string]string
			for _, container := range deployment.Spec.Template.Spec.Containers {
				if container.Name != "dynamic-scoring-addon-controller" {
					continue
				}
				env = make(map[string]string, len(container.Env))
				for _, variable := range container.Env {
					env[variable.Name] = variable.Value
				}
			}
			for name, want := range map[string]string{
				dynamic_scoring.AddonImageEnvVar:            tc.image,
				dynamic_scoring.AddonImagePullSecretsEnvVar: string(imagePullSecretsJSON),
				dynamic_scoring.AddonInstallNamespaceEnvVar: tc.spoke,
			} {
				if got := env[name]; got != want {
					t.Errorf("%s=%q; want %q", name, got, want)
				}
			}

			configSpec := fmt.Sprintf(`agentInstallNamespace: %s
customizedVariables:
  - name: Image
    value: %s
  - name: ImagePullSecrets
    value: '%s'
`, tc.spoke, tc.image, string(imagePullSecretsJSON))
			placementSpec := `clusterSets: [global]
tolerations:
  - key: cluster.open-cluster-management.io/unreachable
    operator: Equal
  - key: cluster.open-cluster-management.io/unavailable
    operator: Equal
predicates:
  - requiredClusterSelector:
      labelSelector:
        matchExpressions:
          - key: cluster-arch
            operator: %s
            values: [aarch64]
`
			for key, spec := range map[string]string{
				"AddOnDeploymentConfig/dynamic-scoring-addon-config":         configSpec,
				"AddOnDeploymentConfig/dynamic-scoring-addon-config-aarch64": configSpec,
				"Placement/dynamic-scoring-placement":                        fmt.Sprintf(placementSpec, "NotIn"),
				"Placement/dynamic-scoring-placement-aarch64":                fmt.Sprintf(placementSpec, "In"),
			} {
				got, ok := resources[key]
				if !ok {
					t.Fatalf("missing %s", key)
				}
				var want map[string]interface{}
				if err := yaml.Unmarshal([]byte(spec), &want); err != nil {
					t.Fatal(err)
				}
				if got.Metadata.Namespace != tc.hub || !reflect.DeepEqual(got.Spec, want) {
					t.Errorf("%s: namespace=%q spec=%v; want namespace=%q spec=%v",
						key, got.Metadata.Namespace, got.Spec, tc.hub, want)
				}
			}
			addon, ok := resources["ClusterManagementAddOn/dynamic-scoring-framework"]
			if !ok {
				t.Fatal("missing ClusterManagementAddOn")
			}
			var want map[string]interface{}
			if err := yaml.Unmarshal([]byte(fmt.Sprintf(`type: Placements
placements:
  - name: dynamic-scoring-placement
    namespace: %[1]s
    configs:
      - group: addon.open-cluster-management.io
        resource: addondeploymentconfigs
        name: dynamic-scoring-addon-config
        namespace: %[1]s
  - name: dynamic-scoring-placement-aarch64
    namespace: %[1]s
    configs:
      - group: addon.open-cluster-management.io
        resource: addondeploymentconfigs
        name: dynamic-scoring-addon-config-aarch64
        namespace: %[1]s
`, tc.hub)), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(addon.Spec["installStrategy"], want) {
				t.Errorf("installStrategy=%v; want %v", addon.Spec["installStrategy"], want)
			}
			if _, ok := addon.Spec["defaultConfigs"]; ok {
				t.Errorf("unexpected defaultConfigs: %v", addon.Spec["defaultConfigs"])
			}
		})
	}
}

func TestStaticAddonConfigs(t *testing.T) {
	const base = "../deploy/addon/dynamic-scoring/resources/"
	configs := make([]resource, 0, 2)
	for _, name := range []string{"dynamic-scoring-addon-config", "dynamic-scoring-addon-config-aarch64"} {
		output, err := os.ReadFile(base + strings.ReplaceAll(name, "-", "_") + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		config, ok := decodeResources(t, output)["AddOnDeploymentConfig/"+name]
		if !ok {
			t.Fatalf("missing AddOnDeploymentConfig/%s", name)
		}
		if got := config.APIVersion; got != addonAPIGroupVersion {
			t.Errorf("%s apiVersion=%q; want %s", name, got, addonAPIGroupVersion)
		}
		var image interface{}
		variables, _ := config.Spec["customizedVariables"].([]interface{})
		for _, variable := range variables {
			if v, ok := variable.(map[string]interface{}); ok && v["name"] == "Image" {
				image = v["value"]
			}
		}
		if want := "quay.io/open-cluster-management/dynamic-scoring-addon:latest"; image != want {
			t.Errorf("%s: Image=%v; want %q", name, image, want)
		}
		configs = append(configs, config)
	}
	if configs[0].Metadata.Namespace != configs[1].Metadata.Namespace ||
		!reflect.DeepEqual(configs[0].Spec, configs[1].Spec) {
		t.Fatalf("standard and ARM addon configs differ: %v != %v", configs[0], configs[1])
	}

	clusterManagementAddon, err := os.ReadFile(base + "clustermanagementaddon.yaml")
	if err != nil {
		t.Fatal(err)
	}
	addon, ok := decodeResources(t, clusterManagementAddon)["ClusterManagementAddOn/dynamic-scoring-framework"]
	if !ok {
		t.Fatal("missing static ClusterManagementAddOn")
	}
	if got := addon.APIVersion; got != addonAPIGroupVersion {
		t.Errorf("ClusterManagementAddOn apiVersion=%q; want %s", got, addonAPIGroupVersion)
	}
	if _, ok := addon.Spec["defaultConfigs"]; ok {
		t.Errorf("unexpected defaultConfigs: %v", addon.Spec["defaultConfigs"])
	}
}

func decodeResources(t *testing.T, output []byte) map[string]resource {
	t.Helper()
	resources := map[string]resource{}
	for _, document := range strings.Split(string(output), "\n---") {
		var r resource
		if err := yaml.Unmarshal([]byte(document), &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind == "" {
			continue
		}
		key := r.Kind + "/" + r.Metadata.Name
		if _, exists := resources[key]; exists {
			t.Fatalf("duplicate resource %s", key)
		}
		r.Raw = document
		resources[key] = r
	}
	return resources
}
