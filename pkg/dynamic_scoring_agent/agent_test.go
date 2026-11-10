package dynamic_scoring_agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"open-cluster-management.io/dynamic-scoring/pkg/common"
)

func TestSanitizeResourceName(t *testing.T) {
	name := sanitizeResourceName("My Score@Name")
	if name != "my-score-name" {
		t.Fatalf("unexpected sanitized name: %s", name)
	}

	empty := sanitizeResourceName("")
	if empty != defaultSanitizedScoreName {
		t.Fatalf("unexpected sanitized empty name: %s", empty)
	}
}

func TestLabelKey(t *testing.T) {
	key := labelKey("a", "b", "c")
	if key != "a||b||c" {
		t.Fatalf("unexpected label key: %s", key)
	}
}

func TestAggregateLabels(t *testing.T) {
	out := aggregateLabels([]string{"a", "", "b", ""})
	if out != "a;b" {
		t.Fatalf("unexpected aggregate labels: %s", out)
	}
}

func TestRenderDimansionName(t *testing.T) {
	format := "${cluster}-${scoreName}-${node}"
	mapping := map[string]string{
		"${cluster}":   "cluster-a",
		"${scoreName}": "latency",
		"${node}":      "node-1",
	}

	name := renderDimansionName(format, mapping)
	if name != "cluster-a-latency-node-1" {
		t.Fatalf("unexpected rendered name: %s", name)
	}
}

func TestGetValidString(t *testing.T) {
	metric := map[string]string{"node": "node-1"}
	if got := getValidString(metric, "node", true); got != "node-1" {
		t.Fatalf("unexpected value: %s", got)
	}

	longValue := strings.Repeat("a", common.DynamicScoreLabelMaxLength+10)
	metric["meta"] = longValue
	truncated := getValidString(metric, "meta", true)
	if len(truncated) != common.DynamicScoreLabelMaxLength {
		t.Fatalf("expected truncation to %d, got %d", common.DynamicScoreLabelMaxLength, len(truncated))
	}
}

func TestReferencedSecretNames(t *testing.T) {
	summaries := []common.ScorerSummary{
		{
			SourceEndpointAuthName:  "shared-secret",
			SourceEndpointAuthKey:   "source-token",
			ScoringEndpointAuthName: "shared-secret",
			ScoringEndpointAuthKey:  "scoring-token",
		},
		{SourceEndpointAuthName: "incomplete-secret"},
	}

	got := referencedSecretNames(summaries)
	if len(got) != 1 {
		t.Fatalf("referenced secrets = %#v; want only the complete reference", got)
	}
	if _, ok := got["shared-secret"]; !ok {
		t.Fatalf("referenced secrets = %#v; missing shared-secret", got)
	}
}

