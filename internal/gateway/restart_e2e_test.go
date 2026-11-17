//go:build linux

package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	pprofprofile "github.com/google/pprof/profile"
)

const (
	restartAdminPassword           = "restart-e2e-admin-password"
	restartWaitTimeout             = 45 * time.Second
	restartStorageDurabilityWindow = 2500 * time.Millisecond
)

func TestRestartErrorType(t *testing.T) {
	if got := restartErrorType("after_pending_recovery"); got != "RestartAfterPendingRecovery" {
		t.Fatalf("restart error type=%q", got)
	}
}

// TestRestartJourneyE2E proves the same-build persistence boundary across a
// graceful server stop, forced process-group stops, and accepted pending work.
func TestRestartJourneyE2E(t *testing.T) {
	fixture := newRestartFixture(t)

	beforeGraceful := fixture.newSignalSet("before_graceful", 1)
	server := fixture.start(time.Second)
	fixture.ingestSignalSet(server, beforeGraceful)
	fixture.assertSignalSet(server, beforeGraceful)
	fixture.stopGracefully(server)

	server = fixture.start(time.Second)
	fixture.assertSignalSet(server, beforeGraceful)
	afterGraceful := fixture.newSignalSet("after_graceful", 2)
	fixture.ingestSignalSet(server, afterGraceful)
	fixture.assertSignalSet(server, afterGraceful)
	fixture.proveManagedLogsRecovery(server, afterGraceful)
	fixture.waitForManagedStorageDurability()
	fixture.stopForcibly(server)

	server = fixture.start(30 * time.Second)
	fixture.assertSignalSet(server, beforeGraceful)
	fixture.assertSignalSet(server, afterGraceful)
	afterForced := fixture.newSignalSet("after_forced", 3)
	fixture.ingestSignalSet(server, afterForced)
	fixture.assertSignalSet(server, afterForced)
	fixture.waitForManagedStorageDurability()

	pending := fixture.newSignalSet("pending_recovery", 4)
	fixture.acceptPendingErrorWhileLogsRestart(server, pending)
	fixture.stopForcibly(server)

	server = fixture.start(time.Second)
	fixture.waitForIssue(server, pending)
	fixture.assertIssue(server, pending)
	fixture.assertPendingInboxCompletedWithinBudget(server)
	fixture.assertSignalSet(server, beforeGraceful)
	fixture.assertSignalSet(server, afterGraceful)
	fixture.assertSignalSet(server, afterForced)
	afterPending := fixture.newSignalSet("after_pending_recovery", 5)
	fixture.ingestSignalSet(server, afterPending)
	fixture.assertSignalSet(server, afterPending)
	fixture.stopGracefully(server)

	t.Log("PASS restart-graceful: old relationships survived and new telemetry was queryable")
	t.Log("PASS restart-forced: queryable telemetry survived a SIGKILL process-group restart")
	t.Log("PASS restart-pending: accepted Inbox work resumed within its 20-attempt budget")
	t.Log("PASS managed-recovery: logs restarted without changing the Gateway PID or interrupting metrics/traces")
}

type restartFixture struct {
	t               *testing.T
	binary          string
	storageBinDir   string
	workDir         string
	dataDir         string
	metaDBURL       string
	token           string
	readerToken     string
	baseURL         string
	componentOffset int
	client          *http.Client
	active          *restartServer
	run             int
}

type restartServer struct {
	cmd      *exec.Cmd
	pid      int
	logPath  string
	logFile  *os.File
	session  *e2eSession
	exited   chan struct{}
	resultMu sync.Mutex
	result   error
	closed   bool
}

