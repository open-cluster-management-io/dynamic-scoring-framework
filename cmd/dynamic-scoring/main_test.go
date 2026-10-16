package main

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"

	"open-cluster-management.io/dynamic-scoring/pkg/dynamic_scoring"
)

func TestAddOnDeploymentConfigOverridesEnvironmentDefaults(t *testing.T) {
	t.Setenv(dynamic_scoring.AddonImageEnvVar, "registry.example.com/env/agent:latest")
	t.Setenv(dynamic_scoring.AddonImagePullSecretsEnvVar, `["env-pull-secret"]`)

	cluster := &clusterv1.ManagedCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a"}}
	addon := &addonapiv1alpha1.ManagedClusterAddOn{ObjectMeta: metav1.ObjectMeta{Name: "dynamic-scoring"}}
	defaults, err := dynamic_scoring.GetDefaultValues(cluster, addon)
	if err != nil {
		t.Fatalf("get environment defaults: %v", err)
	}

	config := addonapiv1alpha1.AddOnDeploymentConfig{
		Spec: addonapiv1alpha1.AddOnDeploymentConfigSpec{
			CustomizedVariables: []addonapiv1alpha1.CustomizedVariable{
				{Name: "Image", Value: "registry.example.com/config/agent:v1"},
				{Name: "ImagePullSecrets", Value: `["config-pull-secret"]`},
			},
		},
	}
	configValues, err := normalizeAddOnDeploymentConfigValuesFunc()(config)
	if err != nil {
		t.Fatalf("normalize AddOnDeploymentConfig values: %v", err)
	}
	values := addonfactory.MergeValues(defaults, configValues)

	if got, want := values["Image"], "registry.example.com/config/agent:v1"; got != want {
		t.Errorf("Image=%v; want %v", got, want)
	}
	if got, want := values["ImagePullSecrets"], []string{"config-pull-secret"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ImagePullSecrets=%v; want %v", got, want)
	}
}

func TestAddOnDeploymentConfigWithoutPullSecretsKeepsEnvironmentDefault(t *testing.T) {
	t.Setenv(dynamic_scoring.AddonImageEnvVar, "registry.example.com/env/agent:latest")
	t.Setenv(dynamic_scoring.AddonImagePullSecretsEnvVar, `["env-pull-secret"]`)

	cluster := &clusterv1.ManagedCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a"}}
	addon := &addonapiv1alpha1.ManagedClusterAddOn{ObjectMeta: metav1.ObjectMeta{Name: "dynamic-scoring"}}
	defaults, err := dynamic_scoring.GetDefaultValues(cluster, addon)
	if err != nil {
		t.Fatalf("get environment defaults: %v", err)
	}

	config := addonapiv1alpha1.AddOnDeploymentConfig{
		Spec: addonapiv1alpha1.AddOnDeploymentConfigSpec{
			CustomizedVariables: []addonapiv1alpha1.CustomizedVariable{
				{Name: "Image", Value: "registry.example.com/config/agent:v1"},
			},
		},
	}
	configValues, err := normalizeAddOnDeploymentConfigValuesFunc()(config)
	if err != nil {
		t.Fatalf("normalize AddOnDeploymentConfig values: %v", err)
	}
	values := addonfactory.MergeValues(defaults, configValues)

	if got, want := values["ImagePullSecrets"], []string{"env-pull-secret"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ImagePullSecrets=%v; want %v", got, want)
	}
}
