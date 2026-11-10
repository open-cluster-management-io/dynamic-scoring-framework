package dynamic_scoring_agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	cmdfactory "open-cluster-management.io/addon-framework/pkg/cmd/factory"
	"open-cluster-management.io/addon-framework/pkg/lease"
	"open-cluster-management.io/addon-framework/pkg/version"
	addonv1alpha1client "open-cluster-management.io/api/client/addon/clientset/versioned"
	clientset "open-cluster-management.io/api/client/cluster/clientset/versioned"
	apiv1alpha2 "open-cluster-management.io/api/cluster/v1alpha1"
	"open-cluster-management.io/dynamic-scoring/pkg/common"
	"open-cluster-management.io/sdk-go/pkg/basecontroller/factory"
)

const (
	configMapQueueKeyPrefix = "configmap:"
	secretQueueKeyPrefix    = "secret:"
)

var (
	dynamicScoreGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: common.DynamicScoreName,
			Help: "Dynamic score from each scorer",
		},
		[]string{
			common.DynamicScoreLabelPrefix + "cluster",
			common.DynamicScoreLabelPrefix + "node",
			common.DynamicScoreLabelPrefix + "device",
			common.DynamicScoreLabelPrefix + "namespace",
			common.DynamicScoreLabelPrefix + "app",
			common.DynamicScoreLabelPrefix + "pod",
			common.DynamicScoreLabelPrefix + "container",
			common.DynamicScoreLabelPrefix + "meta",
			common.DynamicScoreLabelPrefix + "score_name",
		},
	)
	requestsCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: common.DynamicScoreRequestsCounterName,
			Help: "Counter for scoring attempts",
		},
		[]string{
			"score_name",
			"status",
		},
	)
	fetchSecondsCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: common.DynamicScoreFetchSecondsCounterName,
			Help: "Counter for scoring fetch duration in seconds",
		},
		[]string{
			"score_name",
		},
	)
	performSecondsCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: common.DynamicScorePerformSecondsCounterName,
			Help: "Counter for scoring perform duration in seconds",
		},
		[]string{
			"score_name",
		},
	)
	sendSecondsCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: common.DynamicScoreSendSecondsCounterName,
			Help: "Counter for scoring send duration in seconds",
		},
		[]string{
			"score_name",
		},
	)
)

func init() {
	prometheus.MustRegister(dynamicScoreGauge)
	prometheus.MustRegister(requestsCounter)
	prometheus.MustRegister(fetchSecondsCounter)
	prometheus.MustRegister(performSecondsCounter)
	prometheus.MustRegister(sendSecondsCounter)
}

// startMetricsServer starts an HTTP server to expose Prometheus metrics.
func startMetricsServer() {
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		klog.Info("Starting metrics server on :8080")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			klog.Fatalf("failed to start metrics server: %v", err)
		}
	}()
}

func NewAgentCommand(addonName string) *cobra.Command {
	o := NewAgentOptions(addonName)
	cmd := cmdfactory.
		NewControllerCommandConfig("dynamic-scoring-addon-agent", version.Get(), o.RunAgent).
		NewCommand()
	cmd.Use = "agent"
	cmd.Short = "Start the addon agent"

	o.AddFlags(cmd)
	return cmd
}

type AgentOptions struct {
	HubKubeconfigFile     string
	ManagedKubeconfigFile string
	SpokeClusterName      string
	AddonName             string
	AddonNamespace        string
}

func NewAgentOptions(addonName string) *AgentOptions {
	return &AgentOptions{AddonName: addonName}
}

func (o *AgentOptions) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringVar(&o.HubKubeconfigFile, "hub-kubeconfig", o.HubKubeconfigFile,
		"Location of kubeconfig file to connect to hub cluster.")
	flags.StringVar(&o.ManagedKubeconfigFile, "managed-kubeconfig", o.ManagedKubeconfigFile,
		"Location of kubeconfig file to connect to the managed cluster.")
	flags.StringVar(&o.SpokeClusterName, "cluster-name", o.SpokeClusterName, "Name of spoke cluster.")
	flags.StringVar(&o.AddonNamespace, "addon-namespace", o.AddonNamespace, "Installation namespace of addon.")
	flags.StringVar(&o.AddonName, "addon-name", o.AddonName, "name of the addon.")
}

