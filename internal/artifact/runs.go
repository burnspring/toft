package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// prefixDeleter removes every object under one key prefix. Optional, and
// reached through DeleteAppRuns, never a bare assertion: the telemetry
// decorator hides it.
type prefixDeleter interface {
	deletePrefix(ctx context.Context, prefix string) (int, error)
}

// AppRunsPrefix is everything belonging to one app's job runs — the parent of
// every RunPrefix.
func AppRunsPrefix(appID string) string {
	return "runs/" + appID + "/"
}

// DeleteAppRuns removes every run object an app left in the store: its runs'
// outputs. The rows that pointed at them cascade away with the app, so after
// the delete nothing else could ever find them. A store with nothing to
// remove, or with no prefix delete, answers 0.
func DeleteAppRuns(ctx context.Context, s Store, appID string) (int, error) {
	if appID == "" || strings.ContainsAny(appID, "/\\") {
		return 0, fmt.Errorf("artifact: refusing to delete the runs of app %q", appID)
	}
	if i, ok := s.(instrumented); ok {
		s = i.Store
	}
	d, ok := s.(prefixDeleter)
	if !ok || s == nil {
		return 0, nil
	}
	return d.deletePrefix(ctx, AppRunsPrefix(appID))
}

func (s *LocalStore) deletePrefix(ctx context.Context, prefix string) (int, error) {
	dir, err := safeJoin(s.root, prefix)
	if err != nil {
		return 0, err
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return 1, nil
}

func (s *S3Store) deletePrefix(ctx context.Context, prefix string) (int, error) {
	full := s.objectKey(prefix)
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(full),
	})
	removed := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return removed, mapS3Error(err, full)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil || !strings.HasPrefix(*obj.Key, full) {
				continue
			}
			if err := s.Delete(ctx, (*obj.Key)[len(s.prefix):]); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

func (s *AzBlobStore) deletePrefix(ctx context.Context, prefix string) (int, error) {
	full := s.objectKey(prefix)
	pager := s.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &full})
	removed := 0
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return removed, mapAzError(err, full)
		}
		for _, item := range page.Segment.BlobItems {
			if item.Name == nil || !strings.HasPrefix(*item.Name, full) {
				continue
			}
			if err := s.Delete(ctx, strings.TrimPrefix(*item.Name, s.prefix)); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}
