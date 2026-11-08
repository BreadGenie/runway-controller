package store

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// BucketProvider supplies current bucket credentials without putting them in
// grants or journals. Tenants enumerates every bucket to reset on a cold start.
// Implementations must be safe for concurrent calls and honor context deadlines.
type BucketProvider interface {
	Tenants(context.Context) ([]int64, error)
	Bucket(context.Context, int64) (GarageConfig, error)
}

func (store *garageStore) route(ctx context.Context, key string) (*garageStore, error) {
	part, _, ok := strings.Cut(strings.TrimPrefix(key, store.prefix), "/")
	tenant, err := strconv.ParseInt(part, 10, 64)
	if !strings.HasPrefix(key, store.prefix) || !ok || err != nil || tenant <= 0 || tenant > 4294967295 || strconv.FormatInt(tenant, 10) != part {
		return nil, errors.New("invalid managed object scope")
	}
	config, err := store.provider.Bucket(ctx, tenant)
	if err != nil {
		return nil, errors.New("team bucket unavailable")
	}
	if config.validate() != nil || !strings.HasPrefix(config.Endpoint, "https://") {
		return nil, errors.New("invalid team bucket configuration")
	}
	return newGarageStore(config, store.budget), nil
}

// reset deletes only the configured service prefix, including abandoned uploads.
// It completes before the first journal is written; a failed reset is retried
// on the next startup. Listing is paginated and every returned key is checked.
func (store *garageStore) reset(ctx context.Context) error {
	tenants, err := store.provider.Tenants(ctx)
	if err != nil {
		return errors.New("team bucket enumeration unavailable")
	}
	seen := map[int64]bool{}
	for _, tenant := range tenants {
		if seen[tenant] {
			continue
		}
		seen[tenant] = true
		target, err := store.route(ctx, store.prefix+strconv.FormatInt(tenant, 10)+"/0")
		if err != nil {
			return err
		}
		err = target.clearPrefix(ctx, store.prefix)
		target.close()
		if err != nil {
			return errors.New("managed cache reset failed")
		}
	}
	return nil
}

func (store *garageStore) clearPrefix(ctx context.Context, prefix string) error {
	var token *string
	for {
		page, err := store.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &store.bucket, Prefix: &prefix, ContinuationToken: token, MaxKeys: aws.Int32(1000)})
		if err != nil {
			return err
		}
		for _, object := range page.Contents {
			if object.Key == nil || !strings.HasPrefix(*object.Key, prefix) {
				return errors.New("storage returned a foreign prefix")
			}
			if err := store.delete(ctx, *object.Key); err != nil {
				return err
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			return nil
		}
		if page.NextContinuationToken == nil || aws.ToString(page.NextContinuationToken) == aws.ToString(token) {
			return errors.New("invalid storage pagination")
		}
		token = page.NextContinuationToken
	}
}