type restartSignalSet struct {
	label          string
	metricName     string
	logMarker      string
	traceID        string
	eventID        string
	errorType      string
	replaySession  string
	profileService string
	profileUpload  string
	profileFrame   string
	issueID        int
	profileID      int
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	binary := strings.TrimSpace(os.Getenv("LO_E2E_BINARY"))
	storageBinDir := strings.TrimSpace(os.Getenv("LO_E2E_STORAGE_BIN_DIR"))
	if binary == "" || storageBinDir == "" {
		t.Skip("LO_E2E_BINARY and LO_E2E_STORAGE_BIN_DIR are required")
	}
	var err error
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	storageBinDir, err = filepath.Abs(storageBinDir)
	if err != nil {
		t.Fatal(err)
	}
	artifactRoot := strings.TrimSpace(os.Getenv("LO_E2E_RESTART_ARTIFACT_ROOT"))
	if artifactRoot == "" {
		artifactRoot = os.TempDir()
	}
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	workDir, err := os.MkdirTemp(artifactRoot, "lazy-observe-restart-e2e.")
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(workDir, "data")
	metaPath := filepath.Join(dataDir, "meta", "lazy-observe.db")
	store, err := meta.OpenStore(context.Background(), metaPath)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(context.Background(), "Default", "default")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(context.Background(), "admin", restartAdminPassword, "admin"); err != nil {
		t.Fatal(err)
	}
	readerToken := userQueryToken(t, store, "restart-reader")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	baseURL := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	offset, err := availableComponentOffset()
	if err != nil {
		t.Fatal(err)
	}
	fixture := &restartFixture{
		t:               t,
		binary:          binary,
		storageBinDir:   storageBinDir,
		workDir:         workDir,
		dataDir:         dataDir,
		metaDBURL:       "sqlite://" + metaPath,
		token:           token,
		readerToken:     readerToken,
		baseURL:         baseURL,
		componentOffset: offset,
		client:          &http.Client{Timeout: 5 * time.Second},
	}
	t.Cleanup(func() {
		if fixture.active != nil {
			fixture.forceCleanup(fixture.active)
		}
		fixture.killStrayManagedProcesses()
		fixture.sanitizeLogs()
		if t.Failed() || os.Getenv("LO_E2E_KEEP") == "1" {
			if err := os.RemoveAll(fixture.dataDir); err != nil {
				t.Errorf("remove restart E2E state from retained artifacts: %v", err)
			}
			t.Logf("restart E2E artifacts retained at %s", fixture.workDir)
			return
		}
		if err := os.RemoveAll(fixture.workDir); err != nil {
			t.Errorf("remove restart E2E work directory: %v", err)
		}
	})
	return fixture
}

func (f *restartFixture) start(restartDelay time.Duration) *restartServer {
	f.t.Helper()
	if f.active != nil {
		f.t.Fatal("restart fixture already has an active server")
	}
	f.run++
	logPath := filepath.Join(f.workDir, fmt.Sprintf("server-%d.log", f.run))
	logFile, err := os.Create(logPath)
	if err != nil {
		f.t.Fatal(err)
	}
	addr := strings.TrimPrefix(f.baseURL, "http://")
	cmd := exec.Command(
		f.binary,
		"server",
		"--listen", addr,
		"--storage-bin-dir", f.storageBinDir,
		"--data-dir", f.dataDir,
		"--component-port-offset", strconv.Itoa(f.componentOffset),
		"--storage-startup-timeout", "30s",
		"--storage-restart-initial", restartDelay.String(),
		"--storage-restart-max", restartDelay.String(),
	)
	cmd.Env = append(
		os.Environ(),
		"LO_META_DB_URL="+f.metaDBURL,
		"LO_SELF_MONITORING_ENABLED=false",
		"LO_E2E_COMPONENT_PORT_OFFSET="+strconv.Itoa(f.componentOffset),
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		f.t.Fatal(err)
	}
	server := &restartServer{
		cmd: cmd, pid: cmd.Process.Pid, logPath: logPath, logFile: logFile, exited: make(chan struct{}),
	}
	f.active = server
	go func() {
		err := cmd.Wait()
		server.resultMu.Lock()
		server.result = err
		server.resultMu.Unlock()
		close(server.exited)
	}()
	f.eventually("server health after start", restartWaitTimeout, func() bool {
		select {
		case <-server.exited:
			return false
		default:
		}
		status, _ := f.get(nil, f.baseURL+"/healthz")
		return status == http.StatusOK
	})
	server.session = loginE2E(f.t, f.client, f.baseURL, "admin", restartAdminPassword)
	return server
}

