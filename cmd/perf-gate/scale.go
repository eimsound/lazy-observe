package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

const (
	defaultScaleRounds        = 4
	defaultReadbackTimeout    = 30 * time.Second
	otlpResponseBodyLimit     = 1 << 20
	queryResponseBodyLimit    = 4 << 20
	readbackRetryInterval     = 250 * time.Millisecond
	scaleMetricName           = "lazy_observe_scale_gate_samples"
	scaleMarkerAttribute      = "scale_gate_run_id"
	scaleLogMessagePrefix     = "lazy-observe horizontal scaling gate "
	scaleInstanceIdentityPath = "/api/system/info"
	scaleSinkIdentityHeader   = "X-Lazy-Observe-Perf-Sink"
	scaleSinkReceiptHeader    = "X-Lazy-Observe-Perf-Receipt"
	scaleSinkRequestHeader    = "X-Lazy-Observe-Perf-Request"
)

type scalePhaseResult struct {
	Replicas                   int                 `json:"replicas"`
	DurationSeconds            float64             `json:"duration_seconds"`
	Concurrency                int                 `json:"concurrency"`
	MetricSamples              uint64              `json:"metric_samples"`
	LogRecords                 uint64              `json:"log_records"`
	SamplesPerSecond           float64             `json:"samples_per_second"`
	LogsPerSecond              float64             `json:"logs_per_second"`
	RecordsPerSecond           float64             `json:"records_per_second"`
	RequestsFailed             uint64              `json:"requests_failed"`
	FailureKinds               map[string]uint64   `json:"failure_kinds,omitempty"`
	FailureKindsPerTarget      []map[string]uint64 `json:"failure_kinds_per_target,omitempty"`
	RequestLatency             latencySummary      `json:"request_latency"`
	RequestsPerTarget          []uint64            `json:"requests_per_target"`
	MetricRequestsPerTarget    []uint64            `json:"metric_requests_per_target"`
	LogRequestsPerTarget       []uint64            `json:"log_requests_per_target"`
	FailedRequestsPerTarget    []uint64            `json:"failed_requests_per_target"`
	RequestsPerSecondPerTarget []float64           `json:"requests_per_second_per_target"`
	RecordsPerSecondPerTarget  []float64           `json:"records_per_second_per_target"`
}

type scaleTargetIdentity struct {
	Target     string `json:"target"`
	InstanceID string `json:"instance_id"`
	GOMAXPROCS int    `json:"gomaxprocs"`
}

type scaleProcessIdentity struct {
	InstanceID string
	GOMAXPROCS int
}

type scaleTargetBaseline struct {
	Target     string             `json:"target"`
	InstanceID string             `json:"instance_id"`
	Rounds     []scalePhaseResult `json:"rounds"`
	Median     scalePhaseResult   `json:"median"`
}

type scaleThroughputReference struct {
	SamplesPerSecond float64 `json:"samples_per_second"`
	SampleSource     string  `json:"sample_source"`
	LogsPerSecond    float64 `json:"logs_per_second"`
	LogSource        string  `json:"log_source"`
	RecordsPerSecond float64 `json:"records_per_second"`
}

type scaleUpstreamHeadroomEvidence struct {
	MinimumControlSamplesPerSecond float64 `json:"minimum_control_samples_per_second"`
	MaximumScaledSamplesPerSecond  float64 `json:"maximum_scaled_samples_per_second"`
	SampleRatio                    float64 `json:"sample_ratio"`
	MinimumControlLogsPerSecond    float64 `json:"minimum_control_logs_per_second"`
	MaximumScaledLogsPerSecond     float64 `json:"maximum_scaled_logs_per_second"`
	LogRatio                       float64 `json:"log_ratio"`
	MinimumRequiredRatio           float64 `json:"minimum_required_ratio"`
}

type scaleRoundEfficiency struct {
	Round                   int     `json:"round"`
	SampleReplicaEfficiency float64 `json:"sample_replica_efficiency"`
	LogReplicaEfficiency    float64 `json:"log_replica_efficiency"`
	PairedGatePassed        bool    `json:"paired_gate_passed"`
}

type scaleGateResult struct {
	Version                       int                           `json:"version"`
	GitCommit                     string                        `json:"git_commit,omitempty"`
	GeneratedAt                   string                        `json:"generated_at,omitempty"`
	Scope                         string                        `json:"scope"`
	ScalingModel                  string                        `json:"scaling_model"`
	MeasurementUpstream           string                        `json:"measurement_upstream"`
	ControlledUpstreamIdentity    string                        `json:"controlled_upstream_identity"`
	ReadbackMode                  string                        `json:"readback_mode"`
	Targets                       []string                      `json:"targets"`
	TargetIdentities              []scaleTargetIdentity         `json:"target_identities"`
	ReadbackTargetIdentities      []scaleTargetIdentity         `json:"readback_target_identities"`
	NominalBaselines              []scaleTargetBaseline         `json:"nominal_baselines"`
	HighControlBaselines          []scaleTargetBaseline         `json:"single_high_concurrency_control_baselines"`
	ScaledRounds                  []scalePhaseResult            `json:"scaled_rounds"`
	RoundOrders                   [][]string                    `json:"round_orders"`
	Rounds                        int                           `json:"rounds"`
	SettleSeconds                 float64                       `json:"settle_seconds"`
	RecoverySeconds               float64                       `json:"recovery_seconds"`
	PerReplicaConcurrency         int                           `json:"per_replica_concurrency"`
	SingleNominalTotalConcurrency int                           `json:"single_nominal_total_concurrency"`
	SingleControlTotalConcurrency int                           `json:"single_control_total_concurrency"`
	ScaledTotalConcurrency        int                           `json:"scaled_total_concurrency"`
	BatchSize                     int                           `json:"batch_size"`
	SingleNominal                 scalePhaseResult              `json:"single_nominal"`
	SingleCapacityReference       scaleThroughputReference      `json:"single_capacity_reference"`
	Scaled                        scalePhaseResult              `json:"scaled_replicas"`
	UpstreamControlRounds         []scalePhaseResult            `json:"upstream_control_rounds"`
	UpstreamHeadroom              scaleUpstreamHeadroomEvidence `json:"upstream_headroom"`
	SampleScaleFactor             float64                       `json:"sample_scale_factor"`
	LogScaleFactor                float64                       `json:"log_scale_factor"`
	SampleReplicaEfficiency       float64                       `json:"sample_replica_efficiency"`
	LogReplicaEfficiency          float64                       `json:"log_replica_efficiency"`
	MinReplicaEfficiency          float64                       `json:"min_replica_efficiency"`
	ScaledRoundEfficiencies       []scaleRoundEfficiency        `json:"scaled_round_efficiencies"`
	PassingScaledRounds           int                           `json:"passing_scaled_rounds"`
	RequiredPassingScaledRounds   int                           `json:"required_passing_scaled_rounds"`
	MinPassingRoundRatio          float64                       `json:"min_passing_round_ratio"`
	BaselineSampleRatio           float64                       `json:"baseline_sample_ratio"`
	BaselineLogRatio              float64                       `json:"baseline_log_ratio"`
	HighControlSampleRatio        float64                       `json:"high_control_sample_ratio"`
	HighControlLogRatio           float64                       `json:"high_control_log_ratio"`
	MinBaselineRatio              float64                       `json:"min_baseline_ratio"`
	TargetBalanceLowerBound       float64                       `json:"target_balance_lower_bound"`
	ReadbackVerified              bool                          `json:"readback_verified"`
	PayloadProofVerified          bool                          `json:"payload_proof_verified"`
	GatePassed                    bool                          `json:"gate_passed"`
	GateFailures                  []string                      `json:"gate_failures,omitempty"`
}

type phaseCounters struct {
	metrics atomic.Uint64
	logs    atomic.Uint64
	fails   atomic.Uint64
	kinds   failureKindCounter
}

type phaseTargetCounters struct {
	metricRequests atomic.Uint64
	logRequests    atomic.Uint64
	fails          atomic.Uint64
	kinds          failureKindCounter
}

type failureKindCounter struct {
	mu     sync.Mutex
	values map[string]uint64
}

func (c *failureKindCounter) add(kind string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = make(map[string]uint64)
	}
	c.values[kind]++
}

func (c *failureKindCounter) snapshot() map[string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.values) == 0 {
		return nil
	}
	result := make(map[string]uint64, len(c.values))
	for kind, count := range c.values {
		result[kind] = count
	}
	return result
}

type scalePayload struct {
	metricBody []byte
	logBody    []byte
	marker     string
}

