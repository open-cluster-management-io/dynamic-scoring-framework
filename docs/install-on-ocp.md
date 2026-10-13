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

Both `dynamic-scoring-addon-config` and `dynamic-scoring-addon-config-aarch64`
must use the same common addon image, without an architecture suffix. Their
existing Placement selectors and config references remain unchanged. Separate
ARM addon builds from an external cluster are no longer needed. Sample scoring
API images, including policy-watcher, are not published by the DSF image workflows.

## Deploy Dynamic Scoring Framework

```bash
# after login to OpenShift cluster
make deploy IMG_CONTROLLER=$IMG_CONTROLLER
make deploy-addon IMG_ADDON=$IMG_ADDON
```

## (Optional) Configure Agent image pull from Hub registry

If you push the addon image to the hub cluster's internal registry (e.g. `$HOST/open-cluster-management/dynamic-scoring-addon:latest`), the managed clusters also need credentials to pull it.

As default, this framework copy pull secret named `dynamic-scoring-addon-pull-secret` from namespace `open-cluster-management` on hub cluster to `hub-registry-secret` the agent install namespace (e.g. `dynamic-scoring`).

### 1) Create pull secret on Hub cluster

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

### 2) Update `AddOnDeploymentConfig` to use the secret

Edit both `AddOnDeploymentConfig` objects referenced by `ClusterManagementAddOn`
(`dynamic-scoring-addon-config` and `dynamic-scoring-addon-config-aarch64` in
namespace `open-cluster-management`), using the same image and pull secret values.

```yaml
apiVersion: addon.open-cluster-management.io/v1alpha1
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
