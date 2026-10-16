package dynamic_scoring

import (
	"context"
	"os"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	addonapiv1beta1 "open-cluster-management.io/api/addon/v1beta1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
)

func TestGetDefaultValues_DefaultImage(t *testing.T) {
	t.Setenv(AddonImageEnvVar, "")
	t.Setenv(AddonImagePullSecretsEnvVar, "")

	cluster := &clusterv1.ManagedCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a"}}
	addon := &addonapiv1beta1.ManagedClusterAddOn{ObjectMeta: metav1.ObjectMeta{Name: "dynamic-scoring"}}

	values, err := GetDefaultValues(cluster, addon)
	if err != nil {
		t.Fatalf("GetDefaultValues returned error: %v", err)
	}

	secret, ok := values["KubeConfigSecret"].(string)
	if !ok {
		t.Fatalf("expected KubeConfigSecret string, got %T", values["KubeConfigSecret"])
	}
	if secret != "dynamic-scoring-hub-kubeconfig" {
		t.Fatalf("unexpected KubeConfigSecret: %s", secret)
	}

	clusterName, ok := values["ClusterName"].(string)
	if !ok {
		t.Fatalf("expected ClusterName string, got %T", values["ClusterName"])
	}
	if clusterName != "cluster-a" {
		t.Fatalf("unexpected ClusterName: %s", clusterName)
	}

	image, ok := values["Image"].(string)
	if !ok {
		t.Fatalf("expected Image string, got %T", values["Image"])
	}
	if image != DefaultDynamicScoringAddonImage {
		t.Fatalf("unexpected Image: %s", image)
	}
}

func TestGetDefaultValues_EnvOverride(t *testing.T) {
	t.Setenv(AddonImageEnvVar, "example.com/custom:latest")
	t.Setenv(AddonImagePullSecretsEnvVar, "")

	cluster := &clusterv1.ManagedCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-b"}}
	addon := &addonapiv1beta1.ManagedClusterAddOn{ObjectMeta: metav1.ObjectMeta{Name: "addon-b"}}

	values, err := GetDefaultValues(cluster, addon)
	if err != nil {
		t.Fatalf("GetDefaultValues returned error: %v", err)
	}

	image, ok := values["Image"].(string)
	if !ok {
		t.Fatalf("expected Image string, got %T", values["Image"])
	}
	if image != "example.com/custom:latest" {
		t.Fatalf("unexpected Image: %s", image)
	}
}

