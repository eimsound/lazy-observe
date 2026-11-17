package embed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eimsound/lazy-observe/internal/backoff"
)

const (
	MetricsAddr = "127.0.0.1:18428"
	LogsAddr    = "127.0.0.1:19428"
	TracesAddr  = "127.0.0.1:20428"
	AgentAddr   = "127.0.0.1:22428"
)

const (
	defaultRestartInitial = time.Second
	defaultRestartMax     = time.Minute
	defaultHealthInterval = 100 * time.Millisecond
)

type BinarySource interface {
	Path(name string) (string, error)
}

type DirectorySource string

func (d DirectorySource) Path(name string) (string, error) {
	dir, err := filepath.Abs(string(d))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

type Config struct {
	Source           BinarySource
	DataDir          string
	StartupTimeout   time.Duration
	ShutdownTimeout  time.Duration
	RestartInitial   time.Duration
	RestartMax       time.Duration
	Output           io.Writer
	ScrapeConfigPath string
	Standalone       bool
	MetricsIngestURL string
	MetricsRetention time.Duration
	LogsRetention    time.Duration
	TracesRetention  time.Duration
	PortOffset       int

	command       func(name, binary string, args []string) *exec.Cmd
	restartJitter func() float64
	restartWait   func(context.Context, time.Duration) error
}

type componentStatus string

const (
	componentStarting   componentStatus = "starting"
	componentHealthy    componentStatus = "healthy"
	componentRestarting componentStatus = "restarting"
	componentStopping   componentStatus = "stopping"
	componentStopped    componentStatus = "stopped"
)

type managedProcess struct {
	cmd    *exec.Cmd
	writer *linePrefixWriter
	done   chan struct{}
	err    error
}

type component struct {
	name, binary, addr, dataDir string
	args                        []string
	storage                     bool

	mu      sync.RWMutex
	process *managedProcess
	status  componentStatus
	lastErr error
}

func (c *component) update(process *managedProcess, status componentStatus, err error) {
	c.mu.Lock()
	c.process = process
	c.status = status
	c.lastErr = err
	c.mu.Unlock()
}

func (c *component) setStatus(status componentStatus, err error) {
	c.mu.Lock()
	c.status = status
	c.lastErr = err
	c.mu.Unlock()
}

func (c *component) snapshot() (*managedProcess, componentStatus, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.process, c.status, c.lastErr
}

type Supervisor struct {
	components      []*component
	shutdownTimeout time.Duration
	startupTimeout  time.Duration
	restartInitial  time.Duration
	restartMax      time.Duration
	output          io.Writer
	command         func(name, binary string, args []string) *exec.Cmd
	restartJitter   func() float64
	restartWait     func(context.Context, time.Duration) error

	lifecycleCtx    context.Context
	cancelLifecycle context.CancelFunc
	lifecycle       sync.WaitGroup
	fatal           chan error
	fatalOnce       sync.Once
	shutdownOnce    sync.Once
	shutdownErr     error
}

func Start(ctx context.Context, cfg Config) (*Supervisor, error) {
	if cfg.Source == nil {
		return nil, errors.New("binary source is required")
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 20 * time.Second
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 15 * time.Second
	}
	if cfg.RestartInitial <= 0 {
		cfg.RestartInitial = defaultRestartInitial
	}
	if cfg.RestartMax <= 0 {
		cfg.RestartMax = defaultRestartMax
	}
	if cfg.RestartMax < cfg.RestartInitial {
		return nil, errors.New("component restart maximum must not be shorter than initial delay")
	}
	if cfg.Output == nil {
		cfg.Output = os.Stderr
	}
	if cfg.command == nil {
		cfg.command = func(_ string, binary string, args []string) *exec.Cmd {
			return exec.Command(binary, args...)
		}
	}
	if cfg.restartJitter == nil {
		cfg.restartJitter = rand.Float64
	}
	if cfg.restartWait == nil {
		cfg.restartWait = waitForRestart
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	if cfg.ScrapeConfigPath == "" {
		cfg.ScrapeConfigPath = filepath.Join(dataDir, "scrape", "scrape.yml")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.ScrapeConfigPath), 0o755); err != nil {
		return nil, fmt.Errorf("create scrape config directory: %w", err)
	}
	if _, err := os.Stat(cfg.ScrapeConfigPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(cfg.ScrapeConfigPath, []byte("scrape_configs: []\n"), 0o644); err != nil {
			return nil, fmt.Errorf("create initial scrape config: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("inspect scrape config: %w", err)
	}
	lifecycleCtx, cancelLifecycle := context.WithCancel(ctx)
	s := &Supervisor{
		shutdownTimeout: cfg.ShutdownTimeout,
		startupTimeout:  cfg.StartupTimeout,
		restartInitial:  cfg.RestartInitial,
		restartMax:      cfg.RestartMax,
		output:          cfg.Output,
		command:         cfg.command,
		restartJitter:   cfg.restartJitter,
		restartWait:     cfg.restartWait,
		lifecycleCtx:    lifecycleCtx,
		cancelLifecycle: cancelLifecycle,
		fatal:           make(chan error, 1),
	}
	defs := componentDefinitions(cfg, dataDir)
	for i := range defs {
		path, err := cfg.Source.Path(defs[i].binary)
		if err != nil {
			cancelLifecycle()
			return nil, fmt.Errorf("resolve %s binary: %w", defs[i].name, err)
		}
		defs[i].binary = path
		defs[i].status = componentStarting
		s.components = append(s.components, &defs[i])
	}
	for _, managed := range s.components {
		if err := s.startInitial(ctx, managed); err != nil {
			_ = s.Shutdown()
			return nil, err
		}
	}
	for _, managed := range s.components {
		s.lifecycle.Add(1)
		go s.monitor(managed)
	}
	fmt.Fprintf(s.output, "lazy-observe: %d managed components healthy\n", len(defs))
	return s, nil
}

func componentDefinitions(cfg Config, dataDir string) []component {
	metricsAddr := offsetAddress(MetricsAddr, cfg.PortOffset)
	logsAddr := offsetAddress(LogsAddr, cfg.PortOffset)
	tracesAddr := offsetAddress(TracesAddr, cfg.PortOffset)
	agentAddr := offsetAddress(AgentAddr, cfg.PortOffset)
	ingestURL := "http://" + metricsAddr
	defs := make([]component, 0, 4)
	if !cfg.Standalone {
		defs = append(defs,
			component{
				name: "metrics", binary: "victoria-metrics-prod", addr: metricsAddr,
				dataDir: filepath.Join(dataDir, "metrics"), storage: true,
				args: []string{
					"-retentionPeriod=" + retentionArg(cfg.MetricsRetention),
					"-search.latencyOffset=0s",
					"-inmemoryDataFlushInterval=1s",
				},
			},
			component{
				name: "logs", binary: "victoria-logs-prod", addr: logsAddr,
				dataDir: filepath.Join(dataDir, "logs"), storage: true,
				args: []string{
					"-retentionPeriod=" + retentionArg(cfg.LogsRetention),
					"-inmemoryDataFlushInterval=1s",
				},
			},
			component{
				name: "traces", binary: "victoria-traces-prod", addr: tracesAddr,
				dataDir: filepath.Join(dataDir, "traces"), storage: true,
				args: []string{
					"-retentionPeriod=" + retentionArg(cfg.TracesRetention),
					"-inmemoryDataFlushInterval=1s",
					"-insert.indexFlushInterval=1s",
				},
			},
		)
	} else {
		ingestURL = strings.TrimRight(cfg.MetricsIngestURL, "/")
	}
	if ingestURL != "" {
		defs = append(defs, component{
			name: "vmagent", binary: "vmagent-prod", addr: agentAddr,
			args: []string{
				"-promscrape.config=" + cfg.ScrapeConfigPath,
				"-remoteWrite.url=" + ingestURL + "/api/v1/write",
				"-remoteWrite.tmpDataPath=" + filepath.Join(dataDir, "scrape", "remotewrite"),
			},
		})
	}
	return defs
}

func offsetAddress(address string, offset int) string {
	if offset == 0 {
		return address
	}
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		return address
	}
	return net.JoinHostPort(host, strconv.Itoa(port+offset))
}

// Done reports only unrecoverable supervisor failures. A managed child exit is
// recoverable and remains internal to the per-component lifecycle loop.
func (s *Supervisor) Done() <-chan error { return s.fatal }

func (s *Supervisor) startInitial(ctx context.Context, managed *component) error {
	process, err := s.launch(managed, componentStarting, nil)
	if err != nil {
		return fmt.Errorf("start %s: %w", managed.name, err)
	}
	if err := s.waitHealthy(ctx, managed, process, s.startupTimeout); err != nil {
		stopProcess(process)
		<-process.done
		managed.setStatus(componentStopped, err)
		return fmt.Errorf("start %s: %w", managed.name, err)
	}
	managed.setStatus(componentHealthy, nil)
	fmt.Fprintf(s.output, "lazy-observe: %s ready pid=%d\n", managed.name, process.cmd.Process.Pid)
	return nil
}

func (s *Supervisor) launch(
	managed *component,
	status componentStatus,
	lastErr error,
) (*managedProcess, error) {
	info, err := os.Stat(managed.binary)
	if err != nil {
		return nil, fmt.Errorf("locate binary: %w", err)
	}
	if info.Mode()&0o111 == 0 {
		return nil, errors.New("binary is not executable")
	}
	if managed.storage {
		if err := os.MkdirAll(managed.dataDir, 0o755); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}
	args := []string{"-httpListenAddr=" + managed.addr, "-loggerOutput=stderr"}
	if managed.storage {
		args = append(args, "-storageDataPath="+managed.dataDir)
	}
	args = append(args, managed.args...)
	cmd := s.command(managed.name, managed.binary, args)
	if cmd == nil {
		return nil, errors.New("command factory returned nil")
	}
	writer := &linePrefixWriter{prefix: "[" + managed.name + "] ", dst: s.output}
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := startManagedCommand(cmd); err != nil {
		return nil, err
	}
	process := &managedProcess{cmd: cmd, writer: writer, done: make(chan struct{})}
	managed.update(process, status, lastErr)
	go func() {
		process.err = cmd.Wait()
		writer.Flush()
		close(process.done)
	}()
	return process, nil
}

func (s *Supervisor) waitHealthy(
	ctx context.Context,
	managed *component,
	process *managedProcess,
	timeout time.Duration,
) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(defaultHealthInterval)
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-process.done:
			return unexpectedExitError(managed.name, process.err)
		case <-deadline.C:
			if lastErr == nil {
				lastErr = errors.New("health check did not succeed")
			}
			return fmt.Errorf("health check timed out after %s: %w", timeout, lastErr)
		case <-ticker.C:
			lastErr = checkHealth(ctx, client, "http://"+managed.addr+"/health")
			if lastErr == nil {
				return nil
			}
		}
	}
}

