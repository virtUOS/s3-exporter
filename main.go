package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// scrapeTimeout bounds a single Collect call (one Prometheus scrape). Keep it
// below your Prometheus scrape_timeout so a stuck backend fails the scrape
// rather than blocking the collector.
const scrapeTimeout = 30 * time.Second

// maxConcurrentBuckets caps how many buckets are enumerated in parallel during
// a scrape. Buckets are independent, so this keeps one huge bucket from
// serializing the rest while bounding the load placed on the S3 backend.
const maxConcurrentBuckets = 8

// S3API is the subset of the S3 client the collector depends on. Depending on
// an interface (rather than *s3.Client) lets tests inject a fake.
type S3API interface {
	ListBuckets(ctx context.Context, params *s3.ListBucketsInput, optFns ...func(*s3.Options)) (*s3.ListBucketsOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

type S3Collector struct {
	client S3API

	objectCount   *prometheus.Desc
	totalSize     *prometheus.Desc
	lastModified  *prometheus.Desc
	scrapeSuccess *prometheus.Desc
	bucketSuccess *prometheus.Desc
}

func NewS3Collector(client S3API) *S3Collector {
	return &S3Collector{
		client: client,
		objectCount: prometheus.NewDesc(
			"s3_bucket_objects_total",
			"Total number of objects in the bucket",
			[]string{"bucket"}, nil,
		),
		totalSize: prometheus.NewDesc(
			"s3_bucket_size_bytes",
			"Total size of all objects in the bucket",
			[]string{"bucket"}, nil,
		),
		lastModified: prometheus.NewDesc(
			"s3_bucket_last_modified_timestamp_seconds",
			"Timestamp of the most recently modified object",
			[]string{"bucket"}, nil,
		),
		scrapeSuccess: prometheus.NewDesc(
			"s3_exporter_scrape_success",
			"Whether the most recent scrape successfully listed buckets (1) or failed (0)",
			nil, nil,
		),
		bucketSuccess: prometheus.NewDesc(
			"s3_bucket_scrape_success",
			"Whether the most recent scrape fully enumerated this bucket (1) or failed (0)",
			[]string{"bucket"}, nil,
		),
	}
}

func (c *S3Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.objectCount
	ch <- c.totalSize
	ch <- c.lastModified
	ch <- c.scrapeSuccess
	ch <- c.bucketSuccess
}

func (c *S3Collector) Collect(ch chan<- prometheus.Metric) {
	// Bound the whole scrape so a slow or hung S3 backend can't block the
	// collector goroutine indefinitely (overlapping scrapes would otherwise
	// pile up additional full enumerations).
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	// 1. Auto-discover all buckets we have access to
	bucketsOutput, err := c.client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		log.Printf("Error listing buckets: %v", err)
		// Expose the failure so it can be alerted on, not just logged.
		ch <- prometheus.MustNewConstMetric(c.scrapeSuccess, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.scrapeSuccess, prometheus.GaugeValue, 1)

	// 2. Enumerate buckets concurrently with bounded parallelism so a single
	// large bucket doesn't serialize the whole scrape. Writing metrics to ch
	// from multiple goroutines is safe; the registry owns closing the channel.
	sem := make(chan struct{}, maxConcurrentBuckets)
	var wg sync.WaitGroup
	for _, bucket := range bucketsOutput.Buckets {
		bucketName := aws.ToString(bucket.Name)
		wg.Add(1)
		sem <- struct{}{}
		go func(bucketName string) {
			defer wg.Done()
			defer func() { <-sem }()
			c.collectBucket(ctx, ch, bucketName)
		}(bucketName)
	}
	wg.Wait()
}

// collectBucket enumerates a single bucket and publishes its metrics. On any
// listing error it returns without emitting: a mid-pagination failure would
// otherwise report an undercounted total as if it were authoritative, so we
// leave a gap instead, which is the correct signal for a failed scrape.
func (c *S3Collector) collectBucket(ctx context.Context, ch chan<- prometheus.Metric, bucketName string) {
	var count float64
	var sizeBytes float64
	var latestTime int64

	// Use paginator to handle buckets with >1000 objects
	paginator := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			log.Printf("Error listing objects for bucket %s: %v", bucketName, err)
			// Don't publish a partial total. Emit success=0 so the failed
			// bucket is visible (an alertable signal) rather than just a gap.
			ch <- prometheus.MustNewConstMetric(c.bucketSuccess, prometheus.GaugeValue, 0, bucketName)
			return
		}

		for _, obj := range page.Contents {
			count++
			sizeBytes += float64(aws.ToInt64(obj.Size))

			if obj.LastModified != nil {
				modTime := obj.LastModified.Unix()
				if modTime > latestTime {
					latestTime = modTime
				}
			}
		}
	}

	// 3. Publish metrics for this bucket
	ch <- prometheus.MustNewConstMetric(c.bucketSuccess, prometheus.GaugeValue, 1, bucketName)
	ch <- prometheus.MustNewConstMetric(c.objectCount, prometheus.GaugeValue, count, bucketName)
	ch <- prometheus.MustNewConstMetric(c.totalSize, prometheus.GaugeValue, sizeBytes, bucketName)

	if latestTime > 0 {
		ch <- prometheus.MustNewConstMetric(c.lastModified, prometheus.GaugeValue, float64(latestTime), bucketName)
	}
}

func main() {
	accessKey := os.Getenv("AWS_S3_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_S3_SECRET_ACCESS_KEY")
	region := os.Getenv("AWS_S3_REGION")
	endpoint := os.Getenv("AWS_S3_ENDPOINT_URL")

	if accessKey == "" || secretKey == "" || region == "" || endpoint == "" {
		log.Fatal("CRITICAL: Missing required environment variables. " +
			"You must provide AWS_S3_ACCESS_KEY_ID, AWS_S3_SECRET_ACCESS_KEY, AWS_S3_REGION, and AWS_S3_ENDPOINT_URL.")
	}

	port := os.Getenv("METRICS_PORT")
	if port == "" {
		port = "9300"
	}

	path := os.Getenv("METRICS_PATH")
	if path == "" {
		path = "/s3-metrics"
	} else if path[0] != '/' {
		path = "/" + path
	}

	address := os.Getenv("METRICS_ADDRESS")
	if address == "" {
		address = "127.0.0.1"
	}

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		log.Fatalf("Unable to load SDK config: %v", err)
	}

	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	registry := prometheus.NewRegistry()
	collector := NewS3Collector(s3Client)
	registry.MustRegister(collector)

	// Serialize scrapes: each one fully enumerates every bucket, so allowing
	// overlapping requests would multiply the load on the S3 backend. Excess
	// concurrent requests get 503 rather than triggering another enumeration.
	http.Handle(path, promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		MaxRequestsInFlight: 1,
	}))

	log.Printf("Starting S3 exporter on %s:%s%s targeting %s", address, port, path, endpoint)
	log.Fatal(http.ListenAndServe(address+":"+port, nil))

}
