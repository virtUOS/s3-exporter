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

type S3Collector struct {
	client *s3.Client

	objectCount  *prometheus.Desc
	totalSize    *prometheus.Desc
	lastModified *prometheus.Desc
}

func NewS3Collector(client *s3.Client) *S3Collector {
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
	}
}

func (c *S3Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.objectCount
	ch <- c.totalSize
	ch <- c.lastModified
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
		return
	}

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
			return // skip this bucket; don't publish a partial total
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