func TestFetchAuthClearsCachedTokenWhenReferenceIsRemovedOrInvalid(t *testing.T) {
	const namespace = "dynamic-scoring"
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "auth-secret", Namespace: namespace},
		Data:       map[string][]byte{"token": []byte("fresh-token")},
	})
	controller := &agentController{
		spokeKubeClient: client,
		addonNamespace:  namespace,
		sourceAuth:      map[string]string{"score-a": "stale-source-token"},
		scoringAuth:     map[string]string{"score-a": "stale-scoring-token"},
	}
	summary := common.ScorerSummary{
		ScoreName:               "score-a",
		SourceEndpointAuthName:  "auth-secret",
		SourceEndpointAuthKey:   "token",
		ScoringEndpointAuthName: "auth-secret",
		ScoringEndpointAuthKey:  "token",
	}

	if err := controller.fetchSourceAuth(context.Background(), summary); err != nil {
		t.Fatalf("fetch source auth: %v", err)
	}
	if err := controller.fetchScoringAuth(context.Background(), summary); err != nil {
		t.Fatalf("fetch scoring auth: %v", err)
	}
	if got := controller.getSourceAuth(summary.ScoreName); got != "fresh-token" {
		t.Fatalf("source auth = %q; want fresh-token", got)
	}
	if got := controller.getScoringAuth(summary.ScoreName); got != "fresh-token" {
		t.Fatalf("scoring auth = %q; want fresh-token", got)
	}

	summary.SourceEndpointAuthName = ""
	summary.SourceEndpointAuthKey = ""
	summary.ScoringEndpointAuthName = ""
	summary.ScoringEndpointAuthKey = ""
	if err := controller.fetchSourceAuth(context.Background(), summary); err != nil {
		t.Fatalf("fetch removed source auth: %v", err)
	}
	if err := controller.fetchScoringAuth(context.Background(), summary); err != nil {
		t.Fatalf("fetch removed scoring auth: %v", err)
	}
	if got := controller.getSourceAuth(summary.ScoreName); got != "" {
		t.Errorf("source auth after ref removal = %q; want empty", got)
	}
	if got := controller.getScoringAuth(summary.ScoreName); got != "" {
		t.Errorf("scoring auth after ref removal = %q; want empty", got)
	}

	controller.setSourceAuth(summary.ScoreName, "stale-source-token")
	controller.setScoringAuth(summary.ScoreName, "stale-scoring-token")
	summary.SourceEndpointAuthName = "auth-secret"
	summary.SourceEndpointAuthKey = "missing-key"
	summary.ScoringEndpointAuthName = "auth-secret"
	summary.ScoringEndpointAuthKey = "missing-key"
	if err := controller.fetchSourceAuth(context.Background(), summary); err == nil {
		t.Fatal("expected source auth fetch to fail for a missing key")
	}
	if err := controller.fetchScoringAuth(context.Background(), summary); err == nil {
		t.Fatal("expected scoring auth fetch to fail for a missing key")
	}
	if got := controller.getSourceAuth(summary.ScoreName); got != "" {
		t.Errorf("source auth after missing key = %q; want empty", got)
	}
	if got := controller.getScoringAuth(summary.ScoreName); got != "" {
		t.Errorf("scoring auth after missing key = %q; want empty", got)
	}
}

func TestPerformScoringUsesLatestAuthTokens(t *testing.T) {
	sourceTokens := make(chan string, 3)
	scoringTokens := make(chan string, 3)
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceTokens <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer sourceServer.Close()
	scoringServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scoringTokens <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer scoringServer.Close()

	controller := &agentController{
		clusterName:       "cluster-a",
		sourceAuth:        make(map[string]string),
		scoringAuth:       make(map[string]string),
		httpClient:        &http.Client{Timeout: time.Second},
		previousLabelSets: make(map[string]map[string]struct{}),
	}
	summary := &common.ScorerSummary{
		ScoreName:        "score-a",
		SourceType:       common.SourceTypePrometheus,
		SourceEndpoint:   sourceServer.URL,
		SourceQuery:      "up",
		SourceRange:      60,
		SourceStep:       15,
		ScoringEndpoint:  scoringServer.URL,
		ScoreDestination: common.ScoreDestinationNone,
	}

	for _, tokens := range []struct {
		source  string
		scoring string
	}{
		{source: "source-v1", scoring: "scoring-v1"},
		{source: "source-v2", scoring: "scoring-v2"},
		{},
	} {
		controller.setSourceAuth(summary.ScoreName, tokens.source)
		controller.setScoringAuth(summary.ScoreName, tokens.scoring)
		if err := controller.performScoring(context.Background(), summary); err != nil {
			t.Fatalf("perform scoring: %v", err)
		}

		wantSource := ""
		if tokens.source != "" {
			wantSource = "Bearer " + tokens.source
		}
		wantScoring := ""
		if tokens.scoring != "" {
			wantScoring = "Bearer " + tokens.scoring
		}
		if got := <-sourceTokens; got != wantSource {
			t.Errorf("source Authorization = %q; want %q", got, wantSource)
		}
		if got := <-scoringTokens; got != wantScoring {
			t.Errorf("scoring Authorization = %q; want %q", got, wantScoring)
		}
	}
}

