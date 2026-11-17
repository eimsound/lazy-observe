package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
)

func TestAccountAuditFactsExcludeSecretsAndClassifyFailures(t *testing.T) {
	const (
		adminPassword   = "audit-admin-password-secret"
		wrongPassword   = "audit-wrong-password-secret"
		createPassword  = "audit-create-password-secret"
		updatePassword  = "audit-update-password-secret"
		accountPassword = "audit-account-password-secret"
		csrfSecret      = "audit-csrf-token-secret"
	)

	store := testStore(t)
	if _, err := store.CreateUser(context.Background(), "audit-admin", adminPassword, "admin"); err != nil {
		t.Fatal(err)
	}
	emailOwner, err := store.CreateUser(context.Background(), "audit-email-owner", "email-owner-password", "member")
	if err != nil {
		t.Fatal(err)
	}
	occupiedEmail := "audit-occupied@example.com"
	if _, err := store.UpdateUserProfile(context.Background(), emailOwner.ID, nil, &occupiedEmail); err != nil {
		t.Fatal(err)
	}
	self, _, err := store.BootstrapSelfMonitoring(context.Background(), "lo_test-self-monitoring-derived-token-value")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	app := newTestGateway(Config{
		Tokens: store, Store: store, Management: management.New(store), Logger: logger,
		Account: testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay}),
	}).App()

	type result struct {
		body    []byte
		cookies []*http.Cookie
	}
	request := func(method, path, body string, cookie *http.Cookie, want int) result {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set(csrfHeaderName, csrfSecret)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, resp.StatusCode, want, responseBody)
		}
		return result{body: responseBody, cookies: resp.Cookies()}
	}

	request(
		http.MethodPost,
		"/api/auth/login",
		`{"username":"audit-admin","password":"`+wrongPassword+`"}`,
		nil,
		http.StatusUnauthorized,
	)
	login := request(
		http.MethodPost,
		"/api/auth/login",
		`{"username":"audit-admin","password":"`+adminPassword+`"}`,
		nil,
		http.StatusOK,
	)
	if len(login.cookies) != 1 {
		t.Fatalf("login cookies = %d, want 1", len(login.cookies))
	}
	session := login.cookies[0]
	request(
		http.MethodPut,
		"/api/auth/me",
		`{"email":"`+occupiedEmail+`"}`,
		session,
		http.StatusConflict,
	)
	request(
		http.MethodPut,
		"/api/auth/me",
		`{"display_name":"Audit Admin"}`,
		session,
		http.StatusOK,
	)
	createdUserToken := request(
		http.MethodPost,
		"/api/auth/tokens",
		`{"name":"audited-user-token"}`,
		session,
		http.StatusCreated,
	)
	var userToken struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(createdUserToken.body, &userToken); err != nil {
		t.Fatal(err)
	}
	request(http.MethodDelete, "/api/auth/tokens/"+userToken.ID, "", session, http.StatusNoContent)

	createdProject := request(
		http.MethodPost,
		"/api/projects",
		`{"name":"Audited project","slug":"audited-project"}`,
		session,
		http.StatusOK,
	)
	var project projectResponse
	if err := json.Unmarshal(createdProject.body, &project); err != nil {
		t.Fatal(err)
	}
	request(http.MethodPatch, "/api/projects/"+project.ID, `{"name":"Renamed audited project"}`, session, http.StatusOK)
	request(http.MethodPatch, "/api/projects/"+project.ID, `{"slug":"forbidden-rename"}`, session, http.StatusConflict)
	request(
		http.MethodPatch,
		"/api/projects/"+strconvI(self.ID),
		`{"name":"Forbidden self rename"}`,
		session,
		http.StatusForbidden,
	)
	secondProjectBody := request(
		http.MethodPost,
		"/api/projects",
		`{"name":"Audited second project","slug":"audited-second-project"}`,
		session,
		http.StatusOK,
	)
	var secondProject projectResponse
	if err := json.Unmarshal(secondProjectBody.body, &secondProject); err != nil {
		t.Fatal(err)
	}

	createdToken := request(
		http.MethodPost,
		"/api/projects/"+project.ID+"/tokens",
		`{"name":"audited-ingest"}`,
		session,
		http.StatusCreated,
	)
	var token struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(createdToken.body, &token); err != nil {
		t.Fatal(err)
	}
	request(
		http.MethodDelete,
		"/api/projects/"+secondProject.ID+"/tokens/"+token.ID,
		"",
		session,
		http.StatusNotFound,
	)
	request(http.MethodDelete, "/api/projects/"+project.ID+"/tokens/"+token.ID, "", session, http.StatusNoContent)

	createdUser := request(
		http.MethodPost,
		"/api/users",
		`{"username":"audited-user","password":"`+createPassword+`","role":"member"}`,
		session,
		http.StatusCreated,
	)
	var user meta.UserInfo
	if err := json.Unmarshal(createdUser.body, &user); err != nil {
		t.Fatal(err)
	}
	request(
		http.MethodPatch,
		"/api/users/"+strconvI(user.ID),
		`{"username":"audited-user-renamed","email":"`+occupiedEmail+`"}`,
		session,
		http.StatusConflict,
	)
	request(
		http.MethodPatch,
		"/api/users/"+strconvI(user.ID),
		`{"password":"`+updatePassword+`","role":"viewer"}`,
		session,
		http.StatusOK,
	)
	request(http.MethodDelete, "/api/users/"+strconvI(user.ID), "", session, http.StatusNoContent)
	request(http.MethodDelete, "/api/projects/"+secondProject.ID, "", session, http.StatusNoContent)
	request(http.MethodDelete, "/api/projects/"+project.ID, "", session, http.StatusNoContent)
	request(
		http.MethodPost,
		"/api/auth/password",
		`{"current_password":"`+adminPassword+`","new_password":"`+accountPassword+`"}`,
		session,
		http.StatusNoContent,
	)
	request(http.MethodPost, "/api/auth/logout", "", session, http.StatusNoContent)

	logText := output.String()
	for _, secret := range []string{
		adminPassword, wrongPassword, createPassword, updatePassword, accountPassword,
		token.Value, userToken.Value, session.Value, csrfSecret,
	} {
		if secret == "" {
			t.Fatal("test did not obtain a secret value")
		}
		if strings.Contains(logText, secret) {
			t.Fatalf("audit log leaked secret %q: %s", secret, logText)
		}
	}

	records := make([]map[string]any, 0)
	for _, line := range strings.Split(strings.TrimSpace(logText), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode audit record: %v: %s", err, line)
		}
		if record["event"] == managementAuditEvent {
			records = append(records, record)
		}
	}

	expectedTargets := map[string]string{
		"auth.login":               "user",
		"auth.logout":              "user",
		"auth.password_change":     "user",
		"auth.profile_update":      "user",
		"project.create":           "project",
		"project.update":           "project",
		"project.delete":           "project",
		"project_api_token.create": "project_api_token",
		"project_api_token.revoke": "project_api_token",
		"user.create":              "user",
		"user.update":              "user",
		"user.delete":              "user",
		"user_api_token.create":    "user_api_token",
		"user_api_token.revoke":    "user_api_token",
	}
	successes := make(map[string]bool, len(expectedTargets))
	loginFailure, updateFailure := false, false
	userConflict, protectedProject, crossProjectToken := false, false, false
	for _, record := range records {
		action, _ := record["action"].(string)
		target, expected := expectedTargets[action]
		if !expected {
			continue
		}
		if record["target_type"] != target {
			t.Errorf("%s target_type = %#v, want %q", action, record["target_type"], target)
		}
		if record["actor_username"] == nil || record["http_method"] == nil || record["http_path"] == nil {
			t.Errorf("%s missing actor or request context: %#v", action, record)
		}
		success, ok := record["success"].(bool)
		if !ok {
			t.Errorf("%s missing boolean success: %#v", action, record)
			continue
		}
		if success {
			successes[action] = true
			if record["target_id"] == nil {
				t.Errorf("successful %s missing target_id: %#v", action, record)
			}
			if strings.HasPrefix(action, "project.") || strings.HasPrefix(action, "project_api_token.") {
				if record["project_id"] == nil || record["project_slug"] == nil {
					t.Errorf("successful %s missing project context: %#v", action, record)
				}
			}
		}
		if action == "auth.login" && !success && record["failure_reason"] == "invalid_credentials" {
			loginFailure = true
		}
		if action == "project.update" && !success && record["failure_reason"] == "immutable_slug" {
			updateFailure = true
		}
		if action == "user.update" && !success && record["failure_reason"] == "conflict" {
			fields, _ := record["changed_fields"].([]any)
			userConflict = record["target_id"] != nil && len(fields) == 2
		}
		if action == "project.update" && !success && record["failure_reason"] == "protected_project" {
			protectedProject = record["target_id"] != nil && record["project_id"] != nil &&
				record["project_slug"] != nil
		}
		if action == "project_api_token.revoke" && !success && record["failure_reason"] == "not_found" {
			crossProjectToken = record["target_id"] != nil && record["project_id"] != nil &&
				record["project_slug"] != nil
		}
		for _, forbidden := range []string{"password", "token_value", "session_token", "cookie", "error"} {
			if _, exists := record[forbidden]; exists {
				t.Errorf("%s exposed forbidden audit attribute %q: %#v", action, forbidden, record)
			}
		}
	}
	for action := range expectedTargets {
		if !successes[action] {
			t.Errorf("missing successful audit record for %s", action)
		}
	}
	if !loginFailure || !updateFailure {
		t.Errorf("missing safe failure audit records: login=%t project_update=%t", loginFailure, updateFailure)
	}
	if !userConflict || !protectedProject || !crossProjectToken {
		t.Errorf(
			"missing management failure audit facts: user_conflict=%t protected_project=%t cross_project_token=%t",
			userConflict,
			protectedProject,
			crossProjectToken,
		)
	}

	persisted, err := store.ListAudit(context.Background(), meta.AuditListOptions{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	persistedJSON, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		adminPassword, wrongPassword, createPassword, updatePassword, accountPassword,
		token.Value, userToken.Value, session.Value, csrfSecret,
	} {
		if strings.Contains(string(persistedJSON), secret) {
			t.Fatalf("persisted audit leaked secret %q: %s", secret, persistedJSON)
		}
	}
	persistedUserConflict, persistedProtectedProject, persistedCrossProjectToken := false, false, false
	for _, record := range persisted {
		if record.ActorUsername == "" || record.HTTPMethod == "" || record.HTTPPath == "" {
			t.Errorf("persisted audit missing actor or request context: %#v", record)
		}
		switch {
		case record.Action == "user.update" && !record.Success && record.FailureReason == "conflict":
			persistedUserConflict = record.TargetID > 0 && record.ChangedFields == "username,email"
		case record.Action == "project.update" && !record.Success && record.FailureReason == "protected_project":
			persistedProtectedProject = record.TargetID > 0 && record.ProjectID > 0 && record.ProjectSlug != ""
		case record.Action == "project_api_token.revoke" && !record.Success && record.FailureReason == "not_found":
			persistedCrossProjectToken = record.TargetID > 0 && record.ProjectID > 0 && record.ProjectSlug != ""
		}
	}
	if !persistedUserConflict || !persistedProtectedProject || !persistedCrossProjectToken {
		t.Errorf(
			"missing persisted management failure audit facts: user_conflict=%t protected_project=%t cross_project_token=%t",
			persistedUserConflict,
			persistedProtectedProject,
			persistedCrossProjectToken,
		)
	}
}