func (s *Supervisor) monitor(managed *component) {
	defer s.lifecycle.Done()
	process, _, _ := managed.snapshot()
	retryIndex := 0
	for {
		if process != nil {
			select {
			case <-s.lifecycleCtx.Done():
				return
			case <-process.done:
			}
			if s.lifecycleCtx.Err() != nil {
				return
			}
			managed.setStatus(componentRestarting, unexpectedExitError(managed.name, process.err))
		}

		delay, err := backoff.Delay(
			retryIndex,
			s.restartInitial,
			s.restartMax,
			s.restartJitter(),
			0,
			0,
		)
		if err != nil {
			s.publishFatal(fmt.Errorf("calculate %s restart backoff: %w", managed.name, err))
			return
		}
		if retryIndex < int(^uint(0)>>1) {
			retryIndex++
		}
		fmt.Fprintf(s.output, "lazy-observe: %s restarting in %s\n", managed.name, delay)
		if err := s.restartWait(s.lifecycleCtx, delay); err != nil {
			if s.lifecycleCtx.Err() != nil {
				return
			}
			s.publishFatal(fmt.Errorf("wait to restart %s: %w", managed.name, err))
			return
		}
		if s.lifecycleCtx.Err() != nil {
			return
		}

		lastErr := managed.lastError()
		process, err = s.launch(managed, componentRestarting, lastErr)
		if err != nil {
			managed.update(nil, componentRestarting, err)
			fmt.Fprintf(s.output, "lazy-observe: %s restart failed: %v\n", managed.name, err)
			continue
		}
		if err := s.waitHealthy(s.lifecycleCtx, managed, process, s.startupTimeout); err != nil {
			if s.lifecycleCtx.Err() != nil {
				return
			}
			stopProcess(process)
			<-process.done
			managed.setStatus(componentRestarting, err)
			fmt.Fprintf(s.output, "lazy-observe: %s restart did not become healthy: %v\n", managed.name, err)
			process = nil
			continue
		}
		managed.setStatus(componentHealthy, nil)
		retryIndex = 0
		fmt.Fprintf(s.output, "lazy-observe: %s recovered pid=%d\n", managed.name, process.cmd.Process.Pid)
	}
}

