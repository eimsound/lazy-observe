package embed

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type testBinarySource struct{}

func (testBinarySource) Path(string) (string, error) { return os.Executable() }

type helperCommandFactory struct {
	mu        sync.Mutex
	launches  map[string]int
	modes     map[string]string
	eventFile string
}

func newHelperCommandFactory() *helperCommandFactory {
	return &helperCommandFactory{launches: map[string]int{}, modes: map[string]string{}}
}

func (f *helperCommandFactory) command(name, binary string, args []string) *exec.Cmd {
	f.mu.Lock()
	f.launches[name]++
	generation := f.launches[name]
	mode := f.modes[name]
	eventFile := f.eventFile
	f.mu.Unlock()
	commandArgs := append([]string{"-test.run=TestSupervisorHelperProcess", "--"}, args...)
	cmd := exec.Command(binary, commandArgs...)
	cmd.Env = append(
		os.Environ(),
		"LO_SUPERVISOR_HELPER=1",
		"LO_SUPERVISOR_COMPONENT="+name,
		fmt.Sprintf("LO_SUPERVISOR_GENERATION=%d", generation),
		"LO_SUPERVISOR_MODE="+mode,
		"LO_SUPERVISOR_EVENT_FILE="+eventFile,
	)
	return cmd
}

func (f *helperCommandFactory) setMode(name, mode string) {
	f.mu.Lock()
	f.modes[name] = mode
	f.mu.Unlock()
}

func (f *helperCommandFactory) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launches[name]
}