func (f *restartFixture) stopGracefully(server *restartServer) {
	f.t.Helper()
	if server != f.active {
		f.t.Fatal("graceful stop target is not active")
	}
	if err := syscall.Kill(server.pid, syscall.SIGTERM); err != nil {
		f.t.Fatal(err)
	}
	if err := server.wait(30 * time.Second); err != nil {
		f.t.Fatalf("server did not stop gracefully: %v", err)
	}
	f.finishStop(server)
}

func (f *restartFixture) stopForcibly(server *restartServer) {
	f.t.Helper()
	if server != f.active {
		f.t.Fatal("forced stop target is not active")
	}
	// SIGKILL every managed component and the gateway directly rather than
	// relying on a process-group signal. In exec-based CI job containers the
	// gateway's own Setpgid is a no-op, so syscall.Kill(-pid, SIGKILL) targets
	// a process group that does not exist and leaves the managed processes
	// running, which finishStop then times out on.
	for _, pid := range f.managedPIDs() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if err := syscall.Kill(server.pid, syscall.SIGKILL); err != nil {
		f.t.Fatal(err)
	}
	if err := server.wait(15 * time.Second); err == nil {
		f.t.Fatal("SIGKILL process-group stop returned a clean process result")
	}
	f.finishStop(server)
}

func (f *restartFixture) finishStop(server *restartServer) {
	f.t.Helper()
	if err := server.closeLog(); err != nil {
		f.t.Errorf("close server log: %v", err)
	}
	f.active = nil
	// Wait for every managed component and the gateway process itself to exit.
	// The gateway is checked as a single process rather than via its process
	// group: inside exec-based CI job containers the gateway's Setpgid is a
	// no-op, so the group is never established and kill(-pid, 0) reports it as
	// present even after the process has died. A direct per-process check is
	// reliable in every environment.
	f.eventually("managed processes to exit", 15*time.Second, func() bool {
		return len(f.managedPIDs()) == 0 && processGone(server.pid)
	})
}

func (f *restartFixture) forceCleanup(server *restartServer) {
	_ = syscall.Kill(-server.pid, syscall.SIGKILL)
	_ = server.wait(5 * time.Second)
	_ = server.closeLog()
	f.active = nil
}

func (s *restartServer) wait(timeout time.Duration) error {
	select {
	case <-s.exited:
		s.resultMu.Lock()
		defer s.resultMu.Unlock()
		return s.result
	case <-time.After(timeout):
		return fmt.Errorf("process %d did not exit within %s", s.pid, timeout)
	}
}

func (s *restartServer) closeLog() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.logFile.Close()
}

func (f *restartFixture) newSignalSet(label string, sequence int) *restartSignalSet {
	return &restartSignalSet{
		label:          label,
		metricName:     "lazy_observe_restart_" + label,
		logMarker:      "lazy-observe-restart-" + label,
		traceID:        fmt.Sprintf("%032x", 0x1000+sequence),
		eventID:        "restart-event-" + label,
		errorType:      restartErrorType(label),
		replaySession:  "restart-session-" + label,
		profileService: "restart-profile-" + label,
		profileUpload:  fmt.Sprintf("%032x", 0x2000+sequence),
		profileFrame:   "restart." + label,
	}
}

func restartErrorType(label string) string {
	var result strings.Builder
	result.WriteString("Restart")
	capitalize := true
	for _, character := range label {
		if character == '_' {
			capitalize = true
			continue
		}
		if capitalize && character >= 'a' && character <= 'z' {
			character -= 'a' - 'A'
		}
		result.WriteRune(character)
		capitalize = false
	}
	return result.String()
}

func (f *restartFixture) ingestSignalSet(server *restartServer, set *restartSignalSet) {
	f.t.Helper()
	f.postToken(f.baseURL+"/v1/metrics", "application/x-protobuf", metricPayload(set.metricName, 100), nil)
	f.postToken(f.baseURL+"/v1/logs", "application/x-protobuf", logsPayload(set.logMarker), nil)
	f.postToken(
		f.baseURL+"/v1/traces",
		"application/json",
		tracePayload(set.traceID, "restart-service", "restart "+set.label),
		nil,
	)
	f.acceptError(set)
	f.waitForIssue(server, set)
	f.uploadReplay(set)
	f.uploadProfile(set)
}

