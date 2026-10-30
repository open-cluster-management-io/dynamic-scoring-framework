# Install Dynamic Scoring Framework on OpenShift Container Platform

## Prepare Image

### Prerequisites

Ensure that the OpenShift Internal Registry is enabled and accessible. For more information, refer to the official OpenShift documentation:

https://docs.redhat.com/en/documentation/openshift_container_platform/4.18/html/registry/securing-exposing-registry#securing-exposing-registry

### Push Image to OpenShift Internal Registry

The published controller and addon tags are multi-architecture manifests for
`linux/amd64` and `linux/arm64`. Version tags are published by the release workflow;
`latest` is published only from pushes to `main`. Use `skopeo copy --all` to preserve
both architectures when copying these images to another registry (requires `skopeo`).

```bash
# after login to OpenShift hub cluster
HOST=$(oc get route default-route -n openshift-image-registry --template='{{ .spec.host }}')
podman login -u ai-ran-admin -p $(oc whoami -t) $HOST
IMG_CONTROLLER=$HOST/dynamic-scoring/dynamic-scoring-controller:latest
IMG_ADDON=$HOST/open-cluster-management/dynamic-scoring-addon:latest
skopeo copy --all docker://quay.io/open-cluster-management/dynamic-scoring-controller:latest docker://$IMG_CONTROLLER
skopeo copy --all docker://quay.io/open-cluster-management/dynamic-scoring-addon:latest docker://$IMG_ADDON
```

### (Optional) Build and Push Local Changes for Both Architectures

With Docker/BuildKit and registry push credentials, build architecture-tagged
images first, then publish the common manifests. Use the registry image names set
above, and ensure the destination repositories/ImageStreams exist.

```bash
oc whoami -t | docker login "$HOST" --username ai-ran-admin --password-stdin
for ARCH in amd64 arm64; do
  make image image-push \
    IMG_CONTROLLER="${IMG_CONTROLLER}-${ARCH}" \
    IMG_ADDON="${IMG_ADDON}-${ARCH}" \
    IMAGE_BUILD_EXTRA_FLAGS="--platform linux/${ARCH} --build-arg OS=linux --build-arg ARCH=${ARCH}"
done
make image-manifest IMG_CONTROLLER="$IMG_CONTROLLER" IMG_ADDON="$IMG_ADDON"
```

The addon image is a multi-architecture manifest, so the same image works on
amd64 and arm64 managed clusters. DSF uses one architecture-neutral Placement;
separate ARM addon builds from an external cluster are not needed. Sample
scoring API images, including policy-watcher, are not published by the DSF image
workflows.

## Deploy Dynamic Scoring Framework

```bash
# after login to OpenShift cluster
make deploy IMG_CONTROLLER=$IMG_CONTROLLER
make deploy-addon IMG_ADDON=$IMG_ADDON
```

## (Optional) Configure Agent image pull from Hub registry

If you push the addon image to the hub cluster's internal registry (e.g. `$HOST/open-cluster-management/dynamic-scoring-addon:latest`), the managed clusters also need credentials to pull it.

The instructions below use the standalone `make deploy-addon` Kustomize manifests, not the Helm chart. By default the controller uses both `open-cluster-management-image-pull-credentials` and `hub-registry-secret`; no `AddOnDeploymentConfig` is created or referenced. To override these defaults, create an `AddOnDeploymentConfig` and add a config reference to the `global` placement in the `dynamic-scoring-framework` `ClusterManagementAddOn`:

```yaml
configs:
  - group: addon.open-cluster-management.io
    resource: addondeploymentconfigs
    name: dynamic-scoring-addon-config
    namespace: open-cluster-management
```

The agent pod's `ImagePullSecrets` must reference a Secret in its install namespace. Choose either the OCM-provided pull credentials or the DSF-specific hub-secret copy below.

### Option 1: Reuse credentials supplied to `clusteradm join`

If each managed cluster was joined with `clusteradm join --image-pull-credential-file=/path/to/docker-config.json`, and that Docker config contains credentials for the DSF image registry, OCM creates `open-cluster-management-image-pull-credentials` in the klusterlet operator namespace on the managed cluster. DSF labels its agent namespace with `addon.open-cluster-management.io/namespace: "true"`, so the klusterlet operator copies this Secret into the agent namespace. You do not need to create the DSF-specific hub Secret in Option 2.

Create the `AddOnDeploymentConfig` referenced by the CMA with this customized variable:

```yaml
apiVersion: addon.open-cluster-management.io/v1beta1
kind: AddOnDeploymentConfig
metadata:
  name: dynamic-scoring-addon-config
  namespace: open-cluster-management
spec:
  customizedVariables:
    - name: ImagePullSecrets
      value: '["open-cluster-management-image-pull-credentials"]'
```

The credential file must authorize pulls from the registry where the DSF agent image is stored. If it is missing or empty, the OCM Secret may not contain credentials that can pull a private image.

### Option 2: Copy a DSF-specific pull Secret from the hub

Use this option when the klusterlet credentials are unavailable or do not have access to the DSF image registry. DSF copies `dynamic-scoring-addon-pull-secret` from `open-cluster-management` on the hub to `hub-registry-secret` in the agent install namespace.

#### 1) Create the DSF pull Secret on the hub cluster

Following are the steps to create the pull secret on hub cluster.

```bash
SERVICEACCOUNT=dynamic-scoring-image-puller
oc create sa $SERVICEACCOUNT -n open-cluster-management
oc policy add-role-to-user system:image-puller -z $SERVICEACCOUNT -n open-cluster-management
TOKEN=$(oc create token $SERVICEACCOUNT -n open-cluster-management)
REGISTRY=default-route-openshift-image-registry.apps.hubdev01.airan.localdomain
AUTH=$(echo -n "$SERVICEACCOUNT:$TOKEN" | base64 -w0)

cat <<EOF > ./secrets/pull-dockerconfig.json
{
  "auths": {
    "$REGISTRY": {
      "auth": "$AUTH"
    }
  }
}
EOF

oc create secret generic dynamic-scoring-addon-pull-secret \
  --from-file=.dockerconfigjson=./secrets/pull-dockerconfig.json \
  --type=kubernetes.io/dockerconfigjson -n open-cluster-management
```

#### 2) Create an `AddOnDeploymentConfig` to use the DSF pull Secret

Create the `AddOnDeploymentConfig` in namespace `open-cluster-management`, using
the desired image and pull-secret values, and add the config reference shown
above to the CMA's `global` placement.

```yaml
apiVersion: addon.open-cluster-management.io/v1beta1
kind: AddOnDeploymentConfig
metadata:
	name: dynamic-scoring-addon-config
	namespace: open-cluster-management
spec:
	agentInstallNamespace: dynamic-scoring
	customizedVariables:
		# override agent image
		- name: Image
			value: default-route-openshift-image-registry.apps.hubdev01.airan.localdomain/open-cluster-management/dynamic-scoring-addon:latest

		# set imagePullSecrets for managed cluster agent
		- name: ImagePullSecrets
			value: '["hub-registry-secret"]'
```

Note: `ImagePullSecrets` is interpreted as an array of secret names.

oc create secret generic policy-watcher-pull-secret \
  --from-file=.dockerconfigjson=./secrets/pull-dockerconfig.json \
  --type=kubernetes.io/dockerconfigjson -n dynamic-scoring