func runScaleGate(args []string) error {
	flags := flag.NewFlagSet("scale", flag.ContinueOnError)
	configPath := flags.String("config", defaultGateConfigPath, "performance gate config")
	targetList := flags.String("targets", "", "comma-separated live lazy-observe replica URLs")
	readbackTargetList := flags.String(
		"readback-targets",
		"",
		"optional comma-separated real-storage gateway URLs used for measured-payload readback",
	)
	controlledUpstreamURL := flags.String(
		"controlled-upstream",
		"",
		"shared validating OTLP sink URL used only to prove measurement headroom",
	)
	tokenEnv := flags.String(
		"token-env",
		"LO_PERF_TOKEN",
		"environment variable containing the project API token used only for ingest POSTs",
	)
	queryTokenEnv := flags.String(
		"query-token-env",
		"LO_PERF_QUERY_TOKEN",
		"environment variable containing a user API token (lou_...) used only for identity and readback GETs",
	)
	project := flags.String("project", defaultReadProject, "project slug sent as "+projectHeader+" on every read")
	output := flags.String("output", "", "optional JSON result path")
	warmupOverride := flags.Duration("warmup", -1, "override warmup duration (must remain positive)")
	durationOverride := flags.Duration("duration", -1, "override measured phase duration")
	rounds := flags.Int("rounds", defaultScaleRounds, "balanced measured rounds (multiple of four, minimum four)")
	readbackTimeout := flags.Duration(
		"readback-timeout",
		defaultReadbackTimeout,
		"maximum time to query back each measured phase marker",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := loadGateConfig(*configPath)
	if err != nil {
		return err
	}
	targets, err := parseTargets(*targetList)
	if err != nil {
		return err
	}
	if len(targets) != 2 {
		return fmt.Errorf("capacity-controlled scaling gate currently requires exactly two replicas")
	}
	if strings.TrimSpace(*readbackTargetList) == "" {
		return fmt.Errorf(
			"--readback-targets is required so capacity measurement remains separate from real-storage verification",
		)
	}
	readbackTargets, err := parseTargets(*readbackTargetList)
	if err != nil {
		return fmt.Errorf("parse readback targets: %w", err)
	}
	if len(readbackTargets) != 2 {
		return fmt.Errorf("real-storage readback requires exactly two replicas")
	}
	if !disjointScaleTargets(targets, readbackTargets) {
		return fmt.Errorf("measurement targets and real-storage readback targets must be disjoint")
	}
	controlledSink, err := validateBaseURL(*controlledUpstreamURL)
	if err != nil {
		return fmt.Errorf("--controlled-upstream is required: %w", err)
	}
	controlledUpstream := true
	token := strings.TrimSpace(os.Getenv(*tokenEnv))
	if token == "" {
		return fmt.Errorf("token environment variable %s is empty", *tokenEnv)
	}
	if strings.HasPrefix(token, userTokenPrefix) {
		return fmt.Errorf(
			"%s must hold the project ingest token; user API tokens are only sent to read endpoints",
			*tokenEnv,
		)
	}
	readAuth, err := loadReadCredential(*queryTokenEnv, *project)
	if err != nil {
		return err
	}
	warmup := time.Duration(cfg.HAScaling.WarmupSeconds * float64(time.Second))
	settle := time.Duration(cfg.HAScaling.SettleSeconds * float64(time.Second))
	recovery := time.Duration(cfg.HAScaling.RecoverySeconds * float64(time.Second))
	duration := time.Duration(cfg.HAScaling.DurationSeconds * float64(time.Second))
	if *warmupOverride >= 0 {
		warmup = *warmupOverride
	}
	if *durationOverride >= 0 {
		duration = *durationOverride
	}
	if warmup <= 0 || duration <= 0 {
		return fmt.Errorf("warmup and measured duration must both be positive")
	}
	phaseCount := len(targets) + 2 // one nominal phase per target, one high-concurrency control, and one scaled phase.
	if !validScaleRoundCount(*rounds, phaseCount) {
		return fmt.Errorf("round count must be a positive multiple of %d for balanced topology ordering", phaseCount)
	}
	if *readbackTimeout <= 0 {
		return fmt.Errorf("readback timeout must be positive")
	}

	concurrencyPerReplica := cfg.HAScaling.ScaledConcurrencyPerReplica
	scaledTotalConcurrency := concurrencyPerReplica * len(targets)
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// Never forward a project or user token to a redirect target. Replica URLs
			// must directly expose the identity, ingest, and query contracts.
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			MaxIdleConns:        scaledTotalConcurrency * 2,
			MaxIdleConnsPerHost: scaledTotalConcurrency * 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}
	defer client.CloseIdleConnections()
	controlledSinkIdentity, err := discoverControlledSinkIdentity(context.Background(), client, controlledSink)
	if err != nil {
		return err
	}

	identities, err := discoverScaleTargetIdentities(
		context.Background(),
		client,
		targets,
		readAuth,
		cfg.HAScaling.GOMAXPROCSPerReplica,
	)
	if err != nil {
		return err
	}
	readbackIdentities, err := discoverScaleTargetIdentities(
		context.Background(),
		client,
		readbackTargets,
		readAuth,
		cfg.HAScaling.GOMAXPROCSPerReplica,
	)
	if err != nil {
		return fmt.Errorf("discover real-storage readback replicas: %w", err)
	}
	if !disjointScaleIdentities(identities, readbackIdentities) {
		return fmt.Errorf("measurement and real-storage readback targets resolve to overlapping process instances")
	}
	labels := scaleTargetLabels(len(targets))
	if settle > 0 {
		time.Sleep(settle)
	}
	nominalBaselines := make([]scaleTargetBaseline, len(targets))
	nominalByTarget := make([][]scalePhaseResult, len(targets))
	highByTarget := make([][]scalePhaseResult, len(targets))
	for targetIndex, target := range targets {
		warmPayload, payloadErr := newScalePayload(cfg.HAScaling.BatchSize)
		if payloadErr != nil {
			return fmt.Errorf("prepare target %d warmup payload: %w", targetIndex+1, payloadErr)
		}
		warmResult, warmErr := measureScalePhase(
			context.Background(),
			client,
			[]string{target},
			token,
			controlledSinkIdentity,
			warmPayload.metricBody,
			warmPayload.logBody,
			cfg.HAScaling.BatchSize,
			scaledTotalConcurrency,
			warmup,
		)
		if warmErr != nil {
			return fmt.Errorf("target %d warmup: %w", targetIndex+1, warmErr)
		}
		if warmResult.RequestsFailed > cfg.HAScaling.MaxFailedRequests {
			return fmt.Errorf(
				"target %d warmup had %d failed requests (%s)",
				targetIndex+1,
				warmResult.RequestsFailed,
				formatFailureKinds(warmResult.FailureKinds),
			)
		}
		waitScaleRecovery(recovery)
		nominalByTarget[targetIndex] = make([]scalePhaseResult, 0, *rounds)
		highByTarget[targetIndex] = make([]scalePhaseResult, 0, *rounds/len(targets))
	}
	warmPayload, err := newScalePayload(cfg.HAScaling.BatchSize)
	if err != nil {
		return fmt.Errorf("prepare scaled warmup payload: %w", err)
	}
	warmScaled, err := measureScalePhase(
		context.Background(),
		client,
		targets,
		token,
		controlledSinkIdentity,
		warmPayload.metricBody,
		warmPayload.logBody,
		cfg.HAScaling.BatchSize,
		concurrencyPerReplica,
		warmup,
	)
	if err != nil {
		return fmt.Errorf("scaled warmup: %w", err)
	}
	if warmScaled.RequestsFailed > cfg.HAScaling.MaxFailedRequests {
		return fmt.Errorf(
			"scaled warmup had %d failed requests (%s)",
			warmScaled.RequestsFailed,
			formatFailureKinds(warmScaled.FailureKinds),
		)
	}
	waitScaleRecovery(recovery)

	upstreamControlRounds := make([]scalePhaseResult, 0, 2)
	measureUpstreamControl := func(position string) error {
		if !controlledUpstream {
			return nil
		}
		payload, payloadErr := newScalePayload(cfg.HAScaling.BatchSize)
		if payloadErr != nil {
			return fmt.Errorf("prepare %s upstream control payload: %w", position, payloadErr)
		}
		phase, phaseErr := measureScalePhase(
			context.Background(),
			client,
			[]string{controlledSink},
			"",
			controlledSinkIdentity,
			payload.metricBody,
			payload.logBody,
			cfg.HAScaling.BatchSize,
			scaledTotalConcurrency,
			duration,
		)
		if phaseErr != nil {
			return fmt.Errorf("%s upstream control: %w", position, phaseErr)
		}
		if phase.RequestsFailed > cfg.HAScaling.MaxFailedRequests {
			return fmt.Errorf(
				"%s upstream control had %d failed requests (%s)",
				position,
				phase.RequestsFailed,
				formatFailureKinds(phase.FailureKinds),
			)
		}
		upstreamControlRounds = append(upstreamControlRounds, phase)
		waitScaleRecovery(recovery)
		return nil
	}
	if controlledUpstream {
		payload, payloadErr := newScalePayload(cfg.HAScaling.BatchSize)
		if payloadErr != nil {
			return fmt.Errorf("prepare upstream warmup payload: %w", payloadErr)
		}
		warmResult, warmErr := measureScalePhase(
			context.Background(),
			client,
			[]string{controlledSink},
			"",
			controlledSinkIdentity,
			payload.metricBody,
			payload.logBody,
			cfg.HAScaling.BatchSize,
			scaledTotalConcurrency,
			warmup,
		)
		if warmErr != nil {
			return fmt.Errorf(
				"controlled upstream warmup failed: err=%w failures=%d (%s)",
				warmErr,
				warmResult.RequestsFailed,
				formatFailureKinds(warmResult.FailureKinds),
			)
		}
		if warmResult.RequestsFailed > cfg.HAScaling.MaxFailedRequests {
			return fmt.Errorf(
				"controlled upstream warmup failed: err=<nil> failures=%d (%s)",
				warmResult.RequestsFailed,
				formatFailureKinds(warmResult.FailureKinds),
			)
		}
		waitScaleRecovery(recovery)
		if err := measureUpstreamControl("leading"); err != nil {
			return err
		}
	}

	scaledRounds := make([]scalePhaseResult, 0, *rounds)
	roundOrders := make([][]string, 0, *rounds)
	for round := 0; round < *rounds; round++ {
		highTargetIndex := round % len(targets)
		order := balancedScalePhaseOrder(phaseCount, round)
		orderNames := make([]string, 0, len(order))
		for _, phaseKind := range order {
			payload, payloadErr := newScalePayload(cfg.HAScaling.BatchSize)
			if payloadErr != nil {
				return fmt.Errorf("prepare round %d payload: %w", round+1, payloadErr)
			}
			var phase scalePhaseResult
			var phaseErr error
			phaseName := ""
			switch {
			case phaseKind < len(targets):
				targetIndex := phaseKind
				phaseName = labels[targetIndex] + "-nominal"
				phase, phaseErr = measureScalePhase(
					context.Background(),
					client,
					[]string{targets[targetIndex]},
					token,
					controlledSinkIdentity,
					payload.metricBody,
					payload.logBody,
					cfg.HAScaling.BatchSize,
					concurrencyPerReplica,
					duration,
				)
				if phaseErr == nil {
					nominalByTarget[targetIndex] = append(nominalByTarget[targetIndex], phase)
				}
			case phaseKind == len(targets):
				phaseName = labels[highTargetIndex] + "-high-control"
				phase, phaseErr = measureScalePhase(
					context.Background(),
					client,
					[]string{targets[highTargetIndex]},
					token,
					controlledSinkIdentity,
					payload.metricBody,
					payload.logBody,
					cfg.HAScaling.BatchSize,
					scaledTotalConcurrency,
					duration,
				)
				if phaseErr == nil {
					highByTarget[highTargetIndex] = append(highByTarget[highTargetIndex], phase)
				}
			default:
				phaseName = "scaled"
				phase, phaseErr = measureScalePhase(
					context.Background(),
					client,
					targets,
					token,
					controlledSinkIdentity,
					payload.metricBody,
					payload.logBody,
					cfg.HAScaling.BatchSize,
					concurrencyPerReplica,
					duration,
				)
				if phaseErr == nil {
					scaledRounds = append(scaledRounds, phase)
				}
			}
			if phaseErr != nil {
				return fmt.Errorf("round %d %s phase: %w", round+1, phaseName, phaseErr)
			}
			if readbackErr := verifyScalePayloadReadback(
				client,
				readbackTargets,
				token,
				readAuth,
				payload,
				cfg.HAScaling.BatchSize,
				controlledUpstream,
				*readbackTimeout,
			); readbackErr != nil {
				return fmt.Errorf("round %d %s readback: %w", round+1, phaseName, readbackErr)
			}
			orderNames = append(orderNames, phaseName)
			waitScaleRecovery(recovery)
		}
		roundOrders = append(roundOrders, orderNames)
	}
	if err := measureUpstreamControl("trailing"); err != nil {
		return err
	}

	highControlBaselines := make([]scaleTargetBaseline, len(targets))
	for targetIndex := range targets {
		measured := nominalByTarget[targetIndex]
		nominalBaselines[targetIndex] = scaleTargetBaseline{
			Target: labels[targetIndex], InstanceID: identities[targetIndex].InstanceID,
			Rounds: measured, Median: medianScalePhase(measured),
		}
		highMeasured := highByTarget[targetIndex]
		highControlBaselines[targetIndex] = scaleTargetBaseline{
			Target: labels[targetIndex], InstanceID: identities[targetIndex].InstanceID,
			Rounds: highMeasured, Median: medianScalePhase(highMeasured),
		}
	}
	singleNominal := representativeBaseline(nominalBaselines)
	singleHighControl := representativeBaseline(highControlBaselines)
	singleCapacityReference := scaleCapacityReference(nominalBaselines, highControlBaselines)

	scaled := medianScalePhase(scaledRounds)
	result := evaluateCapacityScaling(
		labels,
		singleNominal,
		singleHighControl,
		singleCapacityReference,
		scaled,
		concurrencyPerReplica,
		cfg.HAScaling.MinReplicaEfficiency,
	)
	result.Version = 6
	result.GitCommit, result.GeneratedAt = resultMetadata()
	result.Scope = "stateless_platform_gateway"
	result.ScalingModel = "weak_scaling_with_single_high_concurrency_control"
	result.MeasurementUpstream = "controlled_validating_otlp_sink"
	result.ControlledUpstreamIdentity = controlledSinkIdentity
	result.ReadbackMode = "measured_payload_mirrored_to_real_signal_storage"
	result.Targets = labels
	result.Rounds = *rounds
	result.SettleSeconds = settle.Seconds()
	result.RecoverySeconds = recovery.Seconds()
	result.PerReplicaConcurrency = concurrencyPerReplica
	result.SingleNominalTotalConcurrency = concurrencyPerReplica
	result.SingleControlTotalConcurrency = scaledTotalConcurrency
	result.ScaledTotalConcurrency = scaledTotalConcurrency
	result.BatchSize = cfg.HAScaling.BatchSize
	result.NominalBaselines = nominalBaselines
	result.HighControlBaselines = highControlBaselines
	result.ScaledRounds = scaledRounds
	result.RoundOrders = roundOrders
	result.TargetBalanceLowerBound = cfg.HAScaling.MinTargetBalanceRatio
	result.MinBaselineRatio = cfg.HAScaling.MinBaselineRatio
	result.MinPassingRoundRatio = cfg.HAScaling.MinPassingRoundRatio
	result.BaselineSampleRatio, result.BaselineLogRatio = baselineThroughputRatios(nominalBaselines)
	result.HighControlSampleRatio, result.HighControlLogRatio = baselineThroughputRatios(highControlBaselines)
	result.ReadbackVerified = true
	result.PayloadProofVerified = true
	result.UpstreamControlRounds = upstreamControlRounds
	result.UpstreamHeadroom = conservativeUpstreamHeadroom(
		upstreamControlRounds,
		scaledRounds,
		cfg.HAScaling.MinUpstreamHeadroomRatio,
	)
	applyScaleRoundEfficiencyGate(&result, scaledRounds)
	for index := range labels {
		result.TargetIdentities = append(result.TargetIdentities, scaleTargetIdentity{
			Target: labels[index], InstanceID: identities[index].InstanceID, GOMAXPROCS: identities[index].GOMAXPROCS,
		})
		result.ReadbackTargetIdentities = append(result.ReadbackTargetIdentities, scaleTargetIdentity{
			Target: fmt.Sprintf(
				"readback-%d",
				index+1,
			),
			InstanceID: readbackIdentities[index].InstanceID,
			GOMAXPROCS: readbackIdentities[index].GOMAXPROCS,
		})
	}
	for targetIndex, baseline := range nominalBaselines {
		for roundIndex, phase := range baseline.Rounds {
			if phase.RequestsFailed > cfg.HAScaling.MaxFailedRequests {
				appendScaleFailure(
					&result,
					fmt.Sprintf(
						"target %d baseline round %d had %d failed requests (max %d; %s)",
						targetIndex+1,
						roundIndex+1,
						phase.RequestsFailed,
						cfg.HAScaling.MaxFailedRequests,
						formatFailureKinds(phase.FailureKinds),
					),
				)
			}
		}
	}
	for targetIndex, baseline := range highControlBaselines {
		for roundIndex, phase := range baseline.Rounds {
			if phase.RequestsFailed > cfg.HAScaling.MaxFailedRequests {
				appendScaleFailure(
					&result,
					fmt.Sprintf(
						"target %d high-concurrency control round %d had %d failed requests (max %d; %s)",
						targetIndex+1,
						roundIndex+1,
						phase.RequestsFailed,
						cfg.HAScaling.MaxFailedRequests,
						formatFailureKinds(phase.FailureKinds),
					),
				)
			}
		}
	}
	if result.BaselineSampleRatio < result.MinBaselineRatio {
		appendScaleFailure(
			&result,
			fmt.Sprintf(
				"single-replica metric baselines ratio %.3f is below %.3f",
				result.BaselineSampleRatio,
				result.MinBaselineRatio,
			),
		)
	}
	if result.BaselineLogRatio < result.MinBaselineRatio {
		appendScaleFailure(
			&result,
			fmt.Sprintf(
				"single-replica log baselines ratio %.3f is below %.3f",
				result.BaselineLogRatio,
				result.MinBaselineRatio,
			),
		)
	}
	if result.HighControlSampleRatio < result.MinBaselineRatio {
		appendScaleFailure(
			&result,
			fmt.Sprintf(
				"single high-concurrency metric controls ratio %.3f is below %.3f",
				result.HighControlSampleRatio,
				result.MinBaselineRatio,
			),
		)
	}
	if result.HighControlLogRatio < result.MinBaselineRatio {
		appendScaleFailure(
			&result,
			fmt.Sprintf(
				"single high-concurrency log controls ratio %.3f is below %.3f",
				result.HighControlLogRatio,
				result.MinBaselineRatio,
			),
		)
	}
	for roundIndex, phase := range scaledRounds {
		if phase.RequestsFailed > cfg.HAScaling.MaxFailedRequests {
			appendScaleFailure(
				&result,
				fmt.Sprintf(
					"scaled round %d had %d failed requests (max %d; %s)",
					roundIndex+1,
					phase.RequestsFailed,
					cfg.HAScaling.MaxFailedRequests,
					formatFailureKinds(phase.FailureKinds),
				),
			)
		}
		for _, failure := range scaleTargetBalanceFailures(phase, cfg.HAScaling.MinTargetBalanceRatio) {
			appendScaleFailure(&result, fmt.Sprintf("scaled round %d: %s", roundIndex+1, failure))
		}
	}
	if result.UpstreamHeadroom.SampleRatio < result.UpstreamHeadroom.MinimumRequiredRatio {
		appendScaleFailure(
			&result,
			fmt.Sprintf(
				"metric upstream headroom %.3f is below %.3f",
				result.UpstreamHeadroom.SampleRatio,
				result.UpstreamHeadroom.MinimumRequiredRatio,
			),
		)
	}
	if result.UpstreamHeadroom.LogRatio < result.UpstreamHeadroom.MinimumRequiredRatio {
		appendScaleFailure(
			&result,
			fmt.Sprintf(
				"log upstream headroom %.3f is below %.3f",
				result.UpstreamHeadroom.LogRatio,
				result.UpstreamHeadroom.MinimumRequiredRatio,
			),
		)
	}
	result.GatePassed = len(result.GateFailures) == 0
	if err := writeResult(*output, result); err != nil {
		return err
	}
	if !result.GatePassed {
		return failResult(strings.Join(result.GateFailures, "; "))
	}
	fmt.Printf(
		"PASS: %d equivalent platform replicas achieved aggregate median metric/log capacity efficiency %.3f/%.3f; %d/%d paired rounds met >= %.3f for both signals (required %d at ratio %.3f), with baseline ratios %.3f/%.3f (>= %.3f), concurrency %d per replica, and batch %d; %.0f -> %.0f records/s through %s; upstream headroom %.2fx/%.2fx and real-storage mirrored payload readback verified\n",
		len(targets),
		result.SampleReplicaEfficiency,
		result.LogReplicaEfficiency,
		result.PassingScaledRounds,
		len(result.ScaledRoundEfficiencies),
		result.MinReplicaEfficiency,
		result.RequiredPassingScaledRounds,
		result.MinPassingRoundRatio,
		result.BaselineSampleRatio,
		result.BaselineLogRatio,
		result.MinBaselineRatio,
		result.PerReplicaConcurrency,
		result.BatchSize,
		result.SingleCapacityReference.RecordsPerSecond,
		result.Scaled.RecordsPerSecond,
		result.MeasurementUpstream,
		result.UpstreamHeadroom.SampleRatio,
		result.UpstreamHeadroom.LogRatio,
	)
	return nil
}