func (o *AgentOptions) RunAgent(ctx context.Context, kubeconfig *rest.Config) error {
	managementKubeClient, err := kubernetes.NewForConfig(kubeconfig)
	if err != nil {
		return err
	}

	spokeKubeClient := managementKubeClient
	if len(o.ManagedKubeconfigFile) != 0 {
		managedRestConfig, err := clientcmd.BuildConfigFromFlags("", o.ManagedKubeconfigFile)
		if err != nil {
			return err
		}
		spokeKubeClient, err = kubernetes.NewForConfig(managedRestConfig)
		if err != nil {
			return err
		}
	}

	hubRestConfig, err := clientcmd.BuildConfigFromFlags("", o.HubKubeconfigFile)
	if err != nil {
		return err
	}
	hubKubeClient, err := kubernetes.NewForConfig(hubRestConfig)
	if err != nil {
		return err
	}
	addonClient, err := addonv1alpha1client.NewForConfig(hubRestConfig)
	if err != nil {
		return err
	}
	hubClusterClient, err := clientset.NewForConfig(hubRestConfig)
	if err != nil {
		return err
	}

	hubKubeInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		hubKubeClient,
		10*time.Minute,
		informers.WithNamespace(o.SpokeClusterName),
	)
	dynamicScoringConfigMapInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		spokeKubeClient,
		10*time.Minute,
		informers.WithNamespace(common.DynamicScoringNamespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("metadata.name", common.DynamicScoringConfigName).String()
		}),
	)
	secretInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		spokeKubeClient,
		0,
		informers.WithNamespace(o.AddonNamespace),
	)

	_, controller := newAgentController(
		hubClusterClient,
		spokeKubeClient,
		addonClient,
		dynamicScoringConfigMapInformerFactory.Core().V1().ConfigMaps(),
		secretInformerFactory.Core().V1().Secrets(),
		o.SpokeClusterName,
		o.AddonName,
		o.AddonNamespace,
	)
	leaseUpdater := lease.NewLeaseUpdater(
		managementKubeClient,
		o.AddonName,
		o.AddonNamespace,
	)

	go hubKubeInformerFactory.Start(ctx.Done())
	go dynamicScoringConfigMapInformerFactory.Start(ctx.Done())
	go secretInformerFactory.Start(ctx.Done())
	go controller.Run(ctx, 1)
	go leaseUpdater.Start(ctx)

	klog.Info("Started dynamic scoring agent controller.")

	go startMetricsServer()

	klog.Info("Started prometheus server for exporting dynamic score.")

	<-ctx.Done()
	return nil
}

type ScoringWorker struct {
	cancel         context.CancelFunc
	summaryLock    sync.RWMutex
	summary        *common.ScorerSummary
	interval       time.Duration
	intervalUpdate chan struct{}
}

func (w *ScoringWorker) setSummary(summary *common.ScorerSummary, interval time.Duration) {
	copy := *summary
	w.summaryLock.Lock()
	intervalChanged := w.summary != nil && w.interval != interval
	w.summary = &copy
	w.interval = interval
	w.summaryLock.Unlock()
	if intervalChanged {
		select {
		case w.intervalUpdate <- struct{}{}:
		default:
		}
	}
}

func (w *ScoringWorker) getSummary() *common.ScorerSummary {
	w.summaryLock.RLock()
	defer w.summaryLock.RUnlock()
	copy := *w.summary
	return &copy
}

func (w *ScoringWorker) getInterval() time.Duration {
	w.summaryLock.RLock()
	defer w.summaryLock.RUnlock()
	return w.interval
}

