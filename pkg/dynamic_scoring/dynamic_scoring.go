package dynamic_scoring

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/rest"
	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	"open-cluster-management.io/addon-framework/pkg/agent"
	"open-cluster-management.io/addon-framework/pkg/utils"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"open-cluster-management.io/dynamic-scoring/pkg/rbac"
)

const (
	DefaultDynamicScoringAddonImage = "quay.io/open-cluster-management/dynamic-scoring-addon:latest"
	AddonName                       = "dynamic-scoring-framework"
	DefaultInstallationNamespace    = "dynamic-scoring"

	// AddonImageEnvVar names the environment variable that sets the default addon image.
	AddonImageEnvVar = "DYNAMIC_SCORING_ADDON_IMAGE"
	// AddonImagePullSecretsEnvVar names the environment variable that sets default image pull secrets.
	AddonImagePullSecretsEnvVar = "DYNAMIC_SCORING_ADDON_IMAGE_PULL_SECRETS"
	// AddonInstallNamespaceEnvVar names the environment variable that sets the default agent namespace.
	AddonInstallNamespaceEnvVar = "DYNAMIC_SCORING_ADDON_NAMESPACE"
)

//go:embed manifests
//go:embed manifests/templates
var FS embed.FS

func NewRegistrationOption(kubeConfig *rest.Config, addonName, agentName string) *agent.RegistrationOption {
	return &agent.RegistrationOption{
		CSRConfigurations: agent.KubeClientSignerConfigurations(addonName, agentName),
		CSRApproveCheck:   utils.DefaultCSRApprover(agentName),
		PermissionConfig:  rbac.AddonRBAC(kubeConfig),
	}
}

func GetDefaultValues(cluster *clusterv1.ManagedCluster,
	addon *addonapiv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {

	image := os.Getenv(AddonImageEnvVar)
	if len(image) == 0 {
		image = DefaultDynamicScoringAddonImage
	}
	pullSecrets, err := imagePullSecretsFromEnv()
	if err != nil {
		return nil, err
	}

	manifestConfig := struct {
		KubeConfigSecret string
		ClusterName      string
		Image            string
		ImagePullSecrets []string
	}{
		KubeConfigSecret: fmt.Sprintf("%s-hub-kubeconfig", addon.Name),
		ClusterName:      cluster.Name,
		Image:            image,
		ImagePullSecrets: pullSecrets,
	}

	return addonfactory.StructToValues(manifestConfig), nil
}

func imagePullSecretsFromEnv() ([]string, error) {
	raw := strings.TrimSpace(os.Getenv(AddonImagePullSecretsEnvVar))
	if raw == "" {
		return nil, nil
	}

	var pullSecrets []string
	if err := json.Unmarshal([]byte(raw), &pullSecrets); err != nil {
		return nil, fmt.Errorf("%s must be a JSON array of secret names: %w", AddonImagePullSecretsEnvVar, err)
	}
	if pullSecrets == nil {
		return nil, fmt.Errorf("%s must be a JSON array of secret names", AddonImagePullSecretsEnvVar)
	}
	for i, name := range pullSecrets {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%s contains an empty secret name at index %d", AddonImagePullSecretsEnvVar, i)
		}
	}

	return pullSecrets, nil
}

// AgentInstallNamespaceFromDeploymentConfigOrEnv resolves the namespace used to install the agent.
// A non-empty AddOnDeploymentConfig namespace takes precedence over the environment value. The
// deprecated ManagedClusterAddOn installNamespace field is deliberately ignored.
func AgentInstallNamespaceFromDeploymentConfigOrEnv(
	getter utils.AddOnDeploymentConfigGetter,
) func(*addonapiv1alpha1.ManagedClusterAddOn) (string, error) {
	fromDeploymentConfig := utils.AgentInstallNamespaceFromDeploymentConfigFunc(getter)
	return func(addon *addonapiv1alpha1.ManagedClusterAddOn) (string, error) {
		namespace, err := fromDeploymentConfig(addon)
		if err != nil {
			return "", err
		}
		if namespace != "" {
			return namespace, nil
		}

		// If not set in addonDeploymentConfig, checck env var
		// and then fallback to default
		envNamespace := strings.TrimSpace(os.Getenv(AddonInstallNamespaceEnvVar))
		if envNamespace != "" {
			return envNamespace, nil
		}
		return DefaultInstallationNamespace, nil
	}
}

func AgentHealthProber() *agent.HealthProber {
	return &agent.HealthProber{
		Type: agent.HealthProberTypeDeploymentAvailability,
	}
}
