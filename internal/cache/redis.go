package cache

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Redis struct {
	client *redis.Client
}

func NewRedis(url string) (*Redis, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	client := redis.NewClient(opts)

	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return &Redis{client: client}, nil
}

func (r *Redis) Close() error {
	return r.client.Close()
}

func (r *Redis) SetTTL(ctx context.Context, remote, path string, ttl time.Duration) error {
	key := fmt.Sprintf("ttl:%s:%s", remote, path)
	return r.client.Set(ctx, key, "1", ttl).Err()
}

func (r *Redis) CheckTTL(ctx context.Context, remote, path string) (bool, error) {
	key := fmt.Sprintf("ttl:%s:%s", remote, path)
	exists, err := r.client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return exists > 0, nil
}

// AcquireLock returns the owner token on success and "" when the lock is held.
func (r *Redis) AcquireLock(ctx context.Context, remote, path string, ttl time.Duration) (string, error) {
	key := fmt.Sprintf("lock:%s:%s", remote, path)
	token := rand.Text()
	ok, err := r.client.SetNX(ctx, key, token, ttl).Result()
	if err != nil || !ok {
		return "", err
	}
	return token, nil
}

var releaseScript = redis.NewScript(`if redis.call('get',KEYS[1])==ARGV[1] then return redis.call('del',KEYS[1]) else return 0 end`)

// ReleaseLock deletes the lock only while token still owns it.
func (r *Redis) ReleaseLock(ctx context.Context, remote, path, token string) error {
	key := fmt.Sprintf("lock:%s:%s", remote, path)
	return releaseScript.Run(ctx, r.client, []string{key}, token).Err()
}

// SetETag stores etag for path, or drops the stored one when etag is empty.
func (r *Redis) SetETag(ctx context.Context, remote, path, etag string, ttl time.Duration) error {
	key := fmt.Sprintf("etag:%s:%s", remote, path)
	if etag == "" {
		return r.client.Del(ctx, key).Err()
	}
	return r.client.Set(ctx, key, etag, ttl).Err()
}

func (r *Redis) GetETag(ctx context.Context, remote, path string) (string, error) {
	key := fmt.Sprintf("etag:%s:%s", remote, path)
	val, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return val, err
}

// FirstSeen records now as the first sighting of path unless one is already
// stored, and returns the stored sighting.
func (r *Redis) FirstSeen(ctx context.Context, remote, path string, ttl time.Duration) (time.Time, error) {
	key := fmt.Sprintf("firstseen:%s:%s", remote, path)
	if err := r.client.SetNX(ctx, key, time.Now().Unix(), ttl).Err(); err != nil {
		return time.Time{}, err
	}
	sec, err := r.client.Get(ctx, key).Int64()
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}

func (r *Redis) GetToken(ctx context.Context, key string) (string, error) {
	val, err := r.client.Get(ctx, "token:"+key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return val, err
}

func (r *Redis) SetToken(ctx context.Context, key, token string, ttl time.Duration) error {
	return r.client.Set(ctx, "token:"+key, token, ttl).Err()
}

func (r *Redis) IncrCircuitFailure(ctx context.Context, remote string, cooldown time.Duration) (int64, error) {
	key := fmt.Sprintf("circuit:%s", remote)
	pipe := r.client.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, cooldown)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

func (r *Redis) ResetCircuit(ctx context.Context, remote string) error {
	key := fmt.Sprintf("circuit:%s", remote)
	return r.client.Del(ctx, key).Err()
}

func (r *Redis) GetCircuitFailures(ctx context.Context, remote string) (int64, error) {
	key := fmt.Sprintf("circuit:%s", remote)
	val, err := r.client.Get(ctx, key).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	return val, err
}

func (r *Redis) FlushRemote(ctx context.Context, remote string) error {
	iter := r.client.Scan(ctx, 0, fmt.Sprintf("*:%s:*", remote), 100).Iterator()
	for iter.Next(ctx) {
		r.client.Del(ctx, iter.Val())
	}
	return iter.Err()
}

// ForgetPath drops the freshness and ETag keys of one cached path.
func (r *Redis) ForgetPath(ctx context.Context, remote, path string) error {
	return r.client.Del(ctx, fmt.Sprintf("ttl:%s:%s", remote, path), fmt.Sprintf("etag:%s:%s", remote, path)).Err()
}

// ForgetPrefix drops the freshness and ETag keys of every cached path under prefix.
func (r *Redis) ForgetPrefix(ctx context.Context, remote, prefix string) error {
	glob := globEscaper.Replace(remote + ":" + prefix)
	for _, kind := range []string{"ttl:", "etag:"} {
		iter := r.client.Scan(ctx, 0, kind+glob+"*", 100).Iterator()
		for iter.Next(ctx) {
			if err := r.client.Del(ctx, iter.Val()).Err(); err != nil {
				return err
			}
		}
		if err := iter.Err(); err != nil {
			return err
		}
	}
	return nil
}

var globEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`)

const storeHealthKey = "s3:health"

// SetStoreHealth records the object store state probed by this replica.
func (r *Redis) SetStoreHealth(ctx context.Context, up bool, ttl time.Duration) error {
	val := "down"
	if up {
		val = "up"
	}
	return r.client.Set(ctx, storeHealthKey, val, ttl).Err()
}

// StoreUp reports the probed object store state; a missing key or a Redis error counts as up.
func (r *Redis) StoreUp(ctx context.Context) bool {
	val, err := r.client.Get(ctx, storeHealthKey).Result()
	return err != nil || val != "down"
}