type agentController struct {
	hubClusterClient     clientset.Interface
	spokeKubeClient      kubernetes.Interface
	addonClient          addonv1alpha1client.Interface
	spokeConfigMapLister corev1lister.ConfigMapLister
	referencedSecrets    map[string]struct{}
	clusterName          string
	addonName            string
	addonNamespace       string
	scoringWorkers       map[string]*ScoringWorker
	workerLock           sync.Mutex
	authLock             sync.RWMutex
	sourceAuth           map[string]string
	scoringAuth          map[string]string
	httpClient           *http.Client
	previousLabelSets    map[string]map[string]struct{}
}

func newAgentController(
	hubClusterClient clientset.Interface,
	spokeKubeClient kubernetes.Interface,
	addonClient addonv1alpha1client.Interface,
	configmapInformers corev1informers.ConfigMapInformer,
	secretInformers corev1informers.SecretInformer,
	clusterName string,
	addonName string,
	addonNamespace string,
) (*agentController, factory.Controller) {
	c := &agentController{
		hubClusterClient:     hubClusterClient,
		spokeKubeClient:      spokeKubeClient,
		addonClient:          addonClient,
		clusterName:          clusterName,
		addonName:            addonName,
		addonNamespace:       addonNamespace,
		spokeConfigMapLister: configmapInformers.Lister(),
		referencedSecrets:    make(map[string]struct{}),
		scoringWorkers:       make(map[string]*ScoringWorker),
		workerLock:           sync.Mutex{},
		sourceAuth:           make(map[string]string),
		scoringAuth:          make(map[string]string),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:       10,
				IdleConnTimeout:    90 * time.Second,
				DisableCompression: false,
			},
		},
		previousLabelSets: make(map[string]map[string]struct{}),
	}
	controller := factory.New().
		WithInformersQueueKeysFunc(queueKeys(configMapQueueKeyPrefix), configmapInformers.Informer()).
		WithInformersQueueKeysFunc(queueKeys(secretQueueKeyPrefix), secretInformers.Informer()).
		WithSync(c.sync).ToController("dynamic-scoring-agent-controller")

	return c, controller
}

func (c *agentController) sync(ctx context.Context, syncCtx factory.SyncContext, key string) error {
	if strings.HasPrefix(key, configMapQueueKeyPrefix) {
		return c.syncConfigMap(ctx, strings.TrimPrefix(key, configMapQueueKeyPrefix))
	}
	if strings.HasPrefix(key, secretQueueKeyPrefix) {
		ns, name, err := cache.SplitMetaNamespaceKey(strings.TrimPrefix(key, secretQueueKeyPrefix))
		if err != nil {
			return err
		}
		if ns != c.addonNamespace {
			return nil
		}
		if _, referenced := c.referencedSecrets[name]; !referenced {
			// The Secret is not referenced by any active scorer, so we don't need to do anything.
			return nil
		}

		// The ConfigMap is the source of truth; a referenced Secret event only triggers its reconciliation.
		// This ensures if a secret is updated/deleted/created, the corresponding scorer will be reconciled
		// and the new secret will be fetched.
		configMapKey := common.DynamicScoringNamespace + "/" + common.DynamicScoringConfigName
		return c.syncConfigMap(ctx, configMapKey)
	}

	return nil
}

func queueKeys(prefix string) factory.ObjectQueueKeysFunc {
	return func(obj runtime.Object) []string {
		key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
		if err != nil {
			klog.ErrorS(err, "Failed to create informer queue key", "prefix", prefix)
			return nil
		}

		return []string{prefix + key}
	}
}

func (c *agentController) syncConfigMap(ctx context.Context, key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	if ns != common.DynamicScoringNamespace || name != common.DynamicScoringConfigName {
		return nil
	}

	klog.Infof("Reconciling ConfigMap: %s", key)

	cm, err := c.spokeConfigMapLister.ConfigMaps(ns).Get(name)

	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	c.handleConfigMapUpdate(ctx, cm)
	return nil
}

