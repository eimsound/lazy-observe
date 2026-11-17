package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Redis struct {
	client redis.UniversalClient
	prefix string
}

func NewRedis(client redis.UniversalClient, keyPrefix string) *Redis {
	keyPrefix = strings.Trim(strings.TrimSpace(keyPrefix), ":")
	if keyPrefix == "" {
		keyPrefix = "lazy-observe"
	}
	return &Redis{client: client, prefix: keyPrefix}
}

// Session keys and per-user index members use the token digest so a Redis
// dump or MONITOR stream never exposes usable session tokens.
func (r *Redis) sessionKey(digest string) string { return r.prefix + ":session:" + digest }
func (r *Redis) limitKey(key string) string      { return r.prefix + ":limit:" + key }

func (r *Redis) Create(ctx context.Context, session Session) error {
	ttl := time.Until(session.ExpiresAt)
	if ttl <= 0 {
		return errors.New("session expiry must be in the future")
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now()
	}
	digest := tokenDigest(session.Token)
	stored := session
	stored.Token = ""
	value, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	pipe := r.client.TxPipeline()
	pipe.Set(ctx, r.sessionKey(digest), value, ttl)
	pipe.SAdd(ctx, r.userSessionsKey(session.UserID), digest)
	// Refresh the index TTL on every write; expired sessions drop out of the
	// index lazily when listed.
	pipe.Expire(ctx, r.userSessionsKey(session.UserID), userSessionsIndexTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	return nil
}

func (r *Redis) Get(ctx context.Context, token string) (Session, error) {
	value, err := r.client.Get(ctx, r.sessionKey(tokenDigest(token))).Bytes()
	if errors.Is(err, redis.Nil) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	var session Session
	if err := json.Unmarshal(value, &session); err != nil {
		return Session{}, fmt.Errorf("decode session: %w", err)
	}
	session.Token = token
	return session, nil
}

func (r *Redis) Delete(ctx context.Context, token string) error {
	digest := tokenDigest(token)
	value, err := r.client.Get(ctx, r.sessionKey(digest)).Bytes()
	if errors.Is(err, redis.Nil) {
		return r.client.Del(ctx, r.sessionKey(digest)).Err()
	}
	if err != nil {
		return err
	}
	var session Session
	if err := json.Unmarshal(value, &session); err != nil {
		return r.client.Del(ctx, r.sessionKey(digest)).Err()
	}
	pipe := r.client.TxPipeline()
	pipe.Del(ctx, r.sessionKey(digest))
	pipe.SRem(ctx, r.userSessionsKey(session.UserID), digest)
	_, err = pipe.Exec(ctx)
	return err
}

func (r *Redis) Touch(ctx context.Context, token string, expiresAt time.Time) error {
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return errors.New("session expiry must be in the future")
	}
	key := r.sessionKey(tokenDigest(token))
	value, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var session Session
	if err := json.Unmarshal(value, &session); err != nil {
		return fmt.Errorf("decode session: %w", err)
	}
	session.ExpiresAt = expiresAt
	session.Token = ""
	value, err = json.Marshal(session)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	result, err := r.client.SetArgs(ctx, key, value, redis.SetArgs{Mode: "XX", TTL: ttl}).Result()
	if errors.Is(err, redis.Nil) || result == "" {
		return ErrNotFound
	}
	return err
}

var allowScript = redis.NewScript(`
local current = redis.call('INCR', KEYS[1])
if current == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return current
`)

func (r *Redis) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	if limit <= 0 || window <= 0 {
		return false, errors.New("limit and window must be positive")
	}
	count, err := allowScript.Run(ctx, r.client, []string{r.limitKey(key)}, window.Milliseconds()).Int64()
	if err != nil {
		return false, err
	}
	return count <= int64(limit), nil
}

func (r *Redis) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }

const userSessionsIndexTTL = 8 * 24 * time.Hour

func (r *Redis) userSessionsKey(userID int) string {
	return fmt.Sprintf("%s:user_sessions:%d", r.prefix, userID)
}