func TestScoringWorkerResetsTickerWhenIntervalChanges(t *testing.T) {
	requests := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests <- struct{}{}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	controller := &agentController{
		clusterName:       "cluster-a",
		sourceAuth:        make(map[string]string),
		scoringAuth:       make(map[string]string),
		httpClient:        server.Client(),
		previousLabelSets: make(map[string]map[string]struct{}),
	}
	worker := &ScoringWorker{intervalUpdate: make(chan struct{}, 1)}
	initialSummary := &common.ScorerSummary{
		ScoreName:        "score-a",
		ScoringInterval:  1,
		ScoringEndpoint:  server.URL,
		ScoreDestination: common.ScoreDestinationNone,
	}
	worker.setSummary(initialSummary, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		controller.startScoringLoop(ctx, worker)
	}()
	defer func() {
		cancel()
		<-done
	}()

	select {
	case <-requests:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start with its initial scoring interval")
	}

	updatedSummary := *initialSummary
	updatedSummary.ScoringInterval = 3
	worker.setSummary(&updatedSummary, 3*time.Second)
	select {
	case <-requests:
		t.Fatal("worker did not reset its ticker to the updated interval")
	case <-time.After(1500 * time.Millisecond):
	}
	select {
	case <-requests:
	case <-time.After(4 * time.Second):
		t.Fatal("worker did not score after the updated interval")
	}
}

func TestConfigMapSkipsScorersWithInvalidIntervals(t *testing.T) {
	for _, interval := range []int{0, -1} {
		for _, hasWorker := range []bool{false, true} {
			t.Run(fmt.Sprintf("interval-%d/worker-%t", interval, hasWorker), func(t *testing.T) {
				const scoreName = "score-a"
				summary := common.ScorerSummary{ScoreName: scoreName, ScoringInterval: interval}
				controller := newTestAgentController(t, fake.NewSimpleClientset(), []common.ScorerSummary{summary})
				workerCtx, workerCancel := context.WithCancel(context.Background())
				defer workerCancel()
				if hasWorker {
					controller.scoringWorkers[scoreName] = &ScoringWorker{cancel: workerCancel}
					controller.setSourceAuth(scoreName, "source-token")
					controller.setScoringAuth(scoreName, "scoring-token")
				}

				controller.handleConfigMapUpdate(context.Background(), testConfigMap(t, []common.ScorerSummary{summary}))

				if _, exists := controller.scoringWorkers[scoreName]; exists {
					t.Fatal("scorer with invalid interval was started or left running")
				}
				if hasWorker {
					select {
					case <-workerCtx.Done():
					default:
						t.Fatal("existing scorer worker was not stopped")
					}
					if got := controller.getSourceAuth(scoreName); got != "" {
						t.Errorf("source auth after invalid interval = %q; want empty", got)
					}
					if got := controller.getScoringAuth(scoreName); got != "" {
						t.Errorf("scoring auth after invalid interval = %q; want empty", got)
					}
				}
			})
		}
	}
}

func TestSyncIgnoresUnreferencedSecret(t *testing.T) {
	const namespace = "dynamic-scoring"
	summary := common.ScorerSummary{
		ScoreName:              "score-a",
		SourceEndpointAuthName: "referenced-secret",
		SourceEndpointAuthKey:  "token",
	}
	data, err := json.Marshal([]common.ScorerSummary{summary})
	if err != nil {
		t.Fatalf("marshal scorer summary: %v", err)
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: common.DynamicScoringConfigName, Namespace: common.DynamicScoringNamespace},
		Data:       map[string]string{"summaries": string(data)},
	}
	client := fake.NewSimpleClientset(configMap)
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(namespace))
	configMapInformer := informerFactory.Core().V1().ConfigMaps()
	if err := configMapInformer.Informer().GetIndexer().Add(configMap); err != nil {
		t.Fatalf("add configmap to informer cache: %v", err)
	}
	secretInformer := informerFactory.Core().V1().Secrets()
	controller, _ := newAgentController(
		nil, client, nil, configMapInformer, secretInformer, "cluster-a", "addon-a", namespace,
	)
	controller.referencedSecrets = map[string]struct{}{"referenced-secret": {}}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	controller.scoringWorkers[summary.ScoreName] = &ScoringWorker{cancel: workerCancel}
	controller.setSourceAuth(summary.ScoreName, "cached-token")

	if err := controller.sync(context.Background(), nil, secretQueueKeyPrefix+namespace+"/unrelated-secret"); err != nil {
		t.Fatalf("sync unrelated secret: %v", err)
	}
	if _, ok := controller.scoringWorkers[summary.ScoreName]; !ok {
		t.Fatal("unrelated Secret event stopped the scorer")
	}
	if got := controller.getSourceAuth(summary.ScoreName); got != "cached-token" {
		t.Fatalf("unrelated Secret event changed cached token to %q", got)
	}
	workerCancel()
	<-workerCtx.Done()
}

