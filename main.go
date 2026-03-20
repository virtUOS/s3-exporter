package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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
	ctx := context.TODO()

	// 1. Auto-discover all buckets we have access to
	bucketsOutput, err := c.client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		log.Printf("Error listing buckets: %v", err)
		return
	}

	// 2. Iterate over each bucket and fetch stats
	for _, bucket := range bucketsOutput.Buckets {
		bucketName := aws.ToString(bucket.Name)

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
				break // skip to the next bucket on error
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

	cfg, err := config.LoadDefaultConfig(context.TODO(),
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

	http.Handle("/s3-metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	log.Printf("Starting S3 exporter on 0.0.0.0:%s/s3-metrics targeting %s", port, endpoint)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, nil))
}
