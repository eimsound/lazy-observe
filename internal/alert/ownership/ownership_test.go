package ownership

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestOwnerGoldenVectors(t *testing.T) {
	members := []MemberID{"engine-a", "engine-b", "engine-c"}
	tests := []struct {
		key  RuleKey
		want MemberID
	}{
		{key: RuleKey{ProjectID: 1, Signal: "metrics", RuleID: 1}, want: "engine-a"},
		{key: RuleKey{ProjectID: 1, Signal: "logs", RuleID: 1}, want: "engine-b"},
		{key: RuleKey{ProjectID: 7, Signal: "metrics", RuleID: 42}, want: "engine-b"},
		{key: RuleKey{ProjectID: 99, Signal: "logs", RuleID: 1001}, want: "engine-a"},
	}
	for _, test := range tests {
		got, err := Owner(test.key, members)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("Owner(%+v)=%q want=%q", test.key, got, test.want)
		}
	}
}

func TestOwnerIgnoresMembershipOrderAndDuplicates(t *testing.T) {
	key := RuleKey{ProjectID: 17, Signal: "metrics", RuleID: 23}
	members := []MemberID{"engine-a", "engine-b", "engine-c", "engine-d"}
	want, err := Owner(key, members)
	if err != nil {
		t.Fatal(err)
	}
	for seed := range uint64(100) {
		shuffled := slices.Clone(members)
		rand.New(rand.NewPCG(seed, seed+1)).Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		shuffled = append(shuffled, shuffled[0], shuffled[1])
		got, err := Owner(key, shuffled)
		if err != nil || got != want {
			t.Fatalf("seed=%d Owner=%q err=%v want=%q members=%v", seed, got, err, want, shuffled)
		}
	}
}

func TestOwnerHasMinimalMembershipMovement(t *testing.T) {
	base := []MemberID{"engine-a", "engine-b", "engine-c"}
	joined := append(slices.Clone(base), "engine-d")
	for i := 1; i <= 1000; i++ {
		key := RuleKey{ProjectID: 1 + i%11, Signal: []string{"metrics", "logs"}[i%2], RuleID: i}
		before, err := Owner(key, base)
		if err != nil {
			t.Fatal(err)
		}
		afterJoin, err := Owner(key, joined)
		if err != nil {
			t.Fatal(err)
		}
		if afterJoin != before && afterJoin != "engine-d" {
			t.Fatalf("join moved %+v from %q to existing member %q", key, before, afterJoin)
		}
		afterRemove, err := Owner(key, base[1:])
		if err != nil {
			t.Fatal(err)
		}
		if before != "engine-a" && afterRemove != before {
			t.Fatalf("removal moved unaffected %+v from %q to %q", key, before, afterRemove)
		}
	}
}

func TestOwnerValidation(t *testing.T) {
	tests := []struct {
		name    string
		key     RuleKey
		members []MemberID
		wantErr error
	}{
		{name: "no members", key: RuleKey{ProjectID: 1, Signal: "metrics", RuleID: 1}, wantErr: ErrNoMembers},
		{name: "bad project", key: RuleKey{Signal: "metrics", RuleID: 1}, members: []MemberID{"engine-a"}},
		{name: "bad signal", key: RuleKey{ProjectID: 1, Signal: "traces", RuleID: 1}, members: []MemberID{"engine-a"}},
		{name: "bad rule", key: RuleKey{ProjectID: 1, Signal: "logs"}, members: []MemberID{"engine-a"}},
		{name: "bad member", key: RuleKey{ProjectID: 1, Signal: "logs", RuleID: 1}, members: []MemberID{" engine-a"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Owner(test.key, test.members)
			if err == nil || test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("Owner error=%v want=%v", err, test.wantErr)
			}
		})
	}
	if err := MemberID(fmt.Sprintf("engine-%0250d", 1)).Validate(); err == nil {
		t.Fatal("oversized member ID accepted")
	}
}