func (f *restartFixture) acceptError(set *restartSignalSet) {
	f.t.Helper()
	payload := map[string]any{"events": []any{map[string]any{
		"event_id": set.eventID, "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"platform": "go", "level": "error", "message": "restart persistence marker",
		"exception": map[string]any{
			"type": set.errorType, "value": set.label,
			"stacktrace": map[string]any{"frames": []any{map[string]any{
				"function": "restartJourney." + set.label,
				"filename": "/restart/" + set.label + ".go", "lineno": 42, "in_app": true,
			}}},
		},
		"contexts": map[string]any{"trace": map[string]string{
			"trace_id": set.traceID, "span_id": "0102030405060708",
		}},
		"session_id": set.replaySession,
	}}}
	body, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	f.postToken(f.baseURL+"/api/events/errors", "application/json", body, nil)
}

func (f *restartFixture) waitForIssue(server *restartServer, set *restartSignalSet) {
	f.t.Helper()
	f.eventually("Issue projection for "+set.label, restartWaitTimeout, func() bool {
		issue, ok := f.lookupIssue(server, set)
		if !ok || issue.LastEventID != set.eventID || issue.Count != 1 {
			return false
		}
		if set.issueID != 0 && set.issueID != issue.ID {
			return false
		}
		set.issueID = issue.ID
		return true
	})
}

func (f *restartFixture) lookupIssue(server *restartServer, set *restartSignalSet) (issueSummaryDTO, bool) {
	target := f.baseURL + "/api/issues?" + url.Values{"q": {set.errorType}}.Encode()
	status, body := f.get(server.session, target)
	if status != http.StatusOK {
		return issueSummaryDTO{}, false
	}
	var listing struct {
		Issues []issueSummaryDTO `json:"issues"`
	}
	if json.Unmarshal(body, &listing) != nil || len(listing.Issues) != 1 {
		return issueSummaryDTO{}, false
	}
	return listing.Issues[0], true
}

func (f *restartFixture) uploadReplay(set *restartSignalSet) {
	f.t.Helper()
	now := time.Now().UTC()
	raw := fmt.Appendf(nil, `[{
		"type":5,
		"data":{"tag":"lazy-observe:error","payload":{"timestamp":%q,"event_id":%q,"title":%q,"level":"error"}},
		"timestamp":%d
	}]`, now.Format(time.RFC3339Nano), set.eventID, set.errorType, now.UnixMilli())
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		f.t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		f.t.Fatal(err)
	}
	target := f.baseURL + "/api/replay/segments?" + url.Values{
		"session_id": {set.replaySession}, "seq": {"0"},
	}.Encode()
	f.postToken(target, "application/octet-stream", compressed.Bytes(), map[string]string{
		"Content-Encoding": "gzip", "X-LO-Replay-Error": "1",
	})
}