func (c *component) lastError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastErr
}

func (s *Supervisor) publishFatal(err error) {
	s.fatalOnce.Do(func() { s.fatal <- err })
}

func (s *Supervisor) Health(ctx context.Context) map[string]error {
	client := &http.Client{Timeout: time.Second}
	result := make(map[string]error, len(s.components))
	for _, managed := range s.components {
		process, status, _ := managed.snapshot()
		if status != componentHealthy || process == nil {
			result[managed.name] = fmt.Errorf("component %s", status)
			continue
		}
		select {
		case <-process.done:
			result[managed.name] = errors.New("component restarting")
			continue
		default:
		}
		if err := checkHealth(ctx, client, "http://"+managed.addr+"/health"); err != nil {
			result[managed.name] = errors.New("component health check failed")
			continue
		}
		result[managed.name] = nil
	}
	return result
}

func retentionArg(value time.Duration) string {
	if value <= 0 {
		return "1d"
	}
	if value%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", value/(24*time.Hour))
	}
	return value.String()
}

func (s *Supervisor) ReloadVMAgent() error {
	for _, managed := range s.components {
		if managed.name != "vmagent" {
			continue
		}
		process, status, _ := managed.snapshot()
		if status != componentHealthy || process == nil || process.cmd.Process == nil {
			return errors.New("vmagent is restarting")
		}
		select {
		case <-process.done:
			return errors.New("vmagent is restarting")
		default:
			return process.cmd.Process.Signal(syscall.SIGHUP)
		}
	}
	return errors.New("vmagent is not managed")
}

