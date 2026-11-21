package store

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// BucketProvider supplies current bucket credentials without putting them in
// grants or journals. Only signed tenant identities select buckets.
// Implementations must be safe for concurrent calls and honor context deadlines.
type BucketProvider interface {
	Bucket(context.Context, int64) (GarageConfig, error)
}

func managedObjectScope(key string) (string, int64) {
	root, rest, _ := strings.Cut(key, "/")
	namespace, rest, _ := strings.Cut(rest, "/")
	part, _, ok := strings.Cut(rest, "/")
	prefix := root + "/" + namespace + "/"
	tenant, err := strconv.ParseInt(part, 10, 64)
	if root != "managed" || !validNamespace(prefix, "managed/") || !ok || err != nil || tenant <= 0 || tenant > 4294967295 || strconv.FormatInt(tenant, 10) != part {
		return "", 0
	}
	return prefix, tenant
}

func (store *garageStore) route(ctx context.Context, key string) (*garageStore, error) {
	namespace, tenant := managedObjectScope(key)
	if tenant == 0 {
		return nil, errors.New("invalid managed object scope")
	}
	config, err := store.teamBucket(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var protected map[string]bool
	if store.protectedNamespaces == nil {
		store.touch(config, namespace, key, nil)
	} else if protected = store.protectedNamespaces(tenant); protected != nil {
		store.touch(config, namespace, key, protected)
	}
	return newGarageStore(config, store.budget), nil
}

func (store *garageStore) teamBucket(ctx context.Context, tenant int64) (GarageConfig, error) {
	config, err := store.provider.Bucket(ctx, tenant)
	if err != nil {
		return GarageConfig{}, errors.New("team bucket unavailable")
	}
	if config.validate() != nil || !strings.HasPrefix(config.Endpoint, "https://") {
		return GarageConfig{}, errors.New("invalid team bucket configuration")
	}
	return config, nil
}

// Auth failures never use stale credentials on the next operation. Failed PUTs
// are not replayed automatically: the caller can retry with a fresh reservation.
func (store *garageStore) invalidateAuth(key string, err error) {
	_, tenant := managedObjectScope(key)
	store.invalidateTeamAuth(tenant, err)
}

func (store *garageStore) invalidateTeamAuth(tenant int64, err error) {
	var api smithy.APIError
	var response interface{ HTTPStatusCode() int }
	auth := errors.As(err, &response) && (response.HTTPStatusCode() == 401 || response.HTTPStatusCode() == 403)
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken", "InvalidToken":
			auth = true
		}
	}
	if !auth {
		return
	}
	if provider, ok := store.provider.(interface{ Invalidate(int64) }); ok {
		provider.Invalidate(tenant)
	}
}

// Maintenance is bounded independently of request latency. A
// process only remembers a bounded set of buckets; eviction merely retries work.
func (store *garageStore) touch(config GarageConfig, namespace, objectKey string, protected map[string]bool) {
	store.maintenanceMu.Lock()
	defer store.maintenanceMu.Unlock()
	now := time.Now()
	key := config.Endpoint + "/" + config.Bucket
	if store.maintenanceClosed || store.maintenanceActive >= 256 || store.maintenanceRunning[key] || now.Before(store.maintenance[key]) {
		return
	}
	if len(store.maintenance) >= 256 {
		for k := range store.maintenance {
			if !store.maintenanceRunning[k] {
				delete(store.maintenance, k)
			}
		}
	}
	store.maintenance[key] = now.Add(time.Hour)
	store.maintenanceRunning[key] = true
	store.maintenanceActive++
	store.maintenanceWG.Add(1)
	go func() {
		defer store.maintenanceWG.Done()
		defer func() {
			store.maintenanceMu.Lock()
			store.maintenanceActive--
			delete(store.maintenanceRunning, key)
			store.maintenanceMu.Unlock()
		}()
		target := newGarageStore(config, store.budget)
		defer target.close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		marker := namespace + ".owner"
		// Object timestamps also bound writes after local cache eviction.
		owner, err := target.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &target.bucket, Key: &marker})
		if err != nil && !lostArchive(err) {
			store.invalidateAuth(objectKey, err)
			return
		}
		if err != nil || owner.LastModified == nil || !owner.LastModified.After(now.Add(-time.Hour)) {
			_, err = target.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &target.bucket, Key: &marker, Body: bytes.NewReader([]byte(now.UTC().Format(time.RFC3339)))})
			if err != nil {
				store.invalidateAuth(objectKey, err)
				return
			}
		}

		stale := store.staleAfter
		if stale == 0 {
			stale = 24 * time.Hour
		}
		count, err := target.cleanup(ctx, namespace, now.Add(-stale), protected)
		store.invalidateAuth(objectKey, err)
		if err == nil && count > 0 {
			log.Printf("managed cache cleanup deleted %d objects", count)
		}
	}()
}

// Limit each pass to 1000 deletions, in batches of at most 100. Unknown or
// incomplete timestamps fail closed. Recheck the owner before each batch.
func (store *garageStore) cleanup(ctx context.Context, current string, cutoff time.Time, protected map[string]bool) (int, error) {
	root, delimiter := "managed/", "/"
	var token *string
	count := 0
	for {
		page, err := store.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &store.bucket, Prefix: &root, Delimiter: &delimiter, ContinuationToken: token, MaxKeys: aws.Int32(100)})
		if err != nil {
			return count, err
		}
		for _, group := range page.CommonPrefixes {
			prefix := aws.ToString(group.Prefix)
			id := strings.TrimSuffix(strings.TrimPrefix(prefix, root), "/")
			if prefix == current || prefix == checkpointPrefix || protected[prefix] || id == "" || strings.Contains(id, "/") || prefix != root+id+"/" {
				continue
			}
			n, err := store.cleanupPrefix(ctx, prefix, cutoff, 1000-count)
			count += n
			if err != nil || count >= 1000 {
				return count, err
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			return count, nil
		}
		if page.NextContinuationToken == nil || aws.ToString(page.NextContinuationToken) == aws.ToString(token) {
			return count, errors.New("invalid storage pagination")
		}
		token = page.NextContinuationToken
	}
}

func (store *garageStore) cleanupPrefix(ctx context.Context, prefix string, cutoff time.Time, limit int) (int, error) {
	count := 0
	for count < limit {
		// List a bounded batch first, including the marker if present. With no
		// marker, every object in the prefix must be old, across every page.
		var token *string
		keys := []string{}
		for {
			page, err := store.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &store.bucket, Prefix: &prefix, ContinuationToken: token, MaxKeys: aws.Int32(100)})
			if err != nil {
				return count, err
			}
			for _, object := range page.Contents {
				key := aws.ToString(object.Key)
				if !strings.HasPrefix(key, prefix) || object.LastModified == nil || !object.LastModified.Before(cutoff) {
					return count, nil
				}
				if len(keys) < 100 && len(keys) < limit-count {
					keys = append(keys, key)
				}
			}
			if !aws.ToBool(page.IsTruncated) {
				break
			}
			if page.NextContinuationToken == nil || aws.ToString(page.NextContinuationToken) == aws.ToString(token) {
				return count, errors.New("invalid storage pagination")
			}
			token = page.NextContinuationToken
		}
		if len(keys) == 0 {
			return count, nil
		}
		marker := prefix + ".owner"
		owner, err := store.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &store.bucket, Key: &marker})
		if err == nil {
			if owner.LastModified == nil || !owner.LastModified.Before(cutoff) {
				return count, nil
			}
		} else if !lostArchive(err) {
			return count, err
		}
		for _, key := range keys {
			if err := store.delete(ctx, key); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}