func (f *restartFixture) uploadProfile(set *restartSignalSet) {
	f.t.Helper()
	profileBytes := writeProfile(f.t, syntheticProfile(17, set.profileFrame, "restart.main"))
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("profile", "profile.pb.gz")
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := part.Write(profileBytes); err != nil {
		f.t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	_ = writer.WriteField("type", "cpu")
	_ = writer.WriteField("service", set.profileService)
	_ = writer.WriteField("start", now.Add(-10*time.Second).Format(time.RFC3339))
	_ = writer.WriteField("end", now.Format(time.RFC3339))
	_ = writer.WriteField("labels", fmt.Sprintf(`{"trace_id":%q}`, set.traceID))
	_ = writer.WriteField("upload_id", set.profileUpload)
	if err := writer.Close(); err != nil {
		f.t.Fatal(err)
	}
	response := f.postToken(f.baseURL+"/api/profiles", writer.FormDataContentType(), body.Bytes(), nil)
	var record profileRecordResponse
	if err := json.Unmarshal(response, &record); err != nil || record.ID <= 0 || record.TraceID != set.traceID {
		f.t.Fatalf("profile upload response for %s: err=%v body=%s", set.label, err, response)
	}
	set.profileID = record.ID
}

func (f *restartFixture) assertSignalSet(server *restartServer, set *restartSignalSet) {
	f.t.Helper()
	f.eventually("metric query for "+set.label, restartWaitTimeout, func() bool {
		status, body := f.getToken(f.baseURL + "/api/v1/query?" + url.Values{"query": {set.metricName}}.Encode())
		return status == http.StatusOK && metricValuesAre(body, "100")
	})
	f.eventually("log query for "+set.label, restartWaitTimeout, func() bool {
		target := f.baseURL + "/api/v1/logs/query?" + url.Values{
			"query": {`_msg:="` + set.logMarker + `"`},
		}.Encode()
		status, body := f.getToken(target)
		return status == http.StatusOK && bytes.Contains(body, []byte(set.logMarker))
	})
	f.eventually("trace query for "+set.label, restartWaitTimeout, func() bool {
		status, body := f.getToken(f.baseURL + "/api/traces/" + set.traceID)
		return status == http.StatusOK && bytes.Contains(body, []byte(set.traceID))
	})
	f.waitForIssue(server, set)
	f.assertIssue(server, set)
	f.assertReplay(set)
	f.assertProfile(set)
}

func (f *restartFixture) assertIssue(server *restartServer, set *restartSignalSet) {
	f.t.Helper()
	issue, ok := f.lookupIssue(server, set)
	if !ok || issue.ID != set.issueID || issue.LastEventID != set.eventID || issue.Count != 1 {
		f.t.Fatalf("Issue relationship changed for %s: %+v", set.label, issue)
	}
	f.eventually("Issue raw-event relationship for "+set.label, restartWaitTimeout, func() bool {
		status, body := f.get(
			server.session,
			fmt.Sprintf("%s/api/issues/%d/events/%s", f.baseURL, set.issueID, set.eventID),
		)
		return status == http.StatusOK && bytes.Contains(body, []byte(set.traceID)) &&
			bytes.Contains(body, []byte(set.replaySession))
	})
}

func (f *restartFixture) assertReplay(set *restartSignalSet) {
	f.t.Helper()
	status, body := f.getToken(f.baseURL + "/api/replay/sessions/" + set.replaySession)
	if status != http.StatusOK {
		f.t.Fatalf("Replay %s status=%d body=%s", set.label, status, body)
	}
	var detail struct {
		SessionID string `json:"session_id"`
		Markers   []struct {
			Type    string `json:"type"`
			IssueID string `json:"issue_id"`
			EventID string `json:"event_id"`
		} `json:"markers"`
	}
	if err := json.Unmarshal(body, &detail); err != nil || detail.SessionID != set.replaySession {
		f.t.Fatalf("Replay relationship for %s: err=%v body=%s", set.label, err, body)
	}
	found := false
	for _, marker := range detail.Markers {
		if marker.Type == "error" && marker.IssueID == strconv.Itoa(set.issueID) && marker.EventID == set.eventID {
			found = true
			break
		}
	}
	if !found {
		f.t.Fatalf("Replay relationship for %s: body=%s", set.label, body)
	}
	status, body = f.getToken(f.baseURL + "/api/replay/sessions/" + set.replaySession + "/segments/0")
	if status != http.StatusOK {
		f.t.Fatalf("Replay segment %s status=%d", set.label, status)
	}
	raw := body
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			f.t.Fatal(err)
		}
		raw, err = io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			f.t.Fatal(err)
		}
	}
	if !bytes.Contains(raw, []byte(set.eventID)) {
		f.t.Fatalf("Replay segment blob for %s did not contain event ID: body=%s", set.label, raw)
	}
}

func (f *restartFixture) assertProfile(set *restartSignalSet) {
	f.t.Helper()
	target := f.baseURL + "/api/profiles?" + url.Values{
		"service": {set.profileService}, "type": {"cpu"},
	}.Encode()
	status, body := f.getToken(target)
	var listing profileListResponse
	if status != http.StatusOK || json.Unmarshal(body, &listing) != nil || len(listing.Profiles) != 1 ||
		listing.Profiles[0].ID != set.profileID || listing.Profiles[0].TraceID != set.traceID {
		f.t.Fatalf("Profile relationship for %s: status=%d body=%s", set.label, status, body)
	}
	status, body = f.getToken(fmt.Sprintf("%s/api/profiles/%d/raw", f.baseURL, set.profileID))
	profile, err := pprofprofile.ParseData(body)
	if status != http.StatusOK || err != nil {
		f.t.Fatalf("Profile blob for %s: status=%d err=%v", set.label, status, err)
	}
	for _, function := range profile.Function {
		if function.Name == set.profileFrame {
			return
		}
	}
	f.t.Fatalf("Profile %s lost frame %q", set.label, set.profileFrame)
}