func (c *agentController) handleConfigMapUpdate(ctx context.Context, cm *corev1.ConfigMap) {
	data, ok := cm.Data["summaries"]
	if !ok {
		klog.Warning("Missing 'summaries' key in configmap")
		return
	}
	var summaries []common.ScorerSummary
	if err := json.Unmarshal([]byte(data), &summaries); err != nil {
		klog.ErrorS(err, "Invalid JSON in summaries")
		return
	}
	c.referencedSecrets = referencedSecretNames(summaries)

	klog.Infof("Summay Num: %d", len(summaries))

	// Track active scorers and manage workers
	active := make(map[string]bool)
	for _, s := range summaries {
		klog.Infof("Fetch secrets : %s", s.ScoreName)
		interval, valid := scoringIntervalDuration(s.ScoringInterval)
		if !valid {
			klog.Warningf(
				"Skipping scorer with non-positive scoring interval (%s): %d",
				s.ScoreName,
				s.ScoringInterval,
			)
			continue
		}
		if err := c.fetchSourceAuth(ctx, s); err != nil {
			klog.ErrorS(err, "Failed to fetch source auth, skipping scorer", "scoreName", s.ScoreName)
			continue
		}
		if err := c.fetchScoringAuth(ctx, s); err != nil {
			klog.ErrorS(err, "Failed to fetch scoring auth, skipping scorer", "scoreName", s.ScoreName)
			continue
		}
		klog.Infof("Start target : %s", s.ScoreName)
		c.ensureScoringWorker(ctx, &s, interval, true)
		active[s.ScoreName] = true
	}

	var workersToStop []string

	c.workerLock.Lock()
	for name := range c.scoringWorkers {
		if !active[name] {
			workersToStop = append(workersToStop, name)
		}
	}
	c.workerLock.Unlock()

	// Delete metrics for stopped workers
	for _, name := range workersToStop {
		c.stopScoringWorker(name)
	}
}

func (c *agentController) ensureScoringWorker(
	ctx context.Context,
	summary *common.ScorerSummary,
	interval time.Duration,
	updateExisting bool,
) {
	// The interval has already been validated while reconciling the ConfigMap summary.
	c.workerLock.Lock()
	if worker, exists := c.scoringWorkers[summary.ScoreName]; exists {
		if updateExisting {
			worker.setSummary(summary, interval)
		}
		c.workerLock.Unlock()
		klog.Infof("Scoring worker already exists for %s", summary.ScoreName)
		return
	}
	ctxWorker, cancel := context.WithCancel(ctx)
	worker := &ScoringWorker{cancel: cancel, intervalUpdate: make(chan struct{}, 1)}
	worker.setSummary(summary, interval)
	c.scoringWorkers[summary.ScoreName] = worker
	c.workerLock.Unlock()
	go c.startScoringLoop(ctxWorker, worker)
	klog.Infof("Started scoring worker for %s", summary.ScoreName)
}

func (c *agentController) stopScoringWorker(scoreName string) {
	c.workerLock.Lock()
	worker, exists := c.scoringWorkers[scoreName]
	if exists {
		worker.cancel()
		delete(c.scoringWorkers, scoreName)
	}
	c.workerLock.Unlock()
	c.clearAuth(scoreName)
	dynamicScoreGauge.DeletePartialMatch(prometheus.Labels{
		common.DynamicScoreLabelPrefix + "score_name": scoreName,
	})
	if exists {
		klog.Infof("Stopped scoring worker for %s", scoreName)
		klog.Infof("Deleted metrics for stopped worker %s", scoreName)
	}
}