func TestGetDefaultValues_ImagePullSecretsFromEnv(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "unset", raw: "", want: nil},
		{name: "one secret", raw: `["hub-registry-secret"]`, want: []string{"hub-registry-secret"}},
		{
			name: "multiple secrets",
			raw:  `["klusterlet-secret","dsf-secret"]`,
			want: []string{"klusterlet-secret", "dsf-secret"},
		},
		{name: "empty list", raw: `[]`, want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(AddonImagePullSecretsEnvVar, tt.raw)
			cluster := &clusterv1.ManagedCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a"}}
			addon := &addonapiv1beta1.ManagedClusterAddOn{ObjectMeta: metav1.ObjectMeta{Name: "dynamic-scoring"}}

			values, err := GetDefaultValues(cluster, addon)
			if err != nil {
				t.Fatalf("GetDefaultValues returned error: %v", err)
			}

			got, ok := values["ImagePullSecrets"].([]string)
			if !ok {
				t.Fatalf("expected ImagePullSecrets []string, got %T", values["ImagePullSecrets"])
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("unexpected ImagePullSecrets: got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestGetDefaultValues_InvalidImagePullSecretsEnv(t *testing.T) {
	t.Setenv(AddonImagePullSecretsEnvVar, `"hub-registry-secret"`)
	cluster := &clusterv1.ManagedCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a"}}
	addon := &addonapiv1beta1.ManagedClusterAddOn{ObjectMeta: metav1.ObjectMeta{Name: "dynamic-scoring"}}

	if _, err := GetDefaultValues(cluster, addon); err == nil {
		t.Fatal("expected an error for ImagePullSecrets that is not a JSON array")
	}
}

func TestAgentInstallNamespaceFromDeploymentConfigOrEnv(t *testing.T) {
	t.Run("deployment config overrides environment", func(t *testing.T) {
		t.Setenv(AddonInstallNamespaceEnvVar, "environment-namespace")
		addon := addonWithDeploymentConfig()
		getter := staticAddOnDeploymentConfigGetter{
			config: &addonapiv1beta1.AddOnDeploymentConfig{
				Spec: addonapiv1beta1.AddOnDeploymentConfigSpec{AgentInstallNamespace: "config-namespace"},
			},
		}

		got, err := AgentInstallNamespaceFromDeploymentConfigOrEnv(getter)(context.Background(), addon)
		if err != nil {
			t.Fatalf("namespace resolver returned error: %v", err)
		}
		if got != "config-namespace" {
			t.Fatalf("namespace=%q; want config-namespace", got)
		}
	})

	t.Run("API-defaulted deployment namespace overrides environment", func(t *testing.T) {
		t.Setenv(AddonInstallNamespaceEnvVar, "environment-namespace")
		addon := addonWithDeploymentConfig()
		getter := staticAddOnDeploymentConfigGetter{
			config: &addonapiv1beta1.AddOnDeploymentConfig{
				Spec: addonapiv1beta1.AddOnDeploymentConfigSpec{
					AgentInstallNamespace: addonfactory.AddonDefaultInstallNamespace,
				},
			},
		}

		got, err := AgentInstallNamespaceFromDeploymentConfigOrEnv(getter)(context.Background(), addon)
		if err != nil {
			t.Fatalf("namespace resolver returned error: %v", err)
		}
		if got != addonfactory.AddonDefaultInstallNamespace {
			t.Fatalf("namespace=%q; want %s", got, addonfactory.AddonDefaultInstallNamespace)
		}
	})

	t.Run("API-defaulted deployment namespace is honored without environment", func(t *testing.T) {
		t.Setenv(AddonInstallNamespaceEnvVar, "")
		addon := addonWithDeploymentConfig()
		getter := staticAddOnDeploymentConfigGetter{
			config: &addonapiv1beta1.AddOnDeploymentConfig{
				Spec: addonapiv1beta1.AddOnDeploymentConfigSpec{
					AgentInstallNamespace: addonfactory.AddonDefaultInstallNamespace,
				},
			},
		}

		got, err := AgentInstallNamespaceFromDeploymentConfigOrEnv(getter)(context.Background(), addon)
		if err != nil {
			t.Fatalf("namespace resolver returned error: %v", err)
		}
		if got != addonfactory.AddonDefaultInstallNamespace {
			t.Fatalf("namespace=%q; want %s", got, addonfactory.AddonDefaultInstallNamespace)
		}
	})

	t.Run("environment namespace is used when deployment config is absent", func(t *testing.T) {
		t.Setenv(AddonInstallNamespaceEnvVar, "environment-namespace")
		addon := &addonapiv1beta1.ManagedClusterAddOn{}

		got, err := AgentInstallNamespaceFromDeploymentConfigOrEnv(nil)(context.Background(), addon)
		if err != nil {
			t.Fatalf("namespace resolver returned error: %v", err)
		}
		if got != "environment-namespace" {
			t.Fatalf("namespace=%q; want environment-namespace", got)
		}
	})

	t.Run("environment can select addon framework default", func(t *testing.T) {
		t.Setenv(AddonInstallNamespaceEnvVar, "open-cluster-management-agent-addon")
		addon := &addonapiv1beta1.ManagedClusterAddOn{}

		got, err := AgentInstallNamespaceFromDeploymentConfigOrEnv(nil)(context.Background(), addon)
		if err != nil {
			t.Fatalf("namespace resolver returned error: %v", err)
		}
		if got != "open-cluster-management-agent-addon" {
			t.Fatalf("namespace=%q; want open-cluster-management-agent-addon", got)
		}
	})

	t.Run("DSF default is used when deployment config and environment are absent", func(t *testing.T) {
		t.Setenv(AddonInstallNamespaceEnvVar, "")
		addon := &addonapiv1beta1.ManagedClusterAddOn{}

		got, err := AgentInstallNamespaceFromDeploymentConfigOrEnv(nil)(context.Background(), addon)
		if err != nil {
			t.Fatalf("namespace resolver returned error: %v", err)
		}
		if got != DefaultInstallationNamespace {
			t.Fatalf("namespace=%q; want %s", got, DefaultInstallationNamespace)
		}
	})

	t.Run("environment namespace is trimmed", func(t *testing.T) {
		t.Setenv(AddonInstallNamespaceEnvVar, "  custom-agent  ")
		addon := &addonapiv1beta1.ManagedClusterAddOn{}

		got, err := AgentInstallNamespaceFromDeploymentConfigOrEnv(nil)(context.Background(), addon)
		if err != nil {
			t.Fatalf("namespace resolver returned error: %v", err)
		}
		if got != "custom-agent" {
			t.Fatalf("namespace=%q; want custom-agent", got)
		}
	})
}

func addonWithDeploymentConfig() *addonapiv1beta1.ManagedClusterAddOn {
	return &addonapiv1beta1.ManagedClusterAddOn{
		Status: addonapiv1beta1.ManagedClusterAddOnStatus{
			ConfigReferences: []addonapiv1beta1.ConfigReference{{
				ConfigGroupResource: addonapiv1beta1.ConfigGroupResource{
					Group:    "addon.open-cluster-management.io",
					Resource: "addondeploymentconfigs",
				},
				DesiredConfig: &addonapiv1beta1.ConfigSpecHash{
					ConfigReferent: addonapiv1beta1.ConfigReferent{Namespace: "hub", Name: "agent-config"},
					SpecHash:       "spec-hash",
				},
			}},
		},
	}
}

type staticAddOnDeploymentConfigGetter struct {
	config *addonapiv1beta1.AddOnDeploymentConfig
	err    error
}

func (g staticAddOnDeploymentConfigGetter) Get(
	_ context.Context, _, _ string,
) (*addonapiv1beta1.AddOnDeploymentConfig, error) {
	return g.config, g.err
}

func TestNewRegistrationOption(t *testing.T) {
	option := NewRegistrationOption(&rest.Config{}, "dynamic-scoring", "agent")
	if option == nil {
		t.Fatalf("expected non-nil registration option")
	}
	if option.Configurations == nil {
		t.Fatalf("expected Configurations to be set")
	}
	if option.CSRApproveCheck == nil {
		t.Fatalf("expected CSRApproveCheck to be set")
	}
	if option.PermissionConfig == nil {
		t.Fatalf("expected PermissionConfig to be set")
	}
}

func TestAgentHealthProber(t *testing.T) {
	prober := AgentHealthProber()
	if prober == nil {
		t.Fatalf("expected non-nil health prober")
	}
	if prober.Type != "DeploymentAvailability" {
		t.Fatalf("unexpected prober type: %s", prober.Type)
	}
}

func TestGetDefaultValues_DoesNotMutateEnv(t *testing.T) {
	t.Setenv(AddonImageEnvVar, "")
	t.Setenv(AddonImagePullSecretsEnvVar, "")
	imageBefore := os.Getenv(AddonImageEnvVar)
	pullSecretsBefore := os.Getenv(AddonImagePullSecretsEnvVar)
	cluster := &clusterv1.ManagedCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-c"}}
	addon := &addonapiv1beta1.ManagedClusterAddOn{ObjectMeta: metav1.ObjectMeta{Name: "addon-c"}}

	values, err := GetDefaultValues(cluster, addon)
	if err != nil {
		t.Fatalf("GetDefaultValues returned error: %v", err)
	}

	if _, ok := values["ImagePullSecrets"]; !ok {
		t.Fatal("expected ImagePullSecrets key to exist")
	}
	if got := os.Getenv(AddonImageEnvVar); got != imageBefore {
		t.Errorf("%s changed from %q to %q", AddonImageEnvVar, imageBefore, got)
	}
	if got := os.Getenv(AddonImagePullSecretsEnvVar); got != pullSecretsBefore {
		t.Errorf("%s changed from %q to %q", AddonImagePullSecretsEnvVar, pullSecretsBefore, got)
	}
}

func TestAgentNamespaceHasOCMAddonLabel(t *testing.T) {
	const installNamespace = "custom-agent"
	agentAddon, err := addonfactory.NewAgentAddonFactory(AddonName, FS, "manifests/templates").
		WithAgentInstallNamespace(func(context.Context, *addonapiv1beta1.ManagedClusterAddOn) (string, error) {
			return installNamespace, nil
		}).
		BuildTemplateAgentAddon()
	if err != nil {
		t.Fatalf("build template agent addon: %v", err)
	}

	manifests, err := agentAddon.Manifests(
		context.Background(),
		&clusterv1.ManagedCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a"}},
		&addonapiv1beta1.ManagedClusterAddOn{ObjectMeta: metav1.ObjectMeta{Name: AddonName}},
	)
	if err != nil {
		t.Fatalf("render agent manifests: %v", err)
	}

	for _, manifest := range manifests {
		namespace, ok := manifest.(*corev1.Namespace)
		if !ok || namespace.Name != installNamespace {
			continue
		}
		if got := namespace.Labels["addon.open-cluster-management.io/namespace"]; got != "true" {
			t.Fatalf("addon namespace label=%q; want %q", got, "true")
		}
		return
	}
	t.Fatalf("rendered Namespace %q not found", installNamespace)
}
