package matcher

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCompileAndMatchAnchoredSemantics(t *testing.T) {
	set, err := CompileSet([]Spec{
		{Name: "severity", Op: "=", Value: "critical"},
		{Name: "instance", Op: "=~", Value: `web-\d+`},
	})
	if err != nil {
		t.Fatalf("CompileSet failed: %v", err)
	}
	if set.Len() != 2 {
		t.Fatalf("Len()=%d want 2", set.Len())
	}
	if !set.Matches(map[string]string{"severity": "critical", "instance": "web-42"}) {
		t.Fatal("expected exact and anchored regex match to succeed")
	}
	// Substring must not match because regex is anchored as ^(?:...)$.
	if set.Matches(map[string]string{"severity": "critical", "instance": "pre-web-42"}) {
		t.Fatal("unanchored prefix unexpectedly matched")
	}
	if set.Matches(map[string]string{"severity": "critical", "instance": "web-42-extra"}) {
		t.Fatal("unanchored suffix unexpectedly matched")
	}
	if set.Matches(map[string]string{"severity": "warning", "instance": "web-42"}) {
		t.Fatal("mismatched equality label unexpectedly matched")
	}
	if set.Matches(map[string]string{"severity": "critical"}) {
		t.Fatal("missing label unexpectedly matched")
	}
	var empty Set
	if empty.Matches(map[string]string{"severity": "critical"}) {
		t.Fatal("empty matcher set must never match")
	}
}

func TestValidationRejectsInvalidAndOversizedMatchers(t *testing.T) {
	cases := []struct {
		name  string
		specs []Spec
	}{
		{name: "empty", specs: nil},
		{name: "blank name", specs: []Spec{{Name: "  ", Op: "=", Value: "x"}}},
		{name: "unsupported op", specs: []Spec{{Name: "alertname", Op: "!=", Value: "x"}}},
		{name: "invalid regex", specs: []Spec{{Name: "alertname", Op: "=~", Value: "[unclosed"}}},
		{
			name:  "overlong regex",
			specs: []Spec{{Name: "alertname", Op: "=~", Value: strings.Repeat("a", MaxRegexLength+1)}},
		},
		{
			name: "too many matchers",
			specs: func() []Spec {
				out := make([]Spec, MaxMatchers+1)
				for i := range out {
					out[i] = Spec{Name: fmt.Sprintf("k%d", i), Op: "=", Value: "v"}
				}
				return out
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSet(tc.specs); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestRegexCacheReusesCompiledAndEvictsLRU(t *testing.T) {
	cache := NewRegexCache(2)
	re1, err := cache.Compile(`api-\d+`)
	if err != nil {
		t.Fatal(err)
	}
	re2, err := cache.Compile(`api-\d+`)
	if err != nil {
		t.Fatal(err)
	}
	if re1 != re2 {
		t.Fatal("expected cached *regexp.Regexp pointer reuse")
	}
	stats := cache.Stats()
	if stats.Misses != 1 || stats.Hits != 1 || stats.Entries != 1 {
		t.Fatalf("unexpected stats after hit: %+v", stats)
	}

	if _, err := cache.Compile(`db-\d+`); err != nil {
		t.Fatal(err)
	}
	// Touch api-\d+ so db-\d+ becomes least recently used.
	if _, err := cache.Compile(`api-\d+`); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Compile(`cache-\d+`); err != nil {
		t.Fatal(err)
	}
	stats = cache.Stats()
	if stats.Entries != 2 || stats.Capacity != 2 {
		t.Fatalf("cache did not enforce capacity bound: %+v", stats)
	}
	// api-\d+ should still be cached (hit); db-\d+ was evicted (miss).
	before := cache.Stats()
	if _, err := cache.Compile(`api-\d+`); err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Hits != before.Hits+1 {
		t.Fatalf("expected LRU-retained pattern to hit cache: before=%+v after=%+v", before, cache.Stats())
	}
	before = cache.Stats()
	if _, err := cache.Compile(`db-\d+`); err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Misses != before.Misses+1 {
		t.Fatalf("expected evicted pattern to miss cache: before=%+v after=%+v", before, cache.Stats())
	}
}

func TestRegexCacheConcurrentSafety(t *testing.T) {
	cache := NewRegexCache(16)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 50 {
				pat := fmt.Sprintf("svc-%d-.*", (worker+i)%24)
				re, err := cache.Compile(pat)
				if err != nil || !re.MatchString(fmt.Sprintf("svc-%d-ok", (worker+i)%24)) {
					t.Errorf("concurrent compile/match failed for %q: err=%v", pat, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if stats := cache.Stats(); stats.Entries > 16 {
		t.Fatalf("cache entries %d exceeded capacity 16", stats.Entries)
	}
}

func TestInhibitionRuleCompilationAndEvaluation(t *testing.T) {
	if _, err := CompileInhibitionRule(
		[]Spec{{Name: "alertname", Op: "=", Value: "HostDown"}},
		[]Spec{{Name: "alertname", Op: "=", Value: "ServiceDown"}},
		[]string{" "},
	); err == nil {
		t.Fatal("expected blank equal_label to fail validation")
	}

	rule, err := CompileInhibitionRule(
		[]Spec{{Name: "alertname", Op: "=", Value: "HostDown"}},
		[]Spec{{Name: "alertname", Op: "=~", Value: "Service.*"}},
		[]string{"instance"},
	)
	if err != nil {
		t.Fatal(err)
	}
	target := AlertRef{
		Fingerprint: "target-1",
		Status:      "firing",
		Labels:      map[string]string{"alertname": "ServiceDown", "instance": "node-1"},
	}
	sources := []AlertRef{
		{
			Fingerprint: "src-resolved",
			Status:      "resolved",
			Labels:      map[string]string{"alertname": "HostDown", "instance": "node-1"},
		},
		{
			Fingerprint: "src-other",
			Status:      "firing",
			Labels:      map[string]string{"alertname": "HostDown", "instance": "node-2"},
		},
	}
	if IsInhibited(target, sources, []InhibitionRule{rule}) {
		t.Fatal("target should not be inhibited by resolved or mismatched source")
	}
	sources = append(sources, AlertRef{
		Fingerprint: "src-match",
		Status:      "firing",
		Labels:      map[string]string{"alertname": "HostDown", "instance": "node-1"},
	})
	if !IsInhibited(target, sources, []InhibitionRule{rule}) {
		t.Fatal("target should be inhibited by active matching source with equal label")
	}
}
