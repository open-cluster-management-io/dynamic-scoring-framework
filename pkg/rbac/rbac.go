package rbac

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"open-cluster-management.io/addon-framework/pkg/agent"
	addonapiv1beta1 "open-cluster-management.io/api/addon/v1beta1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
)

func AddonRBAC(kubeConfig *rest.Config) agent.PermissionConfigFunc {
	return func(ctx context.Context, cluster *clusterv1.ManagedCluster, addon *addonapiv1beta1.ManagedClusterAddOn) error {
		kubeclient, err := kubernetes.NewForConfig(kubeConfig)
		if err != nil {
			return err
		}

		groups := agent.DefaultGroups(cluster.Name, addon.Name)

		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("open-cluster-management:%s:agent", addon.Name),
				Namespace: cluster.Name,
			},
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"get", "list", "watch"},
					Resources: []string{"configmaps"},
					APIGroups: []string{""},
				},
				{
					Verbs:     []string{"get", "list", "watch"},
					Resources: []string{"managedclusteraddons"},
					APIGroups: []string{"addon.open-cluster-management.io"},
				},
				{
					Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
					Resources: []string{"addonplacementscores"},
					APIGroups: []string{"cluster.open-cluster-management.io"},
				},
				{
					Verbs:     []string{"update", "patch"},
					Resources: []string{"addonplacementscores/status"},
					APIGroups: []string{"cluster.open-cluster-management.io"},
				},
			},
		}

		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("open-cluster-management:%s:agent", addon.Name),
				Namespace: cluster.Name,
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     fmt.Sprintf("open-cluster-management:%s:agent", addon.Name),
			},
			Subjects: []rbacv1.Subject{
				{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: groups[0]},
			},
		}

		klog.Info("Role and RoleBinding for AddOn ", addon.Name, " on Cluster ", cluster.Name)

		_, err = kubeclient.RbacV1().Roles(cluster.Name).Get(ctx, role.Name, metav1.GetOptions{})
		switch {
		case errors.IsNotFound(err):
			klog.Info("Creating Role ", role.Name, " on Cluster ", cluster.Name, err)
			_, createErr := kubeclient.RbacV1().Roles(cluster.Name).Create(ctx, role, metav1.CreateOptions{})
			if createErr != nil {
				return createErr
			}
		case err != nil:
			klog.Info("Other Err Role ", role.Name, " on Cluster ", cluster.Name, err)
			return err
		}

		_, err = kubeclient.RbacV1().RoleBindings(cluster.Name).Get(ctx, binding.Name, metav1.GetOptions{})
		switch {
		case errors.IsNotFound(err):
			_, createErr := kubeclient.RbacV1().RoleBindings(cluster.Name).Create(
				ctx,
				binding,
				metav1.CreateOptions{},
			)
			if createErr != nil {
				return createErr
			}
		case err != nil:
			return err
		}

		return nil
	}
}