func (f *restartFixture) proveManagedLogsRecovery(server *restartServer, available *restartSignalSet) {
	f.t.Helper()
	logsPID := f.managedPID("victoria-logs-prod")
	if logsPID <= 0 {
		f.t.Fatal("managed VictoriaLogs PID was not found")
	}
	if err := syscall.Kill(logsPID, syscall.SIGKILL); err != nil {
		f.t.Fatal(err)
	}
	f.eventually("logs component restarting health", 10*time.Second, func() bool {
		status, body := f.get(nil, f.baseURL+"/healthz")
		return status == http.StatusServiceUnavailable && bytes.Contains(body, []byte("logs")) &&
			bytes.Contains(body, []byte("restarting")) && !bytes.Contains(body, []byte("127.0.0.1"))
	})
	status, _ := f.get(nil, f.baseURL+"/livez")
	if status != http.StatusOK || syscall.Kill(server.pid, 0) != nil {
		f.t.Fatalf("Gateway changed while logs restarted: livez=%d pid=%d", status, server.pid)
	}
	metricStatus, metricBody := f.getToken(
		f.baseURL + "/api/v1/query?" + url.Values{"query": {available.metricName}}.Encode(),
	)
	traceStatus, traceBody := f.getToken(f.baseURL + "/api/traces/" + available.traceID)
	if metricStatus != http.StatusOK || !metricValuesAre(metricBody, "100") || traceStatus != http.StatusOK ||
		!bytes.Contains(traceBody, []byte(available.traceID)) {
		f.t.Fatalf("unaffected signals failed during logs recovery: metrics=%d traces=%d", metricStatus, traceStatus)
	}
	f.eventually("logs component replacement", 15*time.Second, func() bool {
		pid := f.managedPID("victoria-logs-prod")
		return pid > 0 && pid != logsPID
	})
	f.eventually("health after logs replacement", 15*time.Second, func() bool {
		status, _ := f.get(nil, f.baseURL+"/healthz")
		return status == http.StatusOK
	})
	marker := "lazy-observe-managed-recovery"
	f.postToken(f.baseURL+"/v1/logs", "application/x-protobuf", logsPayload(marker), nil)
	f.eventually("logs ingest after managed recovery", 15*time.Second, func() bool {
		target := f.baseURL + "/api/v1/logs/query?" + url.Values{"query": {`_msg:="` + marker + `"`}}.Encode()
		status, body := f.getToken(target)
		return status == http.StatusOK && bytes.Contains(body, []byte(marker))
	})
}

func (f *restartFixture) acceptPendingErrorWhileLogsRestart(server *restartServer, pending *restartSignalSet) {
	f.t.Helper()
	logsPID := f.managedPID("victoria-logs-prod")
	if logsPID <= 0 {
		f.t.Fatal("managed VictoriaLogs PID was not found for pending-work test")
	}
	if err := syscall.Kill(logsPID, syscall.SIGKILL); err != nil {
		f.t.Fatal(err)
	}
	f.eventually("logs restart backoff before pending acceptance", 10*time.Second, func() bool {
		status, body := f.get(nil, f.baseURL+"/healthz")
		return status == http.StatusServiceUnavailable && bytes.Contains(body, []byte("restarting"))
	})
	f.acceptError(pending)
	f.eventually("accepted error to remain active while logs are down", 10*time.Second, func() bool {
		diagnostics, ok := f.inboxDiagnostics(server)
		return ok && diagnostics.ActiveItems == 1 &&
			diagnostics.PendingItems+diagnostics.ProcessingItems == 1
	})
}