func referencedSecretNames(summaries []common.ScorerSummary) map[string]struct{} {
	referenced := make(map[string]struct{})
	for _, summary := range summaries {
		if summary.SourceEndpointAuthName != "" && summary.SourceEndpointAuthKey != "" {
			referenced[summary.SourceEndpointAuthName] = struct{}{}
		}
		if summary.ScoringEndpointAuthName != "" && summary.ScoringEndpointAuthKey != "" {
			referenced[summary.ScoringEndpointAuthName] = struct{}{}
		}
	}
	return referenced
}

func (c *agentController) setSourceAuth(scoreName, token string) {
	c.authLock.Lock()
	defer c.authLock.Unlock()
	if token == "" {
		delete(c.sourceAuth, scoreName)
		return
	}
	c.sourceAuth[scoreName] = token
}

func (c *agentController) getSourceAuth(scoreName string) string {
	c.authLock.RLock()
	defer c.authLock.RUnlock()
	return c.sourceAuth[scoreName]
}

func (c *agentController) setScoringAuth(scoreName, token string) {
	c.authLock.Lock()
	defer c.authLock.Unlock()
	if token == "" {
		delete(c.scoringAuth, scoreName)
		return
	}
	c.scoringAuth[scoreName] = token
}

func (c *agentController) getScoringAuth(scoreName string) string {
	c.authLock.RLock()
	defer c.authLock.RUnlock()
	return c.scoringAuth[scoreName]
}

func (c *agentController) clearAuth(scoreName string) {
	c.authLock.Lock()
	defer c.authLock.Unlock()
	delete(c.sourceAuth, scoreName)
	delete(c.scoringAuth, scoreName)
}

func scoringIntervalDuration(intervalSeconds int) (time.Duration, bool) {
	if intervalSeconds <= 0 {
		return 0, false
	}

	return time.Duration(intervalSeconds) * time.Second, true
}

func (c *agentController) startScoringLoop(ctx context.Context, worker *ScoringWorker) {
	summary := worker.getSummary()
	ticker := time.NewTicker(worker.getInterval())
	defer ticker.Stop()

	klog.Infof("Start Scoring Loop : %s", summary.ScoreName)

	for {
		select {
		case <-ctx.Done():
			summary := worker.getSummary()
			klog.Infof("Exiting scoring loop for %s", summary.ScoreName)
			return
		case <-worker.intervalUpdate:
			ticker.Reset(worker.getInterval())
		case <-ticker.C:
			summary := worker.getSummary()
			if err := c.performScoring(ctx, summary); err != nil {
				klog.Errorf("Scoring error (%s): %v", summary.ScoreName, err)
				requestsCounter.WithLabelValues(summary.ScoreName, "failure").Inc()
			} else {
				requestsCounter.WithLabelValues(summary.ScoreName, "success").Inc()
			}
		}
	}
}

