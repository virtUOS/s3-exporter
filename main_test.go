package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakePage is one ListObjectsV2 response for a bucket. If err is true the call
// returns an error instead of objects, simulating a mid-pagination failure.
type fakePage struct {
	objects []types.Object
	err     bool
}

// fakeS3 implements S3API. It is stateless with respect to pagination position:
// the page index is encoded in the continuation token, so concurrent
// per-bucket collection needs no locking.
type fakeS3 struct {
	buckets        []string
	listBucketsErr error
	pages          map[string][]fakePage // bucket -> ordered pages
}

func (f *fakeS3) ListBuckets(_ context.Context, _ *s3.ListBucketsInput, _ ...func(*s3.Options)) (*s3.ListBucketsOutput, error) {
	if f.listBucketsErr != nil {
		return nil, f.listBucketsErr
	}
	out := &s3.ListBucketsOutput{}
	for _, name := range f.buckets {
		out.Buckets = append(out.Buckets, types.Bucket{Name: aws.String(name)})
	}
	return out, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	bucket := aws.ToString(in.Bucket)
	pages := f.pages[bucket]

	idx := 0
	if in.ContinuationToken != nil {
		var err error
		idx, err = strconv.Atoi(aws.ToString(in.ContinuationToken))
		if err != nil {
			return nil, err
		}
	}
	if idx >= len(pages) {
		return &s3.ListObjectsV2Output{}, nil
	}

	page := pages[idx]
	if page.err {
		return nil, errors.New("simulated listing failure")
	}

	out := &s3.ListObjectsV2Output{Contents: page.objects}
	if idx < len(pages)-1 {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(strconv.Itoa(idx + 1))
	}
	return out, nil
}

func obj(size int64, modUnix int64) types.Object {
	t := time.Unix(modUnix, 0).UTC()
	return types.Object{Size: aws.Int64(size), LastModified: &t}
}

func TestCollect_SingleBucketMultiplePages(t *testing.T) {
	fake := &fakeS3{
		buckets: []string{"b1"},
		pages: map[string][]fakePage{
			"b1": {
				{objects: []types.Object{obj(100, 1000), obj(200, 3000)}},
				{objects: []types.Object{obj(50, 2000)}},
			},
		},
	}
	c := NewS3Collector(fake)

	expected := `
# HELP s3_bucket_objects_total Total number of objects in the bucket
# TYPE s3_bucket_objects_total gauge
s3_bucket_objects_total{bucket="b1"} 3
# HELP s3_bucket_size_bytes Total size of all objects in the bucket
# TYPE s3_bucket_size_bytes gauge
s3_bucket_size_bytes{bucket="b1"} 350
# HELP s3_bucket_last_modified_timestamp_seconds Timestamp of the most recently modified object
# TYPE s3_bucket_last_modified_timestamp_seconds gauge
s3_bucket_last_modified_timestamp_seconds{bucket="b1"} 3000
# HELP s3_bucket_scrape_success Whether the most recent scrape fully enumerated this bucket (1) or failed (0)
# TYPE s3_bucket_scrape_success gauge
s3_bucket_scrape_success{bucket="b1"} 1
# HELP s3_exporter_scrape_success Whether the most recent scrape successfully listed buckets (1) or failed (0)
# TYPE s3_exporter_scrape_success gauge
s3_exporter_scrape_success 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

func TestCollect_PartialPaginationFailureOmitsData(t *testing.T) {
	// First page succeeds, second errors. The bucket's count/size/lastModified
	// must NOT be published (no undercount); only success=0 is emitted.
	fake := &fakeS3{
		buckets: []string{"b1"},
		pages: map[string][]fakePage{
			"b1": {
				{objects: []types.Object{obj(100, 1000)}},
				{err: true},
			},
		},
	}
	c := NewS3Collector(fake)

	expected := `
# HELP s3_bucket_scrape_success Whether the most recent scrape fully enumerated this bucket (1) or failed (0)
# TYPE s3_bucket_scrape_success gauge
s3_bucket_scrape_success{bucket="b1"} 0
# HELP s3_exporter_scrape_success Whether the most recent scrape successfully listed buckets (1) or failed (0)
# TYPE s3_exporter_scrape_success gauge
s3_exporter_scrape_success 1
`
	// Compare only the success metrics; the data metrics must be absent.
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"s3_bucket_scrape_success", "s3_exporter_scrape_success"); err != nil {
		t.Fatal(err)
	}

	// Assert the data metrics really were omitted for the failed bucket.
	if n := testutil.CollectAndCount(c, "s3_bucket_objects_total"); n != 0 {
		t.Fatalf("expected no objects_total metric for failed bucket, got %d", n)
	}
	if n := testutil.CollectAndCount(c, "s3_bucket_size_bytes"); n != 0 {
		t.Fatalf("expected no size_bytes metric for failed bucket, got %d", n)
	}
}

func TestCollect_ListBucketsFailure(t *testing.T) {
	fake := &fakeS3{listBucketsErr: errors.New("boom")}
	c := NewS3Collector(fake)

	expected := `
# HELP s3_exporter_scrape_success Whether the most recent scrape successfully listed buckets (1) or failed (0)
# TYPE s3_exporter_scrape_success gauge
s3_exporter_scrape_success 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

func TestCollect_EmptyBucketOmitsLastModified(t *testing.T) {
	// An empty bucket reports zero count/size and no last-modified timestamp.
	fake := &fakeS3{
		buckets: []string{"empty"},
		pages:   map[string][]fakePage{"empty": {{objects: nil}}},
	}
	c := NewS3Collector(fake)

	if n := testutil.CollectAndCount(c, "s3_bucket_last_modified_timestamp_seconds"); n != 0 {
		t.Fatalf("expected no last_modified metric for empty bucket, got %d", n)
	}
	expected := `
# HELP s3_bucket_objects_total Total number of objects in the bucket
# TYPE s3_bucket_objects_total gauge
s3_bucket_objects_total{bucket="empty"} 0
# HELP s3_bucket_size_bytes Total size of all objects in the bucket
# TYPE s3_bucket_size_bytes gauge
s3_bucket_size_bytes{bucket="empty"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"s3_bucket_objects_total", "s3_bucket_size_bytes"); err != nil {
		t.Fatal(err)
	}
}

func TestCollect_OneBucketFailsOthersSucceed(t *testing.T) {
	// Mixed outcome across concurrent buckets: good enumerates, bad fails on
	// its first page. good's data must be present; bad's data absent.
	fake := &fakeS3{
		buckets: []string{"good", "bad"},
		pages: map[string][]fakePage{
			"good": {{objects: []types.Object{obj(10, 5000)}}},
			"bad":  {{err: true}},
		},
	}
	c := NewS3Collector(fake)

	expected := `
# HELP s3_bucket_scrape_success Whether the most recent scrape fully enumerated this bucket (1) or failed (0)
# TYPE s3_bucket_scrape_success gauge
s3_bucket_scrape_success{bucket="bad"} 0
s3_bucket_scrape_success{bucket="good"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"s3_bucket_scrape_success"); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(c, "s3_bucket_objects_total"); n != 1 {
		t.Fatalf("expected exactly one objects_total metric (good only), got %d", n)
	}
}