func TestSyncReconcilesConfigMapWhenReferencedSecretAppears(t *testing.T) {
	const namespace = "dynamic-scoring"
	summary := common.ScorerSummary{
		ScoreName:              "score-a",
		SourceEndpointAuthName: "auth-secret",
		SourceEndpointAuthKey:  "token",
		ScoringInterval:        3600,
	}
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "auth-secret", Namespace: namespace},
		Data:       map[string][]byte{"token": []byte("new-token")},
	})
	controller := newTestAgentController(t, client, []common.ScorerSummary{summary})
	controller.referencedSecrets = map[string]struct{}{"auth-secret": {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := controller.sync(ctx, nil, secretQueueKeyPrefix+namespace+"/auth-secret"); err != nil {
		t.Fatalf("sync Secret appearance: %v", err)
	}
	if _, ok := controller.scoringWorkers[summary.ScoreName]; !ok {
		t.Fatal("scorer worker was not started after the Secret appeared")
	}
	if got := controller.getSourceAuth(summary.ScoreName); got != "new-token" {
		t.Fatalf("source token = %q; want new-token", got)
	}
}

func TestSyncStopsScorerWhenReferencedSecretIsUnavailable(t *testing.T) {
	const namespace = "dynamic-scoring"
	for _, tc := range []struct {
		name   string
		secret *corev1.Secret
	}{
		{name: "deleted"},
		{
			name: "key removed",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "auth-secret", Namespace: namespace},
				Data:       map[string][]byte{"other": []byte("value")},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary := common.ScorerSummary{
				ScoreName:              "score-a",
				SourceEndpointAuthName: "auth-secret",
				SourceEndpointAuthKey:  "token",
			}
			objects := []runtime.Object{testConfigMap(t, []common.ScorerSummary{summary})}
			if tc.secret != nil {
				objects = append(objects, tc.secret)
			}
			client := fake.NewSimpleClientset(objects...)
			controller := newTestAgentController(t, client, []common.ScorerSummary{summary})
			controller.referencedSecrets = map[string]struct{}{"auth-secret": {}}
			workerCtx, workerCancel := context.WithCancel(context.Background())
			controller.scoringWorkers[summary.ScoreName] = &ScoringWorker{cancel: workerCancel}
			controller.setSourceAuth(summary.ScoreName, "stale-source-token")
			controller.setScoringAuth(summary.ScoreName, "stale-scoring-token")

			if err := controller.sync(
				context.Background(), nil, secretQueueKeyPrefix+namespace+"/auth-secret",
			); err != nil {
				t.Fatalf("sync unavailable Secret: %v", err)
			}
			select {
			case <-workerCtx.Done():
			case <-time.After(time.Second):
				t.Fatal("scorer worker was not stopped")
			}
			if _, ok := controller.scoringWorkers[summary.ScoreName]; ok {
				t.Fatal("stopped scorer worker remains registered")
			}
			if got := controller.getSourceAuth(summary.ScoreName); got != "" {
				t.Errorf("source token after stop = %q; want empty", got)
			}
			if got := controller.getScoringAuth(summary.ScoreName); got != "" {
				t.Errorf("scoring token after stop = %q; want empty", got)
			}
		})
	}
}