func (c *agentController) performScoring(ctx context.Context, summary *common.ScorerSummary) error {
	klog.Infof("performScoring start : %s", summary.ScoreName)

	var scoringInputData []common.PrometheusMatrixSeries

	// Fetch source data
	if summary.SourceType == common.SourceTypePrometheus {
		fetchStart := time.Now()

		parsedQueries := strings.Split(summary.SourceQuery, ";")
		klog.Infof("Parsed Queries Count: %d for %s", len(parsedQueries), summary.ScoreName)

		end := time.Now()
		start := end.Add(-time.Duration(summary.SourceRange) * time.Second)

		for _, q := range parsedQueries {
			params := url.Values{
				"query": []string{q},
				"start": []string{fmt.Sprintf("%d", start.Unix())},
				"end":   []string{fmt.Sprintf("%d", end.Unix())},
				"step":  []string{fmt.Sprintf("%d", summary.SourceStep)},
			}
			fullURL := fmt.Sprintf("%s?%s", summary.SourceEndpoint, params.Encode())

			req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
			if err != nil {
				return fmt.Errorf("failed to create prometheus query request (%s): %w", summary.ScoreName, err)
			}
			// sourceHeader
			if token := c.getSourceAuth(summary.ScoreName); token != "" {
				req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
			}

			resp, err := c.httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("failed to query prometheus (%s): %w", summary.ScoreName, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("prometheus query failed with status %d for %s", resp.StatusCode, summary.ScoreName)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("failed to read prometheus response: %w", err)
			}

			var promResp common.PrometheusQueryRangeResponse
			if err := json.Unmarshal(body, &promResp); err != nil {
				return fmt.Errorf("prometheus response JSON unmarshal error: %w", err)
			}
			if promResp.Status != "success" {
				return fmt.Errorf("prometheus query failed: %s", promResp.Status)
			}
			scoringInputData = append(scoringInputData, promResp.Data.Result...)
		}

		// Record fetch duration
		fetchDuration := time.Since(fetchStart).Seconds()
		fetchSecondsCounter.WithLabelValues(summary.ScoreName).Add(fetchDuration)
		klog.Infof("Fetched source data in %.2f seconds for %s", fetchDuration, summary.ScoreName)
	}

	scoringPayload := common.ScoringRequest{Data: scoringInputData}
	payloadBytes, err := json.Marshal(scoringPayload)

	if err != nil {
		return fmt.Errorf("failed to marshal scoring payload: %w", err)
	}

	req2, err := http.NewRequestWithContext(ctx, "POST", summary.ScoringEndpoint, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create scoring request (%s): %w", summary.ScoreName, err)
	}

	// scoringHeader
	req2.Header.Set("Content-Type", "application/json")
	if token := c.getScoringAuth(summary.ScoreName); token != "" {
		req2.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	}

	performStart := time.Now()
	resp2, err := c.httpClient.Do(req2)
	if err != nil {
		return fmt.Errorf("failed to query scoring endpoint (%s): %w", summary.ScoreName, err)
	}
	defer func() { _ = resp2.Body.Close() }()

	if resp2.StatusCode != http.StatusOK {
		return fmt.Errorf("scoring endpoint returned status %d for %s", resp2.StatusCode, summary.ScoreName)
	}

	resp2Body, err := io.ReadAll(resp2.Body)
	if err != nil {
		return fmt.Errorf("failed to read scoring response (%s): %w", summary.ScoreName, err)
	}

	// Record perform duration
	performDuration := time.Since(performStart).Seconds()
	performSecondsCounter.WithLabelValues(summary.ScoreName).Add(performDuration)
	klog.Infof("Performed scoring in %.2f seconds for %s", performDuration, summary.ScoreName)

	var scoringResp common.ScoringResponse
	if err := json.Unmarshal(resp2Body, &scoringResp); err != nil {
		return fmt.Errorf("scoring response JSON unmarshal error: %w", err)
	}

	currentLabels := make(map[string]struct{})
	currentPlaceHolderMapping := make(map[string]map[string]string)
	currentScores := make(map[string]float64)

	for _, r := range scoringResp.Results {
		labels := []string{
			c.clusterName,
			getValidString(r.Metric, "node", true),
			getValidString(r.Metric, "device", true),
			getValidString(r.Metric, "namespace", true),
			getValidString(r.Metric, "app", true),
			getValidString(r.Metric, "pod", true),
			getValidString(r.Metric, "container", true),
			getValidString(r.Metric, "meta", true),
			summary.ScoreName,
		}
		placeHolderMapping := map[string]string{
			"${cluster}":   c.clusterName,
			"${node}":      getValidString(r.Metric, "node", false),
			"${device}":    getValidString(r.Metric, "device", false),
			"${namespace}": getValidString(r.Metric, "namespace", false),
			"${app}":       getValidString(r.Metric, "app", false),
			"${pod}":       getValidString(r.Metric, "pod", false),
			"${container}": getValidString(r.Metric, "container", false),
			"${meta}":      getValidString(r.Metric, "meta", false),
			"${scoreName}": summary.ScoreName,
		}
		aggregatedLabels := aggregateLabels(labels)
		currentPlaceHolderMapping[aggregatedLabels] = placeHolderMapping
		currentScores[aggregatedLabels] = r.Score
		dynamicScoreGauge.WithLabelValues(labels...).Set(r.Score)

		currentLabels[labelKey(labels...)] = struct{}{}
	}

	klog.Infof("performScoring success : %s", summary.ScoreName)

	// Clean up old metrics
	if prev, ok := c.previousLabelSets[summary.ScoreName]; ok {
		for labelStr := range prev {
			if _, stillExists := currentLabels[labelStr]; !stillExists {
				labels := strings.Split(labelStr, "||")
				dynamicScoreGauge.DeleteLabelValues(labels...)
			}
		}
	}

	c.previousLabelSets[summary.ScoreName] = currentLabels

	// Send scores to storage
	sendStart := time.Now()

	if summary.ScoreDestination == common.ScoreDestinationAddOnPlacementScore {
		if err := c.updateAddOnPlacementScore(ctx, summary, currentPlaceHolderMapping, currentScores); err != nil {
			return fmt.Errorf("failed to update AddOnPlacementScore (%s): %w", summary.ScoreName, err)
		}
	}
	// Record send duration
	sendDuration := time.Since(sendStart).Seconds()
	sendSecondsCounter.WithLabelValues(summary.ScoreName).Add(sendDuration)
	klog.Infof("Sent scores in %.2f seconds for %s", sendDuration, summary.ScoreName)

	return nil
}

