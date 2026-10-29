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
	rbacv1 "k8s.io/api/rbac/v1"
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
			addon, ok := resources["ClusterManagementAddOn/dynamic-scoring-framework"]
			if !ok {
				t.Fatal("missing ClusterManagementAddOn")
			}
			if got := addon.APIVersion; got != addonAPIGroupVersion {
				t.Errorf("ClusterManagementAddOn apiVersion=%q; want %s", got, addonAPIGroupVersion)
			}
			for key := range resources {
				if strings.HasPrefix(key, "AddOnDeploymentConfig/") {
					t.Errorf("unexpected generated %s", key)
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

			placementSpec := `clusterSets: [global]
tolerations:
  - key: cluster.open-cluster-management.io/unreachable
    operator: Equal
  - key: cluster.open-cluster-management.io/unavailable
    operator: Equal
`
			for key, spec := range map[string]string{
				"Placement/dynamic-scoring-placement": placementSpec,
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
			var want map[string]interface{}
			if err := yaml.Unmarshal([]byte(fmt.Sprintf(`type: Placements
placements:
  - name: dynamic-scoring-placement
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

func TestStaticAddonManifests(t *testing.T) {
	output, err := exec.Command("kustomize", "build", "../deploy/addon/dynamic-scoring").CombinedOutput()
	if err != nil {
		t.Fatalf("kustomize build: %v\n%s", err, output)
	}
	resources := decodeResources(t, output)
	for key := range resources {
		if strings.HasPrefix(key, "AddOnDeploymentConfig/") || strings.Contains(key, "aarch64") {
			t.Errorf("unexpected generated architecture-specific or AddOnDeploymentConfig resource %s", key)
		}
	}

	controller, ok := resources["Deployment/dynamic-scoring-addon-controller"]
	if !ok {
		t.Fatal("missing static addon-controller Deployment")
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal([]byte(controller.Raw), &deployment); err != nil {
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
		dynamic_scoring.AddonImageEnvVar:            "quay.io/open-cluster-management/dynamic-scoring-addon:latest",
		dynamic_scoring.AddonImagePullSecretsEnvVar: `["open-cluster-management-image-pull-credentials","hub-registry-secret"]`,
		dynamic_scoring.AddonInstallNamespaceEnvVar: "dynamic-scoring",
	} {
		if got := env[name]; got != want {
			t.Errorf("%s=%q; want %q", name, got, want)
		}
	}

	addon, ok := resources["ClusterManagementAddOn/dynamic-scoring-framework"]
	if !ok {
		t.Fatal("missing static ClusterManagementAddOn")
	}
	if got := addon.APIVersion; got != addonAPIGroupVersion {
		t.Errorf("ClusterManagementAddOn apiVersion=%q; want %s", got, addonAPIGroupVersion)
	}
	if _, ok := addon.Spec["defaultConfigs"]; ok {
		t.Errorf("unexpected defaultConfigs: %v", addon.Spec["defaultConfigs"])
	}
	strategy, ok := addon.Spec["installStrategy"].(map[string]interface{})
	if !ok {
		t.Fatal("missing ClusterManagementAddOn installStrategy")
	}
	placements, ok := strategy["placements"].([]interface{})
	if !ok || len(placements) != 1 {
		t.Fatalf("installStrategy placements=%v; want exactly one", strategy["placements"])
	}
	placement, ok := placements[0].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected placement value %T", placements[0])
	}
	if _, ok := placement["configs"]; ok {
		t.Errorf("unexpected AddOnDeploymentConfig references in placement: %v", placement["configs"])
	}
	if _, ok := resources["Placement/global"]; !ok {
		t.Fatal("missing architecture-neutral Placement/global")
	}
	role, ok := resources["ClusterRole/dynamic-scoring-addon"]
	if !ok {
		t.Fatal("missing ClusterRole with AddOnDeploymentConfig read permissions")
	}
	var clusterRole rbacv1.ClusterRole
	if err := yaml.Unmarshal([]byte(role.Raw), &clusterRole); err != nil {
		t.Fatal(err)
	}
	hasConfigReadPermissions := false
	for _, rule := range clusterRole.Rules {
		if contains(rule.APIGroups, addonAPIGroupVersion[:strings.LastIndex(addonAPIGroupVersion, "/")]) &&
			contains(rule.Resources, "addondeploymentconfigs") &&
			contains(rule.Verbs, "get") && contains(rule.Verbs, "list") && contains(rule.Verbs, "watch") {
			hasConfigReadPermissions = true
			break
		}
	}
	if !hasConfigReadPermissions {
		t.Error("ClusterRole does not grant get/list/watch on AddOnDeploymentConfigs")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
