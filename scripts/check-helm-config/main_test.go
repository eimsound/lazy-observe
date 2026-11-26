package main

import (
	"strings"
	"testing"
)

const validStandaloneManifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-lazy-observe
data:
  config.yaml: |
    listen: ":3100"
    data_dir: /data
    standalone: true
    meta:
      db_url: ""
      migration_lock_timeout: 60s
    redis:
      enabled: true
      addr: "redis:6379"
      password: ""
      db: 0
      key_prefix: "lazy-observe"
    storage:
      metrics:
        kind: "vm"
        cluster: true
        tenant_account_id: "42"
        query_url: "http://vmselect:8481"
        ingest_url: "http://vminsert:8480"
      logs:
        url: "http://vlogs:9428"
      traces:
        url: "http://vtraces:10428"
    blob:
      backend: s3
      s3:
        bucket: "lazy-observe"
        region: "us-east-1"
        endpoint: "http://s3:8333"
        access_key: ""
        secret_key: ""
    process:
      startup_timeout: 30s
      shutdown_timeout: 20s
      drain_timeout: 25s
    security:
      session_secure_cookie: true
      secret_key_file: "/etc/lazy-observe-key/secret.key"
      admin_password_file: "/etc/lazy-observe-admin/admin-password"
    self_monitoring:
      enabled: true
      web: true
      web_replay: false
      profiling: false
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-lazy-observe
spec:
  template:
    spec:
      containers:
        - name: lazy-observe
          env:
            - name: LO_META_DB_URL
            - name: LO_REDIS_PASSWORD
            - name: LO_BLOB_S3_ACCESS_KEY
            - name: LO_BLOB_S3_SECRET_KEY
`

const validAllinoneManifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-lazy-observe
data:
  config.yaml: |
    listen: ":3100"
    data_dir: /data
    standalone: false
    meta:
      db_url: ""
      migration_lock_timeout: 60s
    redis:
      enabled: false
      addr: "redis:6379"
      password: ""
      db: 0
      key_prefix: "lazy-observe"
    storage:
      metrics:
        kind: "vm"
        cluster: false
        tenant_account_id: "0"
        query_url: ""
        ingest_url: ""
      logs:
        url: ""
      traces:
        url: ""
    blob:
      backend: local
      s3:
        bucket: "lazy-observe"
        region: "us-east-1"
        endpoint: ""
        access_key: ""
        secret_key: ""
    process:
      startup_timeout: 20s
      shutdown_timeout: 15s
      drain_timeout: 20s
    security:
      session_secure_cookie: false
    self_monitoring:
      enabled: true
      web: true
      web_replay: false
      profiling: false
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-lazy-observe
spec:
  template:
    spec:
      containers:
        - name: lazy-observe
`

func TestValidateManifestBytesValid(t *testing.T) {
	if err := validateManifestBytes("standalone.yaml", []byte(validStandaloneManifest)); err != nil {
		t.Fatalf("expected valid standalone manifest to pass, got: %v", err)
	}
	if err := validateManifestBytes("allinone.yaml", []byte(validAllinoneManifest)); err != nil {
		t.Fatalf("expected valid allinone manifest to pass, got: %v", err)
	}
}

func TestValidateManifestBytesRejectsUnknownField(t *testing.T) {
	drifted := strings.Replace(validStandaloneManifest, "session_secure_cookie: true", "secure_cookie: true", 1)
	err := validateManifestBytes("drifted.yaml", []byte(drifted))
	if err == nil {
		t.Fatal("expected unknown field in ConfigMap config.yaml to fail")
	}
	if !strings.Contains(err.Error(), "secure_cookie") {
		t.Fatalf("expected error to mention drifted field name, got: %v", err)
	}
}

func TestValidateManifestBytesRejectsInvalidValue(t *testing.T) {
	invalid := strings.Replace(validStandaloneManifest, `url: "http://vlogs:9428"`, `url: ""`, 1)
	err := validateManifestBytes("invalid.yaml", []byte(invalid))
	if err == nil {
		t.Fatal("expected empty logs.url in standalone mode to fail Validate()")
	}
}

func TestValidateManifestBytesRejectsUnexpectedEnvVar(t *testing.T) {
	driftedEnv := strings.Replace(validStandaloneManifest, "LO_REDIS_PASSWORD", "LO_SECURITY_SECRET_KEY", 1)
	err := validateManifestBytes("drifted-env.yaml", []byte(driftedEnv))
	if err == nil {
		t.Fatal("expected unexpected LO_* env var in Deployment to fail")
	}
	if !strings.Contains(err.Error(), "LO_SECURITY_SECRET_KEY") {
		t.Fatalf("expected error to mention unexpected env var, got: %v", err)
	}
}

func TestValidateManifestBytesRejectsMissingConfigMap(t *testing.T) {
	err := validateManifestBytes("empty.yaml", []byte("apiVersion: v1\nkind: Service\n"))
	if err == nil {
		t.Fatal("expected manifest without ConfigMap to fail")
	}
}

func TestValidateManifestBytesErrorInboxReceiptRetention(t *testing.T) {
	withReceipt := strings.Replace(
		validStandaloneManifest,
		"      drain_timeout: 25s\n",
		"      drain_timeout: 25s\n    error_inbox:\n      receipt_retention: \"30d\"\n",
		1,
	)
	if withReceipt == validStandaloneManifest {
		t.Fatal("fixture replacement did not apply")
	}
	if err := validateManifestBytes("receipt.yaml", []byte(withReceipt)); err != nil {
		t.Fatalf("expected error_inbox.receipt_retention to pass config.Load, got: %v", err)
	}
	negative := strings.Replace(withReceipt, `receipt_retention: "30d"`, `receipt_retention: "-1h"`, 1)
	err := validateManifestBytes("receipt-negative.yaml", []byte(negative))
	if err == nil || !strings.Contains(err.Error(), "error_inbox.receipt_retention") {
		t.Fatalf("expected negative error_inbox.receipt_retention to fail Validate(), got: %v", err)
	}
}
