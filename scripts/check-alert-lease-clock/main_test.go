package main

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestCheckFile(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "variable wrapped by clock helper",
			body: "query := `UPDATE alert_rule_leases SET expires_at = ` + expiry + ` WHERE expires_at > CURRENT_TIMESTAMP`\n" +
				"if pg { query += ` FOR UPDATE` }\n" +
				"db.Exec(s.alertClockSQL(query))",
		},
		{
			name: "literal passed straight to clock helper",
			body: "db.Exec(s.alertClockSQL(`DELETE FROM alert_engine_members WHERE expires_at <= CURRENT_TIMESTAMP`))",
		},
		{
			name: "lease query without clock is not flagged",
			body: "db.Exec(bind(d, `DELETE FROM alert_rule_leases WHERE rule_id = ?`))",
		},
		{
			name: "unrelated tables keep plain bind",
			body: "db.Exec(bind(d, `UPDATE alert_rules SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`))\n" +
				"db.Exec(bind(d, `UPDATE notification_outbox SET claim_until = datetime('now', ?) WHERE available_at <= CURRENT_TIMESTAMP`))",
		},
		{
			name: "variable bound without clock helper",
			body: "query := `SELECT 1 FROM alert_engine_members WHERE expires_at > CURRENT_TIMESTAMP`\n" +
				"db.Exec(bind(d, query))",
			want: []string{"used without alertClockSQL"},
		},
		{
			name: "variable that never reaches the clock helper",
			body: "var query string\n" +
				"query = `SELECT 1 FROM alert_engine_members WHERE expires_at > CURRENT_TIMESTAMP`",
			want: []string{"never reaches alertClockSQL"},
		},
		{
			name: "literal bound without clock helper",
			body: "db.Exec(bind(d, `SELECT 1 FROM alert_rule_leases WHERE expires_at > CURRENT_TIMESTAMP`))",
			want: []string{"reads CURRENT_TIMESTAMP without alertClockSQL"},
		},
		{
			name: "second precision expiry",
			body: "db.Exec(s.alertClockSQL(`UPDATE alert_rule_leases SET expires_at = datetime('now', ?) WHERE rule_id = ?`))",
			want: []string{"second-precision datetime('now', ...)"},
		},
		{
			name: "one wrapped use does not excuse an unwrapped one",
			body: "query := `SELECT 1 FROM alert_rule_leases WHERE expires_at > CURRENT_TIMESTAMP`\n" +
				"db.Exec(s.alertClockSQL(query))\n" +
				"db.Exec(bind(d, query))",
			want: []string{"used without alertClockSQL"},
		},
		{
			name: "source_map_upload_pins wrapped by clock helper",
			body: "db.Exec(s.alertClockSQL(`DELETE FROM source_map_upload_pins WHERE expires_at <= CURRENT_TIMESTAMP`))",
		},
		{
			name: "source_map_upload_pins without clock helper",
			body: "db.Exec(bind(d, `SELECT sha256 FROM source_map_upload_pins WHERE expires_at > CURRENT_TIMESTAMP`))",
			want: []string{"reads CURRENT_TIMESTAMP without alertClockSQL"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			src := "package meta\nfunc (s *Store) f() {\n" + tc.body + "\n}\n"
			file, err := parser.ParseFile(fset, "case.go", src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := checkFile(fset, file)
			if len(got) != len(tc.want) {
				t.Fatalf("violations = %v, want %d matching %q", got, len(tc.want), tc.want)
			}
			for i, want := range tc.want {
				if !strings.Contains(got[i].message, want) {
					t.Errorf("violation %d = %q, want it to contain %q", i, got[i].message, want)
				}
			}
		})
	}
}

func TestPackageLevelQueryMustBeWrapped(t *testing.T) {
	fset := token.NewFileSet()
	src := "package meta\nconst q = `SELECT 1 FROM alert_rule_leases WHERE expires_at > CURRENT_TIMESTAMP`\n"
	file, err := parser.ParseFile(fset, "case.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := checkFile(fset, file); len(got) != 1 {
		t.Fatalf("violations = %v, want one", got)
	}
}