func disjointScaleTargets(left, right []string) bool {
	seen := make(map[string]struct{}, len(left))
	for _, target := range left {
		seen[target] = struct{}{}
	}
	for _, target := range right {
		if _, exists := seen[target]; exists {
			return false
		}
	}
	return true
}

func disjointScaleIdentities(left, right []scaleProcessIdentity) bool {
	seen := make(map[string]struct{}, len(left))
	for _, identity := range left {
		seen[identity.InstanceID] = struct{}{}
	}
	for _, identity := range right {
		if _, exists := seen[identity.InstanceID]; exists {
			return false
		}
	}
	return true
}

func balancedScalePhaseOrder(phases, round int) []int {
	if phases < 2 || phases%2 != 0 {
		return nil
	}
	// Williams' balanced Latin square puts every phase in every temporal
	// position once and balances pairwise before/after order for even counts.
	base := make([]int, 0, phases)
	base = append(base, 0)
	for low, high := 1, phases-1; len(base) < phases; low, high = low+1, high-1 {
		base = append(base, low)
		if len(base) < phases {
			base = append(base, high)
		}
	}
	order := make([]int, phases)
	for index, phase := range base {
		order[index] = (phase + round) % phases
	}
	return order
}

func validScaleRoundCount(rounds, phases int) bool {
	return phases >= 2 && phases%2 == 0 && rounds >= phases && rounds%phases == 0
}

