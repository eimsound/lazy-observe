// Package ownership deterministically assigns alert rules to engine members.
// It is a pure ownership kernel: persistence, leases, and fencing live in the
// alert engine and are deliberately not implied by a hash result.
package ownership

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const hashDomain = "lazy-observe-alert-owner-v1"

var ErrNoMembers = errors.New("alert ownership requires at least one member")

// MemberID is the stable identity of one alert-engine process. It must survive
// membership refreshes and must not be derived from slice position.
type MemberID string

func (id MemberID) Validate() error {
	value := string(id)
	if value == "" || strings.TrimSpace(value) != value {
		return errors.New("alert-engine member ID must be non-empty and have no surrounding whitespace")
	}
	if len(value) > 255 {
		return errors.New("alert-engine member ID must not exceed 255 bytes")
	}
	return nil
}

// RuleKey matches the fields available on the current AlertRule model. Signal
// is part of the key so metrics and logs rules cannot alias if IDs evolve.
type RuleKey struct {
	ProjectID int
	Signal    string
	RuleID    int
}

func (key RuleKey) Validate() error {
	if key.ProjectID <= 0 || key.RuleID <= 0 {
		return errors.New("alert ownership project and rule IDs must be positive")
	}
	if key.Signal != "metrics" && key.Signal != "logs" {
		return fmt.Errorf("unsupported alert ownership signal %q", key.Signal)
	}
	return nil
}

// Owner applies rendezvous hashing and returns one candidate owner. Membership
// order and duplicates do not affect the result. A hash winner is only a
// candidate; the caller must acquire a fenced lease before side effects.
func Owner(key RuleKey, members []MemberID) (MemberID, error) {
	if err := key.Validate(); err != nil {
		return "", err
	}
	seen := make(map[MemberID]struct{}, len(members))
	var winner MemberID
	var winnerScore [sha256.Size]byte
	haveWinner := false
	for _, member := range members {
		if err := member.Validate(); err != nil {
			return "", err
		}
		if _, exists := seen[member]; exists {
			continue
		}
		seen[member] = struct{}{}
		score := rendezvousScore(key, member)
		if !haveWinner || bytes.Compare(score[:], winnerScore[:]) > 0 || (score == winnerScore && member < winner) {
			winner, winnerScore, haveWinner = member, score, true
		}
	}
	if !haveWinner {
		return "", ErrNoMembers
	}
	return winner, nil
}

func rendezvousScore(key RuleKey, member MemberID) [sha256.Size]byte {
	hash := sha256.New()
	writeHashString(hash, hashDomain)
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(key.ProjectID))
	_, _ = hash.Write(number[:])
	writeHashString(hash, key.Signal)
	binary.BigEndian.PutUint64(number[:], uint64(key.RuleID))
	_, _ = hash.Write(number[:])
	writeHashString(hash, string(member))
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

type hashWriter interface {
	Write([]byte) (int, error)
}

func writeHashString(writer hashWriter, value string) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write([]byte(value))
}