func TestSupervisorRestartsUnexpectedManagedExit(t *testing.T) {
	factory := newHelperCommandFactory()
	restartEntered := make(chan struct{}, 1)
	releaseRestart := make(chan struct{})
	supervisor := startTestSupervisor(t, 1_000, factory, func(cfg *Config) {
		cfg.restartWait = func(ctx context.Context, _ time.Duration) error {
			select {
			case restartEntered <- struct{}{}:
			default:
			}
			select {
			case <-releaseRestart:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	logsPID := supervisor.componentPID("logs")
	metricsPID := supervisor.componentPID("metrics")
	if logsPID <= 0 || metricsPID <= 0 {
		t.Fatalf("initial pids logs=%d metrics=%d", logsPID, metricsPID)
	}
	if err := syscall.Kill(logsPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restartEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("logs component did not enter restart backoff")
	}
	health := supervisor.Health(context.Background())
	if health["logs"] == nil || !strings.Contains(health["logs"].Error(), "restarting") {
		t.Fatalf("logs restart health=%v", health["logs"])
	}
	for _, name := range []string{"metrics", "traces", "vmagent"} {
		if health[name] != nil {
			t.Fatalf("unaffected %s health=%v", name, health[name])
		}
	}
	close(releaseRestart)

	waitForSupervisorCondition(t, 5*time.Second, func() bool {
		return factory.count("logs") >= 2 && supervisor.componentPID("logs") != logsPID &&
			supervisor.Health(context.Background())["logs"] == nil
	})
	if got := supervisor.componentPID("metrics"); got != metricsPID {
		t.Fatalf("metrics restarted with logs: pid=%d want=%d", got, metricsPID)
	}
	select {
	case err := <-supervisor.Done():
		t.Fatalf("recoverable component exit became supervisor fatal error: %v", err)
	default:
	}
}

func TestSupervisorBackoffIsCapped(t *testing.T) {
	factory := newHelperCommandFactory()
	delays := make(chan time.Duration, 16)
	restartWait := func(ctx context.Context, delay time.Duration) error {
		select {
		case delays <- delay:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	supervisor := startTestSupervisor(t, 1_100, factory, func(cfg *Config) {
		cfg.RestartInitial = 10 * time.Millisecond
		cfg.RestartMax = 25 * time.Millisecond
		cfg.restartJitter = func() float64 { return 1 }
		cfg.restartWait = restartWait
	})
	factory.setMode("logs", "exit")
	if err := syscall.Kill(supervisor.componentPID("logs"), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	got := make([]time.Duration, 0, 4)
	for len(got) < cap(got) {
		select {
		case delay := <-delays:
			got = append(got, delay)
		case <-time.After(5 * time.Second):
			t.Fatalf("restart delays=%v", got)
		}
	}
	if want := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		25 * time.Millisecond,
		25 * time.Millisecond,
	}; !slices.Equal(
		got,
		want,
	) {
		t.Fatalf("restart delays=%v want=%v", got, want)
	}
}

func TestSupervisorShutdownNeverRestarts(t *testing.T) {
	factory := newHelperCommandFactory()
	factory.eventFile = filepath.Join(t.TempDir(), "shutdown-order.log")
	supervisor := startTestSupervisor(t, 1_200, factory, nil)
	before := map[string]int{}
	for _, name := range []string{"metrics", "logs", "traces", "vmagent"} {
		before[name] = factory.count(name)
	}
	if err := supervisor.Shutdown(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	for name, want := range before {
		if got := factory.count(name); got != want {
			t.Fatalf("%s launches after shutdown=%d want=%d", name, got, want)
		}
	}
	events, err := os.ReadFile(factory.eventFile)
	if err != nil {
		t.Fatal(err)
	}
	order := strings.Fields(string(events))
	if len(order) != 4 || order[0] != "vmagent" ||
		!slices.Contains(order[1:], "metrics") || !slices.Contains(order[1:], "logs") ||
		!slices.Contains(order[1:], "traces") {
		t.Fatalf("shutdown order=%v", order)
	}
}

func TestStandaloneSupervisorRestartsOnlyOwnedVMAgent(t *testing.T) {
	factory := newHelperCommandFactory()
	cfg := Config{
		Source:           testBinarySource{},
		DataDir:          t.TempDir(),
		StartupTimeout:   3 * time.Second,
		ShutdownTimeout:  3 * time.Second,
		RestartInitial:   10 * time.Millisecond,
		RestartMax:       10 * time.Millisecond,
		Output:           io.Discard,
		PortOffset:       1_300,
		Standalone:       true,
		MetricsIngestURL: "http://metrics.example",
		command:          factory.command,
		restartJitter:    func() float64 { return 1 },
	}
	supervisor, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Shutdown() })
	if len(supervisor.components) != 1 || supervisor.components[0].name != "vmagent" ||
		supervisor.components[0].storage {
		t.Fatalf("standalone managed components=%v", componentNamesFromPointers(supervisor.components))
	}
	initialPID := supervisor.componentPID("vmagent")
	if err := syscall.Kill(initialPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorCondition(t, 5*time.Second, func() bool {
		return factory.count("vmagent") >= 2 && supervisor.componentPID("vmagent") != initialPID &&
			supervisor.Health(context.Background())["vmagent"] == nil
	})
}

func TestManagedStorageUsesBoundedUncleanShutdownFlushWindow(t *testing.T) {
	definitions := componentDefinitions(Config{
		MetricsRetention: time.Hour,
		LogsRetention:    time.Hour,
		TracesRetention:  time.Hour,
	}, t.TempDir())
	byName := make(map[string][]string, len(definitions))
	for index := range definitions {
		definition := &definitions[index]
		byName[definition.name] = definition.args
	}
	for _, name := range []string{"metrics", "logs", "traces"} {
		if !slices.Contains(byName[name], "-inmemoryDataFlushInterval=1s") {
			t.Fatalf("%s args do not bound unclean-shutdown persistence: %v", name, byName[name])
		}
	}
	if !slices.Contains(byName["traces"], "-insert.indexFlushInterval=1s") {
		t.Fatalf("traces args do not persist the query index promptly: %v", byName["traces"])
	}
	// VictoriaTraces v0.12+ also applies its 30s default latency offset to the LogsQL APIs (LLM stats); managed
	// storage keeps queries near real time, the same as metrics.
	for _, name := range []string{"metrics", "traces"} {
		if !slices.Contains(byName[name], "-search.latencyOffset=0s") {
			t.Fatalf("%s args do not disable the query latency offset: %v", name, byName[name])
		}
	}
}

func startTestSupervisor(
	t *testing.T,
	portOffset int,
	factory *helperCommandFactory,
	configure func(*Config),
) *Supervisor {
	t.Helper()
	cfg := Config{
		Source:          testBinarySource{},
		DataDir:         t.TempDir(),
		StartupTimeout:  3 * time.Second,
		ShutdownTimeout: 3 * time.Second,
		Output:          io.Discard,
		PortOffset:      portOffset,
		command:         factory.command,
	}
	if configure != nil {
		configure(&cfg)
	}
	supervisor, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Shutdown() })
	return supervisor
}

func waitForSupervisorCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("supervisor condition timed out")
}

func TestSupervisorHelperProcess(t *testing.T) {
	if os.Getenv("LO_SUPERVISOR_HELPER") != "1" {
		return
	}
	mode := os.Getenv("LO_SUPERVISOR_MODE")
	if mode == "exit" {
		os.Exit(17)
	}
	address := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "-httpListenAddr=") {
			address = strings.TrimPrefix(arg, "-httpListenAddr=")
			break
		}
	}
	if address == "" {
		fmt.Fprintln(os.Stderr, "missing helper listen address")
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "OK")
	})}
	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, syscall.SIGTERM, syscall.SIGINT)
	go func() { _ = server.Serve(listener) }()
	<-stopping
	if eventFile := os.Getenv("LO_SUPERVISOR_EVENT_FILE"); eventFile != "" {
		file, err := os.OpenFile(eventFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintln(file, os.Getenv("LO_SUPERVISOR_COMPONENT"))
			_ = file.Close()
		}
	}
	_ = server.Shutdown(context.Background())
	os.Exit(0)
}

