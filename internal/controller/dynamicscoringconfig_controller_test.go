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

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dynamicscoringv1alpha1 "open-cluster-management.io/dynamic-scoring/api/v1alpha1"
	"open-cluster-management.io/dynamic-scoring/pkg/common"
)

var _ = Describe("DynamicScoringConfig helpers", func() {
	Context("namespace scope", func() {
		It("ignores DynamicScoringConfigs outside the controller namespace", func(ctx SpecContext) {
			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			Expect(dynamicscoringv1alpha1.AddToScheme(scheme)).To(Succeed())

			config := &dynamicscoringv1alpha1.DynamicScoringConfig{
				ObjectMeta: metav1.ObjectMeta{Name: common.DynamicScoringConfigName, Namespace: "other-namespace"},
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()
			reconciler := &DynamicScoringConfigReconciler{Client: kubeClient, Namespace: "controller-namespace"}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
				Name: config.Name, Namespace: config.Namespace,
			}})
			Expect(err).NotTo(HaveOccurred())

			actual := &dynamicscoringv1alpha1.DynamicScoringConfig{}
			Expect(kubeClient.Get(ctx, types.NamespacedName{Name: config.Name, Namespace: config.Namespace}, actual)).To(Succeed())
			Expect(actual.Finalizers).To(BeEmpty())
		})
	})

	Context("mask helpers", func() {
		It("builds and checks mask map", func() {
			maskMap := buildMaskMap([]common.Mask{{ClusterName: "cluster-a", ScoreName: "latency"}})
			Expect(isMasked(maskMap, "cluster-a", "latency")).To(BeTrue())
			Expect(isMasked(maskMap, "cluster-a", "power")).To(BeFalse())
		})
	})

	Context("URL helpers", func() {
		It("validates URLs", func() {
			Expect(isValidURL("http://example.com/path")).To(BeTrue())
			Expect(isValidURL("://bad-url")).To(BeFalse())
		})
	})

	Context("manifest work builder", func() {
		It("embeds summaries in ConfigMap", func() {
			summaries := []common.ScorerSummary{{Name: "scorer-a", ScoreName: "latency"}}
			manifest := buildConfigManifestWork("cluster-a", summaries)
			Expect(manifest.Name).To(Equal(common.ManifestWorkConfigMapName))
			Expect(manifest.Namespace).To(Equal("cluster-a"))
			Expect(manifest.Spec.Workload.Manifests).To(HaveLen(1))

			cm, ok := manifest.Spec.Workload.Manifests[0].Object.(*corev1.ConfigMap)
			Expect(ok).To(BeTrue())
			Expect(cm.Name).To(Equal(common.DynamicScoringConfigName))
			Expect(cm.Namespace).To(Equal(common.DynamicScoringNamespace))
			Expect(cm.Data).To(HaveKey("summaries"))
		})
	})

	Context("endpoint helpers", func() {
		It("builds source endpoint from config URL", func() {
			scorer := dynamicscoringv1alpha1.DynamicScorer{
				Spec: dynamicscoringv1alpha1.DynamicScorerSpec{
					ConfigURL: "http://config.example.com/config",
					Source: dynamicscoringv1alpha1.SourceConfigWithAuth{
						Path: "/api/v1/query_range",
					},
				},
			}
			endpoint, err := getValidSourceFullEndpoint(scorer)
			Expect(err).NotTo(HaveOccurred())
			Expect(endpoint).To(Equal("http://config.example.com/api/v1/query_range"))
		})

		It("builds scoring endpoint from scoring host", func() {
			scorer := dynamicscoringv1alpha1.DynamicScorer{
				Spec: dynamicscoringv1alpha1.DynamicScorerSpec{
					ConfigURL: "http://config.example.com/config",
					Scoring: dynamicscoringv1alpha1.ScoringConfigWithAuth{
						Host: "http://score.example.com",
						Path: "/api/v1/score",
					},
				},
			}
			endpoint, err := getValidScoringFullEndpoint(scorer)
			Expect(err).NotTo(HaveOccurred())
			Expect(endpoint).To(Equal("http://score.example.com/api/v1/score"))
		})

		It("builds source and scoring endpoints from synced config when source and scoring hosts differ", func() {
			scorer := dynamicscoringv1alpha1.DynamicScorer{
				Spec: dynamicscoringv1alpha1.DynamicScorerSpec{
					ConfigURL: "http://config.example.com/config",
				},
				Status: dynamicscoringv1alpha1.DynamicScorerStatus{
					LastSyncedConfig: &common.Config{
						Source: common.SourceConfig{
							Host: "http://prometheus.monitoring.svc:9090",
							Path: "/api/v1/query_range",
						},
						Scoring: common.ScoringConfig{
							Host: "http://scorer-service.scoring.svc:8080",
							Path: "/api/v1/score",
						},
					},
				},
			}
			sourceEndpoint, err := getValidSourceFullEndpoint(scorer)
			Expect(err).NotTo(HaveOccurred())
			Expect(sourceEndpoint).To(Equal("http://prometheus.monitoring.svc:9090/api/v1/query_range"))

			scoringEndpoint, err := getValidScoringFullEndpoint(scorer)
			Expect(err).NotTo(HaveOccurred())
			Expect(scoringEndpoint).To(Equal("http://scorer-service.scoring.svc:8080/api/v1/score"))
		})
	})

	Context("dimension format helper", func() {
		It("defaults when format is empty", func() {
			scorer := dynamicscoringv1alpha1.DynamicScorer{}
			format := getValidScoreDimentionFormat(scorer)
			Expect(format).To(Equal("${scoreName}"))
		})
	})
})