func TestSyncConfigMapClearsAuthWhenReferenceIsRemoved(t *testing.T) {
	const namespace = "dynamic-scoring"
	summary := common.ScorerSummary{ScoreName: "score-a", ScoringInterval: 3600}
	client := fake.NewSimpleClientset()
	controller := newTestAgentController(t, client, []common.ScorerSummary{summary})
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	controller.scoringWorkers[summary.ScoreName] = &ScoringWorker{cancel: workerCancel}
	controller.setSourceAuth(summary.ScoreName, "old-source-token")
	controller.setScoringAuth(summary.ScoreName, "old-scoring-token")
	controller.referencedSecrets = map[string]struct{}{"old-secret": {}}

	if err := controller.syncConfigMap(context.Background(), namespace+"/"+common.DynamicScoringConfigName); err != nil {
		t.Fatalf("sync ConfigMap without auth refs: %v", err)
	}
	if got := controller.getSourceAuth(summary.ScoreName); got != "" {
		t.Errorf("source token after reference removal = %q; want empty", got)
	}
	if got := controller.getScoringAuth(summary.ScoreName); got != "" {
		t.Errorf("scoring token after reference removal = %q; want empty", got)
	}
	if _, ok := controller.scoringWorkers[summary.ScoreName]; !ok {
		t.Fatal("removing auth references unexpectedly stopped the scorer")
	}
	if len(controller.referencedSecrets) != 0 {
		t.Errorf("referenced Secrets after removal = %#v; want none", controller.referencedSecrets)
	}
	workerCancel()
	<-workerCtx.Done()
}

func TestStopScoringWorkerClearsAuthAndScoreMetric(t *testing.T) {
	const scoreName = "secret-refresh-stop-test"
	labels := []string{"cluster-a", "node-a", "", "", "", "", "", "", scoreName}
	dynamicScoreGauge.WithLabelValues(labels...).Set(42)
	controller := &agentController{
		scoringWorkers: make(map[string]*ScoringWorker),
		sourceAuth:     map[string]string{scoreName: "source-token"},
		scoringAuth:    map[string]string{scoreName: "scoring-token"},
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	controller.scoringWorkers[scoreName] = &ScoringWorker{cancel: workerCancel}

	controller.stopScoringWorker(scoreName)
	select {
	case <-workerCtx.Done():
	default:
		t.Fatal("worker context was not canceled")
	}
	if got := controller.getSourceAuth(scoreName); got != "" {
		t.Errorf("source auth after stop = %q; want empty", got)
	}
	if got := controller.getScoringAuth(scoreName); got != "" {
		t.Errorf("scoring auth after stop = %q; want empty", got)
	}

	metrics := make(chan prometheus.Metric, 16)
	dynamicScoreGauge.Collect(metrics)
	close(metrics)
	for metric := range metrics {
		collected := &dto.Metric{}
		if err := metric.Write(collected); err != nil {
			t.Fatalf("write collected metric: %v", err)
		}
		for _, label := range collected.GetLabel() {
			if label.GetName() == common.DynamicScoreLabelPrefix+"score_name" && label.GetValue() == scoreName {
				t.Fatalf("score metric for %q was not cleared", scoreName)
			}
		}
	}
}

func newTestAgentController(
	t *testing.T,
	client *fake.Clientset,
	summaries []common.ScorerSummary,
) *agentController {
	t.Helper()
	factory := informers.NewSharedInformerFactoryWithOptions(
		client,
		0,
		informers.WithNamespace(common.DynamicScoringNamespace),
	)
	configMaps := factory.Core().V1().ConfigMaps()
	if err := configMaps.Informer().GetIndexer().Add(testConfigMap(t, summaries)); err != nil {
		t.Fatalf("add ConfigMap to informer cache: %v", err)
	}
	controller, _ := newAgentController(
		nil,
		client,
		nil,
		configMaps,
		factory.Core().V1().Secrets(),
		"cluster-a",
		"addon-a",
		common.DynamicScoringNamespace,
	)
	return controller
}

func testConfigMap(t *testing.T, summaries []common.ScorerSummary) *corev1.ConfigMap {
	t.Helper()
	data, err := json.Marshal(summaries)
	if err != nil {
		t.Fatalf("marshal scorer summaries: %v", err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: common.DynamicScoringConfigName, Namespace: common.DynamicScoringNamespace},
		Data:       map[string]string{"summaries": string(data)},
	}
}