func checkHealth(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "OK" {
		return fmt.Errorf("status=%d body=%q", resp.StatusCode, body)
	}
	return nil
}

func (s *Supervisor) Shutdown() error {
	s.shutdownOnce.Do(func() { s.shutdownErr = s.shutdown() })
	return s.shutdownErr
}

func (s *Supervisor) shutdown() error {
	s.cancelLifecycle()
	s.lifecycle.Wait()
	for _, managed := range s.components {
		managed.setStatus(componentStopping, nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	var errs []error
	if err := s.stopComponents(ctx, false); err != nil {
		errs = append(errs, err)
	}
	if ctx.Err() == nil {
		if err := s.stopComponents(ctx, true); err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() != nil {
		s.killAll()
		errs = append(errs, fmt.Errorf("shutdown timed out after %s", s.shutdownTimeout))
	}
	for _, managed := range s.components {
		managed.setStatus(componentStopped, nil)
	}
	return errors.Join(errs...)
}

type stopTarget struct {
	managed  *component
	process  *managedProcess
	signaled bool
}

func (s *Supervisor) stopComponents(ctx context.Context, storage bool) error {
	targets := make([]stopTarget, 0, len(s.components))
	for _, managed := range s.components {
		if managed.storage != storage {
			continue
		}
		process, _, _ := managed.snapshot()
		if process != nil {
			targets = append(targets, stopTarget{managed: managed, process: process})
		}
	}
	var errs []error
	for index := range targets {
		select {
		case <-targets[index].process.done:
			continue
		default:
		}
		process := targets[index].process.cmd.Process
		if process == nil {
			continue
		}
		if err := process.Signal(syscall.SIGTERM); err != nil {
			if !errors.Is(err, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("stop %s: %w", targets[index].managed.name, err))
			}
			continue
		}
		targets[index].signaled = true
	}
	for _, target := range targets {
		select {
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		case <-target.process.done:
			if target.signaled && target.process.err != nil && !isSignalExit(target.process.err) {
				errs = append(errs, fmt.Errorf("%s: %w", target.managed.name, target.process.err))
			}
		}
	}
	return errors.Join(errs...)
}

func (s *Supervisor) killAll() {
	for _, managed := range s.components {
		process, _, _ := managed.snapshot()
		if process == nil || process.cmd.Process == nil {
			continue
		}
		select {
		case <-process.done:
		default:
			_ = process.cmd.Process.Kill()
		}
	}
}

func (s *Supervisor) componentPID(name string) int {
	for _, managed := range s.components {
		if managed.name != name {
			continue
		}
		process, _, _ := managed.snapshot()
		if process != nil && process.cmd.Process != nil {
			return process.cmd.Process.Pid
		}
	}
	return 0
}

func stopProcess(process *managedProcess) {
	if process == nil || process.cmd.Process == nil {
		return
	}
	select {
	case <-process.done:
	default:
		_ = process.cmd.Process.Kill()
	}
}

func unexpectedExitError(name string, err error) error {
	if err == nil {
		return fmt.Errorf("%s exited unexpectedly with status 0", name)
	}
	return fmt.Errorf("%s exited unexpectedly: %w", name, err)
}

func isSignalExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled()
}

func waitForRestart(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var outputMu sync.Mutex

type linePrefixWriter struct {
	mu     sync.Mutex
	prefix string
	dst    io.Writer
	buf    bytes.Buffer
}

func (w *linePrefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, _ := w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			_, _ = w.buf.WriteString(line)
			break
		}
		w.writeLine(line)
	}
	return n, nil
}

func (w *linePrefixWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.writeLine(w.buf.String() + "\n")
		w.buf.Reset()
	}
}

func (w *linePrefixWriter) writeLine(line string) {
	outputMu.Lock()
	defer outputMu.Unlock()
	_, _ = fmt.Fprint(w.dst, w.prefix, line)
}