func waitScaleRecovery(duration time.Duration) {
	if duration > 0 {
		time.Sleep(duration)
	}
}

func formatFailureKinds(kinds map[string]uint64) string {
	if len(kinds) == 0 {
		return "unclassified"
	}
	keys := make([]string, 0, len(kinds))
	for kind := range kinds {
		keys = append(keys, kind)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, kind := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", kind, kinds[kind]))
	}
	return strings.Join(parts, ",")
}

func parseTargets(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	targets := make([]string, 0, len(parts))
	seen := make(map[string]bool)
	for inputIndex, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		target, err := validateBaseURL(part)
		if err != nil {
			return nil, fmt.Errorf("invalid target %d: %w", inputIndex+1, err)
		}
		if seen[target] {
			return nil, fmt.Errorf("target %d duplicates an earlier target", inputIndex+1)
		}
		seen[target] = true
		targets = append(targets, target)
	}
	if len(targets) < 2 {
		return nil, fmt.Errorf("at least two distinct targets are required for a horizontal scaling gate")
	}
	return targets, nil
}

func scaleTargetLabels(count int) []string {
	labels := make([]string, count)
	for index := range labels {
		labels[index] = fmt.Sprintf("target-%d", index+1)
	}
	return labels
}

func discoverControlledSinkIdentity(ctx context.Context, client *http.Client, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/health", nil)
	if err != nil {
		return "", fmt.Errorf("build controlled upstream identity request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("read controlled upstream identity: request failed")
	}
	_, readErr := readBoundedResponse(resp.Body, otlpResponseBodyLimit)
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil || resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("read controlled upstream identity: invalid health response")
	}
	identity := strings.TrimSpace(resp.Header.Get(scaleSinkIdentityHeader))
	if !validControlledSinkIdentity(identity) {
		return "", fmt.Errorf("read controlled upstream identity: invalid %s header", scaleSinkIdentityHeader)
	}
	return identity, nil
}