func (r *Redis) loginFailKey(key string) string { return r.prefix + ":login_fail:" + key }
func (r *Redis) loginLockKey(key string) string { return r.prefix + ":login_lock:" + key }

func (r *Redis) ListByUser(ctx context.Context, userID int, currentToken string) ([]SessionRef, error) {
	members, err := r.client.SMembers(ctx, r.userSessionsKey(userID)).Result()
	if err != nil {
		return nil, err
	}
	result := make([]SessionRef, 0, len(members))
	stale := make([]string, 0)
	currentDigest := ""
	if currentToken != "" {
		currentDigest = tokenDigest(currentToken)
	}
	for _, digest := range members {
		value, err := r.client.Get(ctx, r.sessionKey(digest)).Bytes()
		if errors.Is(err, redis.Nil) {
			stale = append(stale, digest)
			continue
		}
		if err != nil {
			return nil, err
		}
		var session Session
		if err := json.Unmarshal(value, &session); err != nil {
			stale = append(stale, digest)
			continue
		}
		result = append(result, SessionRef{
			ID:        digestRefID(digest),
			CreatedAt: session.CreatedAt,
			ExpiresAt: session.ExpiresAt,
			Current:   currentDigest != "" && digest == currentDigest,
		})
	}
	if len(stale) > 0 {
		_ = r.client.SRem(ctx, r.userSessionsKey(userID), stale).Err()
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}

func (r *Redis) DeleteByRef(ctx context.Context, userID int, refID string) error {
	members, err := r.client.SMembers(ctx, r.userSessionsKey(userID)).Result()
	if err != nil {
		return err
	}
	for _, digest := range members {
		if digestRefID(digest) != refID {
			continue
		}
		pipe := r.client.TxPipeline()
		pipe.Del(ctx, r.sessionKey(digest))
		pipe.SRem(ctx, r.userSessionsKey(userID), digest)
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
		return nil
	}
	return ErrNotFound
}

func (r *Redis) DeleteOtherByUser(ctx context.Context, userID int, keepToken string) error {
	members, err := r.client.SMembers(ctx, r.userSessionsKey(userID)).Result()
	if err != nil {
		return err
	}
	pipe := r.client.TxPipeline()
	removed := 0
	keepDigest := tokenDigest(keepToken)
	for _, digest := range members {
		if digest == keepDigest {
			continue
		}
		pipe.Del(ctx, r.sessionKey(digest))
		pipe.SRem(ctx, r.userSessionsKey(userID), digest)
		removed++
	}
	if removed > 0 {
		_, err = pipe.Exec(ctx)
	}
	return err
}

var loginFailureScript = redis.NewScript(`
local current = redis.call('INCR', KEYS[1])
if current == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
if current >= tonumber(ARGV[2]) then
  redis.call('SET', KEYS[2], '1', 'PX', ARGV[3])
  return 1
end
return 0
`)

func (r *Redis) ConsumeFailure(
	ctx context.Context,
	key string,
	threshold int,
	window, lockFor time.Duration,
) (bool, error) {
	if key == "" || threshold <= 0 || window <= 0 || lockFor <= 0 {
		return false, errors.New("login gate arguments must be valid")
	}
	locked, err := loginFailureScript.Run(
		ctx,
		r.client,
		[]string{r.loginFailKey(key), r.loginLockKey(key)},
		window.Milliseconds(),
		threshold,
		lockFor.Milliseconds(),
	).Int64()
	if err != nil {
		return false, err
	}
	return locked == 1, nil
}

func (r *Redis) Locked(ctx context.Context, key string) (bool, error) {
	exists, err := r.client.Exists(ctx, r.loginLockKey(key)).Result()
	if err != nil {
		return false, err
	}
	return exists > 0, nil
}

func (r *Redis) Clear(ctx context.Context, key string) error {
	return r.client.Del(ctx, r.loginFailKey(key), r.loginLockKey(key)).Err()
}