func TestOffsetAddress(t *testing.T) {
	if got := offsetAddress(AgentAddr, 100); got != "127.0.0.1:22528" {
		t.Fatalf("offset address=%q", got)
	}
	if got := offsetAddress(AgentAddr, 0); got != AgentAddr {
		t.Fatalf("zero offset address=%q", got)
	}
}

func TestStandaloneComponentTopologyExcludesSignalStorage(t *testing.T) {
	cfg := Config{
		Standalone:       true,
		ScrapeConfigPath: "/tmp/scrape.yml",
	}
	defs := componentDefinitions(cfg, "/tmp/data")
	if got := componentNames(defs); len(got) != 0 {
		t.Fatalf("query-only standalone components=%v", got)
	}
	for index := range defs {
		if defs[index].storage {
			t.Fatalf("standalone unexpectedly owns storage component %q", defs[index].name)
		}
	}

	cfg.MetricsIngestURL = "http://remote-write:8428"
	defs = componentDefinitions(cfg, "/tmp/data")
	if got := componentNames(defs); !slices.Equal(got, []string{"vmagent"}) {
		t.Fatalf("write-enabled standalone components=%v", got)
	}
	if !slices.Contains(defs[0].args, "-remoteWrite.url=http://remote-write:8428/api/v1/write") {
		t.Fatalf("vmagent args=%v", defs[0].args)
	}
}

func TestAllInOneComponentTopologyOwnsThreeSignalStores(t *testing.T) {
	defs := componentDefinitions(Config{ScrapeConfigPath: "/tmp/scrape.yml"}, "/tmp/data")
	if got := componentNames(defs); !slices.Equal(got, []string{"metrics", "logs", "traces", "vmagent"}) {
		t.Fatalf("all-in-one components=%v", got)
	}
	for i := range 3 {
		if !defs[i].storage {
			t.Fatalf("component %q is not marked as storage", defs[i].name)
		}
	}
}

func componentNames(defs []component) []string {
	names := make([]string, len(defs))
	for i := range defs {
		names[i] = defs[i].name
	}
	return names
}

func componentNamesFromPointers(defs []*component) []string {
	names := make([]string, len(defs))
	for i := range defs {
		names[i] = defs[i].name
	}
	return names
}

func TestReloadVMAgentReportsHTTPStatusErrors(t *testing.T) {
	statusCode := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/-/reload" {
			t.Errorf("unexpected reload request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(statusCode)
		if statusCode != http.StatusOK {
			_, _ = io.WriteString(w, "invalid scrape.yml")
		}
	}))
	defer server.Close()

	addr := strings.TrimPrefix(server.URL, "http://")
	sup := &Supervisor{
		components: []*component{
			{
				name:   "vmagent",
				addr:   addr,
				status: componentHealthy,
				process: &managedProcess{
					cmd:  &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}},
					done: make(chan struct{}),
				},
			},
		},
	}
	if err := sup.ReloadVMAgent(); err != nil {
		t.Fatalf("expected HTTP 200 reload to succeed, got err=%v", err)
	}
	statusCode = http.StatusInternalServerError
	if err := sup.ReloadVMAgent(); err == nil || !strings.Contains(err.Error(), "invalid scrape.yml") {
		t.Fatalf("expected HTTP 500 reload error with body, got err=%v", err)
	}
}