func (f *restartFixture) assertPendingInboxCompletedWithinBudget(server *restartServer) {
	f.t.Helper()
	f.eventually("pending Inbox completion after restart", restartWaitTimeout, func() bool {
		diagnostics, ok := f.inboxDiagnostics(server)
		return ok && diagnostics.ActiveItems == 0 && diagnostics.PendingItems == 0 &&
			diagnostics.ProcessingItems == 0 && diagnostics.RetriesTotal > 0 && diagnostics.RetriesTotal < 20
	})
}

func (f *restartFixture) waitForManagedStorageDurability() {
	// Managed storage is configured with a one-second in-memory flush interval.
	// VictoriaTraces may need one interval to publish its ID index and one more
	// to persist it, so cross that bounded horizon before the deliberate SIGKILL.
	timer := time.NewTimer(restartStorageDurabilityWindow)
	defer timer.Stop()
	<-timer.C
}

func (f *restartFixture) inboxDiagnostics(server *restartServer) (errortrack.InboxDiagnostics, bool) {
	status, body := f.get(server.session, f.baseURL+"/api/system/error-inbox")
	var diagnostics errortrack.InboxDiagnostics
	return diagnostics, status == http.StatusOK && json.Unmarshal(body, &diagnostics) == nil
}

func (f *restartFixture) postToken(target, contentType string, body []byte, headers map[string]string) []byte {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", contentType)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		f.t.Fatalf("POST %s: status=%d body=%s", target, resp.StatusCode, responseBody)
	}
	return responseBody
}

// getToken reads with a user API token; the project token is ingest-only.
func (f *restartFixture) getToken(target string) (int, []byte) {
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+f.readerToken)
	req.Header.Set("X-LO-Project", "default")
	return f.do(req)
}

func (f *restartFixture) get(session *e2eSession, target string) (int, []byte) {
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	for _, cookie := range session.cookies() {
		req.AddCookie(cookie)
	}
	if session != nil {
		req.Header.Set("X-LO-Project", "default")
	}
	return f.do(req)
}

func (f *restartFixture) do(req *http.Request) (int, []byte) {
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func (f *restartFixture) eventually(description string, timeout time.Duration, condition func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Fatalf("timed out waiting for %s", description)
}

func (f *restartFixture) managedPID(name string) int {
	for _, pid := range f.managedPIDs() {
		cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if bytes.Contains(cmdline, []byte(name)) {
			return pid
		}
	}
	return 0
}

func (f *restartFixture) managedPIDs() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var result []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || !bytes.Contains(cmdline, []byte(f.dataDir)) {
			continue
		}
		if bytes.Contains(cmdline, []byte("victoria-metrics-prod")) ||
			bytes.Contains(cmdline, []byte("victoria-logs-prod")) ||
			bytes.Contains(cmdline, []byte("victoria-traces-prod")) ||
			bytes.Contains(cmdline, []byte("vmagent-prod")) {
			result = append(result, pid)
		}
	}
	return result
}

func (f *restartFixture) killStrayManagedProcesses() {
	for _, pid := range f.managedPIDs() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func (f *restartFixture) sanitizeLogs() {
	tokenPattern := regexp.MustCompile(`lou?_[[:xdigit:]]+`)
	passwordPattern := regexp.MustCompile(`(?i)(password=)[^[:space:]]+`)
	for run := 1; run <= f.run; run++ {
		path := filepath.Join(f.workDir, fmt.Sprintf("server-%d.log", run))
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		body = tokenPattern.ReplaceAll(body, []byte("[REDACTED_TOKEN]"))
		body = passwordPattern.ReplaceAll(body, []byte("${1}[REDACTED]"))
		_ = os.WriteFile(path, body, 0o600)
	}
}

func processGone(pid int) bool {
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

func availableComponentOffset() (int, error) {
	basePorts := []int{18428, 19428, 20428, 22428}
	start := 1_000 + os.Getpid()%20_000
	for attempt := 0; attempt < 600; attempt++ {
		offset := 1_000 + (start+attempt*37)%29_000
		listeners := make([]net.Listener, 0, len(basePorts))
		available := true
		for _, base := range basePorts {
			listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(base+offset)))
			if err != nil {
				available = false
				break
			}
			listeners = append(listeners, listener)
		}
		for _, listener := range listeners {
			_ = listener.Close()
		}
		if available {
			return offset, nil
		}
	}
	return 0, errors.New("no free managed-component port offset found")
}
