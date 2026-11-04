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
	"encoding/json"
	"net/http"
	"net/http/httptest"

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

var _ = Describe("DynamicScorer helpers", func() {
	Context("namespace scope", func() {
		It("ignores DynamicScorers outside the controller namespace", func(ctx SpecContext) {
			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			Expect(dynamicscoringv1alpha1.AddToScheme(scheme)).To(Succeed())

			scorer := &dynamicscoringv1alpha1.DynamicScorer{
				ObjectMeta: metav1.ObjectMeta{Name: "foreign-scorer", Namespace: "other-namespace"},
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scorer).Build()
			reconciler := &DynamicScorerReconciler{Client: kubeClient, Namespace: "controller-namespace"}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
				Name: scorer.Name, Namespace: scorer.Namespace,
			}})
			Expect(err).NotTo(HaveOccurred())

			actual := &dynamicscoringv1alpha1.DynamicScorer{}
			Expect(kubeClient.Get(ctx, types.NamespacedName{Name: scorer.Name, Namespace: scorer.Namespace}, actual)).To(Succeed())
			Expect(actual.Status.HealthStatus).To(BeEmpty())
		})

		It("lists scorers only from the controller namespace for health checks", func(ctx SpecContext) {
			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			Expect(dynamicscoringv1alpha1.AddToScheme(scheme)).To(Succeed())

			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				&dynamicscoringv1alpha1.DynamicScorer{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "controller-namespace"}},
				&dynamicscoringv1alpha1.DynamicScorer{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "other-namespace"}},
			).Build()

			scorers, err := listDynamicScorers(ctx, kubeClient, "controller-namespace")
			Expect(err).NotTo(HaveOccurred())
			Expect(scorers.Items).To(HaveLen(1))
			Expect(scorers.Items[0].Name).To(Equal("local"))
		})
	})

	Context("syncScoringHealthz", func() {
		It("marks scorer active on 200", func(ctx SpecContext) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()

			scorer := &dynamicscoringv1alpha1.DynamicScorer{
				Spec: dynamicscoringv1alpha1.DynamicScorerSpec{
					ConfigURL: server.URL + "/config",
				},
			}

			err := syncScoringHealthz(ctx, scorer)
			Expect(err).NotTo(HaveOccurred())
			Expect(scorer.Status.HealthStatus).To(Equal(common.ScorerHealthStatusActive))
		})

		It("marks scorer inactive on non-200", func(ctx SpecContext) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()

			scorer := &dynamicscoringv1alpha1.DynamicScorer{
				Spec: dynamicscoringv1alpha1.DynamicScorerSpec{
					ConfigURL: server.URL + "/config",
				},
			}

			err := syncScoringHealthz(ctx, scorer)
			Expect(err).NotTo(HaveOccurred())
			Expect(scorer.Status.HealthStatus).To(Equal(common.ScorerHealthStatusInactive))
		})
	})

	Context("syncScoringConfig", func() {
		It("updates LastSyncedConfig from endpoint", func(ctx SpecContext) {
			expected := common.Config{
				Name:        "sample-score",
				Description: "sample",
				Source: common.SourceConfig{
					Type: common.SourceTypePrometheus,
					Host: "http://prometheus.example.com",
					Path: "/api/v1/query_range",
					Params: common.SourceParams{
						Query: "up",
						Range: 60,
						Step:  5,
					},
				},
				Scoring: common.ScoringConfig{
					Host: "http://scoring.example.com",
					Path: "/api/v1/score",
					Params: common.ScoringParams{
						Name:     "sample-score",
						Interval: 30,
					},
				},
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/config" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(expected); err != nil {
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer server.Close()

			scorer := &dynamicscoringv1alpha1.DynamicScorer{
				Spec: dynamicscoringv1alpha1.DynamicScorerSpec{
					ConfigURL: server.URL + "/config",
				},
			}

			err := syncScoringConfig(ctx, scorer)
			Expect(err).NotTo(HaveOccurred())
			Expect(scorer.Status.HealthStatus).To(Equal(common.ScorerHealthStatusActive))
			Expect(scorer.Status.LastSyncedConfig).NotTo(BeNil())
			Expect(*scorer.Status.LastSyncedConfig).To(Equal(expected))
		})
	})
})