func validControlledSinkIdentity(identity string) bool {
	if !strings.HasPrefix(identity, "losink_") || len(identity) < 15 || len(identity) > 96 {
		return false
	}
	for _, char := range identity {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func discoverScaleTargetIdentities(
	ctx context.Context,
	client *http.Client,
	targets []string,
	auth readCredential,
	expectedGOMAXPROCS int,
) ([]scaleProcessIdentity, error) {
	identities := make([]scaleProcessIdentity, len(targets))
	seen := make(map[string]int, len(targets))
	for targetIndex, target := range targets {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+scaleInstanceIdentityPath, nil)
		if err != nil {
			return nil, fmt.Errorf("build identity request for target %d", targetIndex+1)
		}
		auth.apply(req)
		req.Header.Set("Cache-Control", "no-cache")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("read identity for target %d: request failed", targetIndex+1)
		}
		body, readErr := readBoundedResponse(resp.Body, otlpResponseBodyLimit)
		closeErr := resp.Body.Close()
		if readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("read identity for target %d: invalid response", targetIndex+1)
		}
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("read identity for target %d: HTTP %d", targetIndex+1, resp.StatusCode)
		}
		var info struct {
			InstanceID string `json:"instance_id"`
			GOMAXPROCS int    `json:"gomaxprocs"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&info); err != nil {
			return nil, fmt.Errorf("read identity for target %d: invalid JSON contract", targetIndex+1)
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return nil, fmt.Errorf("read identity for target %d: invalid JSON contract", targetIndex+1)
		}
		info.InstanceID = strings.TrimSpace(info.InstanceID)
		if !validScaleInstanceID(info.InstanceID) {
			return nil, fmt.Errorf("read identity for target %d: invalid instance_id", targetIndex+1)
		}
		if info.GOMAXPROCS != expectedGOMAXPROCS {
			return nil, fmt.Errorf(
				"read identity for target %d: gomaxprocs=%d, want fixed per-replica budget %d",
				targetIndex+1,
				info.GOMAXPROCS,
				expectedGOMAXPROCS,
			)
		}
		if first, exists := seen[info.InstanceID]; exists {
			return nil, fmt.Errorf(
				"target %d and target %d resolve to the same process instance",
				first+1,
				targetIndex+1,
			)
		}
		seen[info.InstanceID] = targetIndex
		identities[targetIndex] = scaleProcessIdentity{InstanceID: info.InstanceID, GOMAXPROCS: info.GOMAXPROCS}
	}
	return identities, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return err
}

func validScaleInstanceID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func newScalePayload(batchSize int) (scalePayload, error) {
	markerBytes := make([]byte, 12)
	if _, err := rand.Read(markerBytes); err != nil {
		return scalePayload{}, fmt.Errorf("generate marker: %w", err)
	}
	marker := hex.EncodeToString(markerBytes)
	metricBody, err := metricsLoadPayload(batchSize)
	if err != nil {
		return scalePayload{}, fmt.Errorf("encode metrics payload: %w", err)
	}
	logBody, err := logsLoadPayload(batchSize)
	if err != nil {
		return scalePayload{}, fmt.Errorf("encode logs payload: %w", err)
	}
	var metricsRequest metricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(metricBody, &metricsRequest); err != nil {
		return scalePayload{}, fmt.Errorf("decode generated metrics payload: %w", err)
	}
	for _, resourceMetrics := range metricsRequest.ResourceMetrics {
		for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
			for _, metric := range scopeMetrics.Metrics {
				for _, point := range metric.GetGauge().DataPoints {
					point.Attributes = append(point.Attributes, commonKeyValue(scaleMarkerAttribute, marker))
				}
			}
		}
	}
	metricBody, err = proto.Marshal(&metricsRequest)
	if err != nil {
		return scalePayload{}, fmt.Errorf("encode tagged metrics payload: %w", err)
	}
	var logsRequest logspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(logBody, &logsRequest); err != nil {
		return scalePayload{}, fmt.Errorf("decode generated logs payload: %w", err)
	}
	for _, resourceLogs := range logsRequest.ResourceLogs {
		for _, scopeLogs := range resourceLogs.ScopeLogs {
			for _, record := range scopeLogs.LogRecords {
				record.Body = stringAnyValue(scaleLogMessagePrefix + marker)
			}
		}
	}
	logBody, err = proto.Marshal(&logsRequest)
	if err != nil {
		return scalePayload{}, fmt.Errorf("encode tagged logs payload: %w", err)
	}
	return scalePayload{metricBody: metricBody, logBody: logBody, marker: marker}, nil
}

func commonKeyValue(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: stringAnyValue(value)}
}

func measureScalePhase(
	ctx context.Context,
	client *http.Client,
	targets []string,
	token, expectedSinkIdentity string,
	metricsBody, logsBody []byte,
	batchSize, concurrencyPerReplica int,
	duration time.Duration,
) (scalePhaseResult, error) {
	if duration <= 0 {
		return scalePhaseResult{}, fmt.Errorf("phase duration must be positive")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	requestNonce := make([]byte, 12)
	if _, err := rand.Read(requestNonce); err != nil {
		return scalePhaseResult{}, fmt.Errorf("generate phase request nonce: %w", err)
	}
	requestPrefix := "loreq_" + hex.EncodeToString(requestNonce) + "_"
	metricReceipt := fmt.Sprintf("%x", sha256.Sum256(metricsBody))
	logReceipt := fmt.Sprintf("%x", sha256.Sum256(logsBody))
	var requestSequence atomic.Uint64
	var counters phaseCounters
	perTarget := make([]phaseTargetCounters, len(targets))
	latencies := make([]time.Duration, 0, 1<<16)
	var latencyMu sync.Mutex
	var workers sync.WaitGroup
	var ready sync.WaitGroup
	ready.Add(len(targets) * concurrencyPerReplica)
	startWorkers := make(chan struct{})
	for targetIndex, target := range targets {
		for localWorker := 0; localWorker < concurrencyPerReplica; localWorker++ {
			isMetrics := localWorker%2 == 0
			workers.Add(1)
			go func(targetIndex int, target string, isMetrics bool) {
				defer workers.Done()
				ready.Done()
				select {
				case <-startWorkers:
				case <-ctx.Done():
					return
				}
				path, body := "/v1/logs", logsBody
				if isMetrics {
					path, body = "/v1/metrics", metricsBody
				}
				for ctx.Err() == nil {
					requestStarted := time.Now()
					requestID := requestPrefix + strconv.FormatUint(requestSequence.Add(1), 10)
					status, sinkIdentity, receipt, echoedRequestID, requestErr := executeScaleExport(
						ctx,
						client,
						target+path,
						token,
						requestID,
						body,
						isMetrics,
					)
					if requestErr == nil && status/100 == 2 && sinkIdentity != expectedSinkIdentity {
						requestErr = fmt.Errorf("controlled upstream identity mismatch")
					}
					expectedReceipt := logReceipt
					if isMetrics {
						expectedReceipt = metricReceipt
					}
					if requestErr == nil && status/100 == 2 && receipt != expectedReceipt {
						requestErr = fmt.Errorf("controlled upstream payload receipt mismatch")
					}
					if requestErr == nil && status/100 == 2 && echoedRequestID != requestID {
						requestErr = fmt.Errorf("controlled upstream request proof mismatch")
					}
					if ctx.Err() != nil {
						return
					}
					latencyMu.Lock()
					latencies = append(latencies, time.Since(requestStarted))
					latencyMu.Unlock()
					if requestErr != nil || status/100 != 2 {
						kind := scaleFailureKind(status, requestErr)
						counters.fails.Add(1)
						counters.kinds.add(kind)
						perTarget[targetIndex].fails.Add(1)
						perTarget[targetIndex].kinds.add(kind)
						continue
					}
					if isMetrics {
						counters.metrics.Add(uint64(batchSize))
						perTarget[targetIndex].metricRequests.Add(1)
					} else {
						counters.logs.Add(uint64(batchSize))
						perTarget[targetIndex].logRequests.Add(1)
					}
				}
			}(targetIndex, target, isMetrics)
		}
	}
	ready.Wait()
	if err := ctx.Err(); err != nil {
		close(startWorkers)
		workers.Wait()
		return scalePhaseResult{}, fmt.Errorf("phase context canceled before measurement: %w", err)
	}
	started := time.Now()
	timer := time.AfterFunc(duration, cancel)
	close(startWorkers)
	workers.Wait()
	timer.Stop()
	elapsed := time.Since(started).Seconds()
	if elapsed <= 0 {
		return scalePhaseResult{}, fmt.Errorf("phase duration was not positive")
	}
	requestsPerTarget := make([]uint64, len(targets))
	metricRequestsPerTarget := make([]uint64, len(targets))
	logRequestsPerTarget := make([]uint64, len(targets))
	failedRequestsPerTarget := make([]uint64, len(targets))
	failureKindsPerTarget := make([]map[string]uint64, len(targets))
	requestsPerSecondPerTarget := make([]float64, len(targets))
	recordsPerSecondPerTarget := make([]float64, len(targets))
	for index := range perTarget {
		metricRequestsPerTarget[index] = perTarget[index].metricRequests.Load()
		logRequestsPerTarget[index] = perTarget[index].logRequests.Load()
		failedRequestsPerTarget[index] = perTarget[index].fails.Load()
		failureKindsPerTarget[index] = perTarget[index].kinds.snapshot()
		requestsPerTarget[index] = metricRequestsPerTarget[index] + logRequestsPerTarget[index]
		requestsPerSecondPerTarget[index] = float64(requestsPerTarget[index]) / elapsed
		recordsPerSecondPerTarget[index] = float64(requestsPerTarget[index]*uint64(batchSize)) / elapsed
	}
	metricSamples, logRecords := counters.metrics.Load(), counters.logs.Load()
	return scalePhaseResult{
		Replicas: len(targets), DurationSeconds: elapsed,
		Concurrency:   len(targets) * concurrencyPerReplica,
		MetricSamples: metricSamples, LogRecords: logRecords,
		SamplesPerSecond: float64(metricSamples) / elapsed,
		LogsPerSecond:    float64(logRecords) / elapsed,
		RecordsPerSecond: float64(metricSamples+logRecords) / elapsed,
		RequestsFailed:   counters.fails.Load(), FailureKinds: counters.kinds.snapshot(),
		FailureKindsPerTarget: failureKindsPerTarget, RequestLatency: summarizeLatencies(latencies),
		RequestsPerTarget: requestsPerTarget, MetricRequestsPerTarget: metricRequestsPerTarget,
		LogRequestsPerTarget: logRequestsPerTarget, FailedRequestsPerTarget: failedRequestsPerTarget,
		RequestsPerSecondPerTarget: requestsPerSecondPerTarget, RecordsPerSecondPerTarget: recordsPerSecondPerTarget,
	}, nil
}

func scaleFailureKind(status int, err error) string {
	if status > 0 && status/100 != 2 {
		return fmt.Sprintf("http_%d", status)
	}
	if err == nil {
		return "unknown"
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "partial success"):
		return "otlp_partial_success"
	case strings.Contains(message, "invalid OTLP"):
		return "invalid_otlp_response"
	case strings.Contains(message, "response body exceeds"):
		return "response_body_limit"
	case strings.Contains(message, "context deadline exceeded"):
		return "request_timeout"
	case strings.Contains(message, "upstream identity"):
		return "upstream_identity_mismatch"
	case strings.Contains(message, "payload receipt"):
		return "upstream_receipt_mismatch"
	case strings.Contains(message, "request proof"):
		return "upstream_request_proof_mismatch"
	default:
		return "transport_or_io"
	}
}

func executeScaleExport(
	ctx context.Context,
	client *http.Client,
	target, token, requestID string,
	body []byte,
	isMetrics bool,
) (int, string, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, "", "", "", err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if requestID != "" {
		req.Header.Set(scaleSinkRequestHeader, requestID)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", "", "", err
	}
	sinkIdentity := strings.TrimSpace(resp.Header.Get(scaleSinkIdentityHeader))
	receipt := strings.TrimSpace(resp.Header.Get(scaleSinkReceiptHeader))
	echoedRequestID := strings.TrimSpace(resp.Header.Get(scaleSinkRequestHeader))
	responseBody, readErr := readBoundedResponse(resp.Body, otlpResponseBodyLimit)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return resp.StatusCode, sinkIdentity, receipt, echoedRequestID, readErr
	}
	if closeErr != nil {
		return resp.StatusCode, sinkIdentity, receipt, echoedRequestID, closeErr
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, sinkIdentity, receipt, echoedRequestID, nil
	}
	if err := validateOTLPExportResponse(responseBody, isMetrics); err != nil {
		return resp.StatusCode, sinkIdentity, receipt, echoedRequestID, err
	}
	return resp.StatusCode, sinkIdentity, receipt, echoedRequestID, nil
}

func readBoundedResponse(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response body exceeds limit")
	}
	return data, nil
}

func validateOTLPExportResponse(body []byte, isMetrics bool) error {
	if isMetrics {
		var response metricspb.ExportMetricsServiceResponse
		if err := proto.Unmarshal(body, &response); err != nil {
			return fmt.Errorf("invalid OTLP metrics response")
		}
		partial := response.GetPartialSuccess()
		if partial != nil &&
			(partial.GetRejectedDataPoints() != 0 || strings.TrimSpace(partial.GetErrorMessage()) != "") {
			return fmt.Errorf("OTLP metrics partial success rejected %d data points", partial.GetRejectedDataPoints())
		}
		return nil
	}
	var response logspb.ExportLogsServiceResponse
	if err := proto.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("invalid OTLP logs response")
	}
	partial := response.GetPartialSuccess()
	if partial != nil && (partial.GetRejectedLogRecords() != 0 || strings.TrimSpace(partial.GetErrorMessage()) != "") {
		return fmt.Errorf("OTLP logs partial success rejected %d log records", partial.GetRejectedLogRecords())
	}
	return nil
}

func verifyScalePayloadReadback(
	client *http.Client,
	targets []string,
	ingestToken string,
	readAuth readCredential,
	payload scalePayload,
	batchSize int,
	mirrorToRealStorage bool,
	timeout time.Duration,
) error {
	if mirrorToRealStorage {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		for _, export := range []struct {
			path      string
			body      []byte
			isMetrics bool
		}{
			{path: "/v1/metrics", body: payload.metricBody, isMetrics: true},
			{path: "/v1/logs", body: payload.logBody},
		} {
			status, _, _, _, err := executeScaleExport(
				ctx,
				client,
				targets[0]+export.path,
				ingestToken,
				"",
				export.body,
				export.isMetrics,
			)
			if err != nil {
				return fmt.Errorf(
					"mirror measured %s payload to real storage: %w",
					strings.TrimPrefix(export.path, "/v1/"),
					err,
				)
			}
			if status/100 != 2 {
				return fmt.Errorf(
					"mirror measured %s payload to real storage: HTTP %d",
					strings.TrimPrefix(export.path, "/v1/"),
					status,
				)
			}
		}
	}
	return verifyScaleReadback(client, targets, readAuth, payload.marker, batchSize, timeout)
}

func verifyScaleReadback(
	client *http.Client,
	targets []string,
	auth readCredential,
	marker string,
	batchSize int,
	timeout time.Duration,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for targetIndex, target := range targets {
		for {
			metricFound, logFound := queryScaleMarker(ctx, client, target, auth, marker, batchSize)
			if metricFound && logFound {
				break
			}
			if ctx.Err() != nil {
				return fmt.Errorf("target %d did not expose the metric and log markers before timeout", targetIndex+1)
			}
			timer := time.NewTimer(readbackRetryInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("target %d did not expose the metric and log markers before timeout", targetIndex+1)
			case <-timer.C:
			}
		}
	}
	return nil
}

func queryScaleMarker(
	ctx context.Context,
	client *http.Client,
	target string,
	auth readCredential,
	marker string,
	batchSize int,
) (bool, bool) {
	metricQuery := fmt.Sprintf(`count(%s{%s=%q})`, scaleMetricName, scaleMarkerAttribute, marker)
	metricURL := target + "/api/v1/query?" + url.Values{"query": {metricQuery}, "nocache": {"1"}}.Encode()
	metricBody, metricOK := executeScaleQuery(ctx, client, metricURL, auth)
	metricFound := metricOK && validScaleMetricReadback(metricBody, batchSize)
	logMessage := scaleLogMessagePrefix + marker
	logURL := target + "/api/v1/logs/query?" + url.Values{
		"query": {`_msg:="` + logMessage + `"`},
		"limit": {strconv.Itoa(batchSize)},
	}.Encode()
	logBody, logOK := executeScaleQuery(ctx, client, logURL, auth)
	return metricFound, logOK && validScaleLogReadback(logBody, logMessage, batchSize)
}

func validScaleMetricReadback(body []byte, batchSize int) bool {
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if decodeSingleJSON(body, &response) != nil || response.Status != "success" ||
		response.Data.ResultType != "vector" {
		return false
	}
	for _, row := range response.Data.Result {
		if len(row.Value) != 2 {
			continue
		}
		var text string
		if json.Unmarshal(row.Value[1], &text) != nil {
			var number json.Number
			if json.Unmarshal(row.Value[1], &number) != nil {
				continue
			}
			text = number.String()
		}
		value, err := strconv.ParseFloat(text, 64)
		if err == nil && value == float64(batchSize) && !math.IsInf(value, 0) && !math.IsNaN(value) {
			return true
		}
	}
	return false
}

func validScaleLogReadback(body []byte, expectedMessage string, batchSize int) bool {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), queryResponseBodyLimit)
	sequences := make(map[int]struct{}, batchSize)
	rows := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row map[string]json.RawMessage
		if decodeSingleJSON(line, &row) != nil || row == nil {
			return false
		}
		var message, sequenceText string
		if json.Unmarshal(row["_msg"], &message) != nil || message != expectedMessage ||
			json.Unmarshal(row["sequence"], &sequenceText) != nil || !validLogTimestamp(row["_time"]) {
			return false
		}
		sequence, err := strconv.Atoi(sequenceText)
		if err != nil || sequence < 0 || sequence >= batchSize {
			return false
		}
		sequences[sequence] = struct{}{}
		rows++
	}
	return scanner.Err() == nil && rows == batchSize && len(sequences) == batchSize
}

func executeScaleQuery(ctx context.Context, client *http.Client, target string, auth readCredential) ([]byte, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false
	}
	auth.apply(req)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	body, readErr := readBoundedResponse(resp.Body, queryResponseBodyLimit)
	closeErr := resp.Body.Close()
	return body, readErr == nil && closeErr == nil && resp.StatusCode/100 == 2
}

func medianScalePhase(rounds []scalePhaseResult) scalePhaseResult {
	if len(rounds) == 0 {
		return scalePhaseResult{}
	}
	result := scalePhaseResult{
		Replicas:    rounds[0].Replicas,
		Concurrency: rounds[0].Concurrency,
		DurationSeconds: medianScaleFloat(
			rounds,
			func(value scalePhaseResult) float64 { return value.DurationSeconds },
		),
		MetricSamples: medianScaleUint(rounds, func(value scalePhaseResult) uint64 { return value.MetricSamples }),
		LogRecords:    medianScaleUint(rounds, func(value scalePhaseResult) uint64 { return value.LogRecords }),
		SamplesPerSecond: medianScaleFloat(
			rounds,
			func(value scalePhaseResult) float64 { return value.SamplesPerSecond },
		),
		LogsPerSecond: medianScaleFloat(rounds, func(value scalePhaseResult) float64 { return value.LogsPerSecond }),
		RecordsPerSecond: medianScaleFloat(
			rounds,
			func(value scalePhaseResult) float64 { return value.RecordsPerSecond },
		),
		RequestsFailed: medianScaleUint(rounds, func(value scalePhaseResult) uint64 { return value.RequestsFailed }),
		RequestLatency: latencySummary{
			P50MS: medianScaleFloat(rounds, func(value scalePhaseResult) float64 { return value.RequestLatency.P50MS }),
			P95MS: medianScaleFloat(rounds, func(value scalePhaseResult) float64 { return value.RequestLatency.P95MS }),
			P99MS: medianScaleFloat(rounds, func(value scalePhaseResult) float64 { return value.RequestLatency.P99MS }),
		},
	}
	for targetIndex := range rounds[0].RequestsPerTarget {
		result.RequestsPerTarget = append(
			result.RequestsPerTarget,
			medianScaleUint(
				rounds,
				func(value scalePhaseResult) uint64 { return scaleUintAt(value.RequestsPerTarget, targetIndex) },
			),
		)
		result.MetricRequestsPerTarget = append(
			result.MetricRequestsPerTarget,
			medianScaleUint(
				rounds,
				func(value scalePhaseResult) uint64 { return scaleUintAt(value.MetricRequestsPerTarget, targetIndex) },
			),
		)
		result.LogRequestsPerTarget = append(
			result.LogRequestsPerTarget,
			medianScaleUint(
				rounds,
				func(value scalePhaseResult) uint64 { return scaleUintAt(value.LogRequestsPerTarget, targetIndex) },
			),
		)
		result.FailedRequestsPerTarget = append(
			result.FailedRequestsPerTarget,
			medianScaleUint(
				rounds,
				func(value scalePhaseResult) uint64 { return scaleUintAt(value.FailedRequestsPerTarget, targetIndex) },
			),
		)
		result.RequestsPerSecondPerTarget = append(
			result.RequestsPerSecondPerTarget,
			medianScaleFloat(rounds, func(value scalePhaseResult) float64 {
				return scaleFloatAt(value.RequestsPerSecondPerTarget, targetIndex)
			}),
		)
		result.RecordsPerSecondPerTarget = append(
			result.RecordsPerSecondPerTarget,
			medianScaleFloat(rounds, func(value scalePhaseResult) float64 {
				return scaleFloatAt(value.RecordsPerSecondPerTarget, targetIndex)
			}),
		)
	}
	return result
}

func medianScaleFloat(rounds []scalePhaseResult, value func(scalePhaseResult) float64) float64 {
	values := make([]float64, len(rounds))
	for index := range rounds {
		values[index] = value(rounds[index])
	}
	return median(values)
}

func medianScaleUint(rounds []scalePhaseResult, value func(scalePhaseResult) uint64) uint64 {
	values := make([]uint64, len(rounds))
	for index := range rounds {
		values[index] = value(rounds[index])
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return uint64(math.Round(float64(values[middle-1])/2 + float64(values[middle])/2))
}

func scaleUintAt(values []uint64, index int) uint64 {
	if index >= len(values) {
		return 0
	}
	return values[index]
}

func scaleFloatAt(values []float64, index int) float64 {
	if index >= len(values) {
		return 0
	}
	return values[index]
}

func representativeBaseline(baselines []scaleTargetBaseline) scalePhaseResult {
	if len(baselines) == 0 {
		return scalePhaseResult{}
	}
	result := scalePhaseResult{Replicas: 1, Concurrency: baselines[0].Median.Concurrency}
	latencyP50 := make([]float64, 0, len(baselines))
	latencyP95 := make([]float64, 0, len(baselines))
	latencyP99 := make([]float64, 0, len(baselines))
	var totalRequests, totalMetricRequests, totalLogRequests, totalFailed uint64
	var totalRequestsPerSecond, totalRecordsPerSecond float64
	for _, baseline := range baselines {
		value := baseline.Median
		result.DurationSeconds += value.DurationSeconds
		result.MetricSamples += value.MetricSamples
		result.LogRecords += value.LogRecords
		result.SamplesPerSecond += value.SamplesPerSecond
		result.LogsPerSecond += value.LogsPerSecond
		result.RecordsPerSecond += value.RecordsPerSecond
		result.RequestsFailed += value.RequestsFailed
		totalRequests += scaleUintAt(value.RequestsPerTarget, 0)
		totalMetricRequests += scaleUintAt(value.MetricRequestsPerTarget, 0)
		totalLogRequests += scaleUintAt(value.LogRequestsPerTarget, 0)
		totalFailed += scaleUintAt(value.FailedRequestsPerTarget, 0)
		totalRequestsPerSecond += scaleFloatAt(value.RequestsPerSecondPerTarget, 0)
		totalRecordsPerSecond += scaleFloatAt(value.RecordsPerSecondPerTarget, 0)
		latencyP50 = append(latencyP50, value.RequestLatency.P50MS)
		latencyP95 = append(latencyP95, value.RequestLatency.P95MS)
		latencyP99 = append(latencyP99, value.RequestLatency.P99MS)
	}
	count := float64(len(baselines))
	result.DurationSeconds /= count
	result.MetricSamples = uint64(math.Round(float64(result.MetricSamples) / count))
	result.LogRecords = uint64(math.Round(float64(result.LogRecords) / count))
	result.RequestsFailed = uint64(math.Round(float64(result.RequestsFailed) / count))
	result.SamplesPerSecond /= count
	result.LogsPerSecond /= count
	result.RecordsPerSecond /= count
	result.RequestLatency = latencySummary{
		P50MS: median(latencyP50),
		P95MS: median(latencyP95),
		P99MS: median(latencyP99),
	}
	result.RequestsPerTarget = []uint64{uint64(math.Round(float64(totalRequests) / count))}
	result.MetricRequestsPerTarget = []uint64{uint64(math.Round(float64(totalMetricRequests) / count))}
	result.LogRequestsPerTarget = []uint64{uint64(math.Round(float64(totalLogRequests) / count))}
	result.FailedRequestsPerTarget = []uint64{uint64(math.Round(float64(totalFailed) / count))}
	result.RequestsPerSecondPerTarget = []float64{totalRequestsPerSecond / count}
	result.RecordsPerSecondPerTarget = []float64{totalRecordsPerSecond / count}
	return result
}

func scaleCapacityReference(nominal, highControl []scaleTargetBaseline) scaleThroughputReference {
	result := scaleThroughputReference{}
	consider := func(source string, phase scalePhaseResult) {
		if phase.SamplesPerSecond > result.SamplesPerSecond {
			result.SamplesPerSecond = phase.SamplesPerSecond
			result.SampleSource = source
		}
		if phase.LogsPerSecond > result.LogsPerSecond {
			result.LogsPerSecond = phase.LogsPerSecond
			result.LogSource = source
		}
	}
	for _, baseline := range nominal {
		consider(baseline.Target+"_nominal", baseline.Median)
	}
	for _, baseline := range highControl {
		consider(baseline.Target+"_high_control", baseline.Median)
	}
	result.RecordsPerSecond = result.SamplesPerSecond + result.LogsPerSecond
	return result
}

func conservativeUpstreamHeadroom(
	controlRounds, scaledRounds []scalePhaseResult,
	minimumRequired float64,
) scaleUpstreamHeadroomEvidence {
	result := scaleUpstreamHeadroomEvidence{
		MinimumControlSamplesPerSecond: math.MaxFloat64,
		MinimumControlLogsPerSecond:    math.MaxFloat64,
		MinimumRequiredRatio:           minimumRequired,
	}
	for _, phase := range controlRounds {
		result.MinimumControlSamplesPerSecond = min(result.MinimumControlSamplesPerSecond, phase.SamplesPerSecond)
		result.MinimumControlLogsPerSecond = min(result.MinimumControlLogsPerSecond, phase.LogsPerSecond)
	}
	for _, phase := range scaledRounds {
		result.MaximumScaledSamplesPerSecond = max(result.MaximumScaledSamplesPerSecond, phase.SamplesPerSecond)
		result.MaximumScaledLogsPerSecond = max(result.MaximumScaledLogsPerSecond, phase.LogsPerSecond)
	}
	if len(controlRounds) == 0 {
		result.MinimumControlSamplesPerSecond = 0
		result.MinimumControlLogsPerSecond = 0
	}
	if result.MaximumScaledSamplesPerSecond > 0 {
		result.SampleRatio = result.MinimumControlSamplesPerSecond / result.MaximumScaledSamplesPerSecond
	}
	if result.MaximumScaledLogsPerSecond > 0 {
		result.LogRatio = result.MinimumControlLogsPerSecond / result.MaximumScaledLogsPerSecond
	}
	return result
}

func evaluateScaleRoundEfficiencies(
	rounds []scalePhaseResult,
	reference scaleThroughputReference,
	replicas int,
	minimumEfficiency, minimumPassingRatio float64,
) ([]scaleRoundEfficiency, int, int) {
	evaluations := make([]scaleRoundEfficiency, 0, len(rounds))
	passing := 0
	for roundIndex, phase := range rounds {
		evaluation := scaleRoundEfficiency{Round: roundIndex + 1}
		if replicas > 0 && reference.SamplesPerSecond > 0 {
			evaluation.SampleReplicaEfficiency = phase.SamplesPerSecond / reference.SamplesPerSecond / float64(replicas)
		}
		if replicas > 0 && reference.LogsPerSecond > 0 {
			evaluation.LogReplicaEfficiency = phase.LogsPerSecond / reference.LogsPerSecond / float64(replicas)
		}
		evaluation.PairedGatePassed = evaluation.SampleReplicaEfficiency >= minimumEfficiency &&
			evaluation.LogReplicaEfficiency >= minimumEfficiency
		if evaluation.PairedGatePassed {
			passing++
		}
		evaluations = append(evaluations, evaluation)
	}
	required := int(math.Ceil(float64(len(rounds)) * minimumPassingRatio))
	return evaluations, passing, required
}

func applyScaleRoundEfficiencyGate(result *scaleGateResult, rounds []scalePhaseResult) {
	result.ScaledRoundEfficiencies, result.PassingScaledRounds, result.RequiredPassingScaledRounds = evaluateScaleRoundEfficiencies(
		rounds,
		result.SingleCapacityReference,
		len(result.Targets),
		result.MinReplicaEfficiency,
		result.MinPassingRoundRatio,
	)
	if result.PassingScaledRounds < result.RequiredPassingScaledRounds {
		appendScaleFailure(result, fmt.Sprintf(
			"only %d of %d scaled rounds passed paired metric/log replica efficiency %.3f (require %d rounds from ratio %.3f); failing rounds: %s",
			result.PassingScaledRounds,
			len(result.ScaledRoundEfficiencies),
			result.MinReplicaEfficiency,
			result.RequiredPassingScaledRounds,
			result.MinPassingRoundRatio,
			formatFailingScaleRounds(result.ScaledRoundEfficiencies),
		))
	}
	result.GatePassed = len(result.GateFailures) == 0
}

func formatFailingScaleRounds(evaluations []scaleRoundEfficiency) string {
	failed := make([]string, 0)
	for _, evaluation := range evaluations {
		if evaluation.PairedGatePassed {
			continue
		}
		failed = append(failed, fmt.Sprintf(
			"%d(metric=%.3f,log=%.3f)",
			evaluation.Round,
			evaluation.SampleReplicaEfficiency,
			evaluation.LogReplicaEfficiency,
		))
	}
	if len(failed) == 0 {
		return "none"
	}
	return strings.Join(failed, ",")
}

func baselineThroughputRatios(baselines []scaleTargetBaseline) (float64, float64) {
	if len(baselines) < 2 {
		return 0, 0
	}
	minSamples, maxSamples := math.MaxFloat64, float64(0)
	minLogs, maxLogs := math.MaxFloat64, float64(0)
	for _, baseline := range baselines {
		samples := baseline.Median.SamplesPerSecond
		logs := baseline.Median.LogsPerSecond
		minSamples = min(minSamples, samples)
		maxSamples = max(maxSamples, samples)
		minLogs = min(minLogs, logs)
		maxLogs = max(maxLogs, logs)
	}
	if minSamples <= 0 || minLogs <= 0 || maxSamples <= 0 || maxLogs <= 0 {
		return 0, 0
	}
	return minSamples / maxSamples, minLogs / maxLogs
}

func scaleTargetBalanceFailures(phase scalePhaseResult, lowerBound float64) []string {
	if phase.Replicas < 2 {
		return nil
	}
	failures := make([]string, 0)
	checkUint := func(name string, values []uint64) {
		if len(values) != phase.Replicas {
			failures = append(failures, fmt.Sprintf("%s lacks per-target measurements", name))
			return
		}
		var total uint64
		for _, value := range values {
			total += value
		}
		minimum := float64(total) / float64(phase.Replicas) * lowerBound
		for index, value := range values {
			if float64(value) < minimum {
				failures = append(
					failures,
					fmt.Sprintf("target %d %s %d is below balanced lower bound %.1f", index+1, name, value, minimum),
				)
			}
		}
	}
	checkFloat := func(name string, values []float64) {
		if len(values) != phase.Replicas {
			failures = append(failures, fmt.Sprintf("%s lacks per-target measurements", name))
			return
		}
		var total float64
		for _, value := range values {
			total += value
		}
		minimum := total / float64(phase.Replicas) * lowerBound
		for index, value := range values {
			if value < minimum {
				failures = append(
					failures,
					fmt.Sprintf("target %d %s %.1f is below balanced lower bound %.1f", index+1, name, value, minimum),
				)
			}
		}
	}
	checkUint("successful requests", phase.RequestsPerTarget)
	checkUint("successful metric requests", phase.MetricRequestsPerTarget)
	checkUint("successful log requests", phase.LogRequestsPerTarget)
	checkFloat("records/s", phase.RecordsPerSecondPerTarget)
	return failures
}

func appendScaleFailure(result *scaleGateResult, failure string) {
	for _, existing := range result.GateFailures {
		if existing == failure {
			return
		}
	}
	result.GateFailures = append(result.GateFailures, failure)
}

func evaluateCapacityScaling(
	targets []string,
	nominal, highControl scalePhaseResult,
	capacityReference scaleThroughputReference,
	scaled scalePhaseResult,
	perReplicaConcurrency int,
	minEfficiency float64,
) scaleGateResult {
	result := scaleGateResult{
		Version: 1, Targets: targets,
		SingleNominal: nominal, SingleCapacityReference: capacityReference, Scaled: scaled,
		MinReplicaEfficiency: minEfficiency,
	}
	if capacityReference.SamplesPerSecond > 0 {
		result.SampleScaleFactor = scaled.SamplesPerSecond / capacityReference.SamplesPerSecond
		result.SampleReplicaEfficiency = result.SampleScaleFactor / float64(len(targets))
	}
	if capacityReference.LogsPerSecond > 0 {
		result.LogScaleFactor = scaled.LogsPerSecond / capacityReference.LogsPerSecond
		result.LogReplicaEfficiency = result.LogScaleFactor / float64(len(targets))
	}
	wantScaledConcurrency := perReplicaConcurrency * len(targets)
	if nominal.Concurrency != perReplicaConcurrency {
		result.GateFailures = append(
			result.GateFailures,
			fmt.Sprintf("single nominal concurrency is %d, want %d", nominal.Concurrency, perReplicaConcurrency),
		)
	}
	if highControl.Concurrency != wantScaledConcurrency {
		result.GateFailures = append(
			result.GateFailures,
			fmt.Sprintf(
				"single high-concurrency control is %d, want %d",
				highControl.Concurrency,
				wantScaledConcurrency,
			),
		)
	}
	if scaled.Concurrency != wantScaledConcurrency {
		result.GateFailures = append(
			result.GateFailures,
			fmt.Sprintf("scaled total concurrency is %d, want %d", scaled.Concurrency, wantScaledConcurrency),
		)
	}
	for index, requests := range scaled.RequestsPerTarget {
		if requests == 0 {
			result.GateFailures = append(
				result.GateFailures,
				fmt.Sprintf("target %d received no successful scaled-phase requests", index+1),
			)
		}
	}
	result.GatePassed = len(result.GateFailures) == 0
	return result
}