func getValidString(m map[string]string, key string, truncate bool) string {
	if val, ok := m[key]; ok {
		if truncate && len(val) > common.DynamicScoreLabelMaxLength {
			val = val[:common.DynamicScoreLabelMaxLength]
		}
		return val
	}
	return ""
}

func (c *agentController) fetchSourceAuth(ctx context.Context, summary common.ScorerSummary) error {
	if summary.SourceEndpointAuthName == "" || summary.SourceEndpointAuthKey == "" {
		c.setSourceAuth(summary.ScoreName, "")
		return nil
	}
	secret, err := c.spokeKubeClient.CoreV1().
		Secrets(c.addonNamespace).
		Get(ctx, summary.SourceEndpointAuthName, metav1.GetOptions{})
	if err != nil {
		c.setSourceAuth(summary.ScoreName, "")
		return fmt.Errorf("failed to get source auth secret (%s): %w", summary.ScoreName, err)
	}
	val, ok := secret.Data[summary.SourceEndpointAuthKey]
	if !ok {
		c.setSourceAuth(summary.ScoreName, "")
		return fmt.Errorf("source auth key not found in secret (%s): %s", summary.ScoreName, summary.SourceEndpointAuthKey)
	}
	c.setSourceAuth(summary.ScoreName, string(val))
	return nil
}

func (c *agentController) fetchScoringAuth(ctx context.Context, summary common.ScorerSummary) error {
	if summary.ScoringEndpointAuthName == "" || summary.ScoringEndpointAuthKey == "" {
		c.setScoringAuth(summary.ScoreName, "")
		return nil
	}
	secret, err := c.spokeKubeClient.CoreV1().
		Secrets(c.addonNamespace).
		Get(ctx, summary.ScoringEndpointAuthName, metav1.GetOptions{})
	if err != nil {
		c.setScoringAuth(summary.ScoreName, "")
		return fmt.Errorf("failed to get scoring auth secret (%s): %w", summary.ScoreName, err)
	}
	val, ok := secret.Data[summary.ScoringEndpointAuthKey]
	if !ok {
		c.setScoringAuth(summary.ScoreName, "")
		return fmt.Errorf(
			"scoring auth key not found in secret (%s): %s",
			summary.ScoreName,
			summary.ScoringEndpointAuthKey,
		)
	}
	c.setScoringAuth(summary.ScoreName, string(val))
	return nil
}

