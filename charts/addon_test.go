package charts

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type resource struct {
	Kind     string
	Metadata struct{ Name, Namespace string }
	Spec     map[string]interface{}
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
		values                  []string
	}{
		{
			"defaults", "quay.io/open-cluster-management/dynamic-scoring-addon:latest",
			"open-cluster-management", "dynamic-scoring", nil,
		},
		{"custom", "registry.example.com/team/agent:v1.2.3", "custom-hub", "custom-spoke", []string{
			"--set-string", "spokeSetting.agent.image.name=registry.example.com/team/agent",
			"--set-string", "spokeSetting.agent.image.tag=v1.2.3",
			"--set-string", "hubSetting.namespace=custom-hub",
			"--set-string", "spokeSetting.namespace=custom-spoke",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"template", "test", chart}, tc.values...)
			output, err := exec.Command(helm, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}
			resources := decodeResources(t, output)
			configSpec := fmt.Sprintf(`agentInstallNamespace: %s
customizedVariables:
  - name: Image
    value: %s
  - name: ImagePullSecrets
    value: '["hub-registry-secret"]'
`, tc.spoke, tc.image)
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
			supportedConfigs := []interface{}{map[string]interface{}{
				"group": "addon.open-cluster-management.io", "resource": "addondeploymentconfigs",
			}}
			if !reflect.DeepEqual(addon.Spec["supportedConfigs"], supportedConfigs) {
				t.Errorf("supportedConfigs=%v; want %v", addon.Spec["supportedConfigs"], supportedConfigs)
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
		resources[key] = r
	}
	return resources
}