func (c *agentController) updateAddOnPlacementScore(
	ctx context.Context,
	summary *common.ScorerSummary,
	currentPlaceHolderMapping map[string]map[string]string,
	currentScores map[string]float64,
) error {
	// See resource-usage-collect-addon agent for reference implementation.
	sanitizedName := sanitizeResourceName(summary.ScoreName)
	klog.Info("Checking AddOnPlacementScore: ", c.clusterName, sanitizedName)

	scoreClient := c.hubClusterClient.ClusterV1alpha1().AddOnPlacementScores(c.clusterName)
	addonPlacementScore, err := scoreClient.Get(ctx, sanitizedName, metav1.GetOptions{})
	klog.Info("Current AddOnPlacementScore: ", addonPlacementScore, "err: ", err)

	items := []apiv1alpha2.AddOnPlacementScoreItem{}
	dimensionValueMap := make(map[string]int32)

	for labelStr, score := range currentScores {
		renderedDimentionName := renderDimansionName(summary.ScoreDimensionFormat, currentPlaceHolderMapping[labelStr])
		if renderedDimentionName == "" {
			continue
		}
		dimensionValueMap[renderedDimentionName] = int32(score)
	}
	for dimension, value := range dimensionValueMap {
		items = append(items, apiv1alpha2.AddOnPlacementScoreItem{
			Name:  dimension,
			Value: value,
		})
	}

	switch {
	case errors.IsNotFound(err):
		klog.Info("not found AddOnPlacementScore, creating new one")
		klog.Info(err)
		addonPlacementScore = &apiv1alpha2.AddOnPlacementScore{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: c.clusterName,
				Name:      sanitizedName,
			},
			Status: apiv1alpha2.AddOnPlacementScoreStatus{
				Scores: items,
			},
		}
		_, err = scoreClient.Create(ctx, addonPlacementScore, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		return nil
	case err != nil:
		return err
	}

	klog.Info("AddOnPlacementScore found, updating existing one")
	addonPlacementScore.Status.Scores = items
	_, err = scoreClient.UpdateStatus(ctx, addonPlacementScore, metav1.UpdateOptions{})
	return err
}

func labelKey(values ...string) string {
	return strings.Join(values, "||")
}

const defaultSanitizedScoreName = "score"

func isAlphaNum(r rune) bool {
	return ('a' <= r && r <= 'z') || ('0' <= r && r <= '9')
}

func sanitizeResourceName(name string) string {
	if name == "" {
		return defaultSanitizedScoreName
	}
	// lowercase
	s := strings.ToLower(name)

	// replace any char not in [a-z0-9.-] with '-'
	reInvalid := regexp.MustCompile(`[^a-z0-9\.-]`)
	s = reInvalid.ReplaceAllString(s, "-")

	// collapse multiple '-'
	reDash := regexp.MustCompile(`-+`)
	s = reDash.ReplaceAllString(s, "-")

	// trim leading/trailing '-' or '.'
	s = strings.Trim(s, "-.")
	// ensure length <= 253
	if len(s) > 253 {
		s = s[:253]
		// if cut ends with non-alnum, trim trailing non-alnum
		s = strings.TrimRightFunc(s, func(r rune) bool {
			return !isAlphaNum(r)
		})
	}

	// ensure starts with alnum
	if s == "" {
		s = defaultSanitizedScoreName
	}
	first := s[0]
	last := s[len(s)-1]
	if !isAlphaNum(rune(first)) {
		s = "a" + s
	}
	if !isAlphaNum(rune(last)) {
		s = s + "a"
	}

	return s
}

func aggregateLabels(labels []string) string {
	noneEmptyLabels := []string{}
	for _, label := range labels {
		if label != "" {
			noneEmptyLabels = append(noneEmptyLabels, label)
		}
	}
	return strings.Join(noneEmptyLabels, ";")
}

func renderDimansionName(format string, placeHolderMapping map[string]string) string {
	result := format
	for placeholder, value := range placeHolderMapping {
		result = strings.ReplaceAll(result, placeholder, value)
	}
	return result
}
