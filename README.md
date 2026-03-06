# 🪣 S3 Prometheus Exporter

A lightweight, minimal Prometheus exporter for S3 environments where you do not have access to the metrics.

This exporter automatically discovers all buckets your credentials have access to and exposes key metrics about their size and object count.

## ✨ Features

- 🔍 Auto-Discovery: Automatically lists and iterates through all available buckets.
- 🔒 Strict Configuration: Fails fast.
- 🏗️ Self-Hosted First: Enforces PathStyle addressing out of the box, which is required for most on-premise S3 installations.
- 🪶 Lightweight: Deploys as a statically linked binary in an empty Docker image (< 15MB), with no shell and an unprivileged user.

## 📊 Exposed Metrics

Metrics are exposed on the `/s3-metrics` endpoint. Every metric includes a bucket label with the name of the S3 bucket.

| Metric Name | Type | Description |
|---|---|---|
| s3_bucket_objects_total | Gauge | Total number of objects currently in the bucket. |
| s3_bucket_size_bytes | Gauge | Total combined size of all objects in the bucket. |
| s3_bucket_last_modified_timestamp_seconds | Gauge | Unix timestamp of the most recently modified object. |

An example output looks like this:

```
# HELP s3_bucket_last_modified_timestamp_seconds Timestamp of the most recently modified object
# TYPE s3_bucket_last_modified_timestamp_seconds gauge
s3_bucket_last_modified_timestamp_seconds{bucket="mybucket-1"} 1.772715739e+09
s3_bucket_last_modified_timestamp_seconds{bucket="mybucket-2"} 1.772715739e+09
s3_bucket_last_modified_timestamp_seconds{bucket="mybucket-3"} 1.772715739e+09
s3_bucket_last_modified_timestamp_seconds{bucket="mybucket-4"} 1.772715739e+09
# HELP s3_bucket_objects_total Total number of objects in the bucket
# TYPE s3_bucket_objects_total gauge
s3_bucket_objects_total{bucket="mybucket-1"} 12
s3_bucket_objects_total{bucket="mybucket-2"} 3000
s3_bucket_objects_total{bucket="mybucket-3"} 3
s3_bucket_objects_total{bucket="mybucket-4"} 0
# HELP s3_bucket_size_bytes Total size of all objects in the bucket
# TYPE s3_bucket_size_bytes gauge
s3_bucket_size_bytes{bucket="mybucket-1"} 154236
s3_bucket_size_bytes{bucket="mybucket-2"} 234562143
s3_bucket_size_bytes{bucket="mybucket-3"} 65204
s3_bucket_size_bytes{bucket="mybucket-4"} 0
```

## ⚙️ Configuration

The exporter is configured via environment variables. There are no default fallbacks for AWS credentials. If a required variable is missing, the application will crash immediately on startup.

| Environment Variable | Required | Default	Description |
|---|---|---|
| AWS_ACCESS_KEY_ID | ✅ Yes | - | Your S3 Access Key. |
| AWS_SECRET_ACCESS_KEY | ✅ Yes | - | Your S3 Secret Key. |
| AWS_REGION | ✅ Yes | - | The S3 region (e.g., us-east-1, or your custom region). |
| S3_ENDPOINT | ✅ Yes | - | Full URL to your self-hosted S3 cluster (e.g., http://s3.internal:9000). |
| METRICS_PORT | ❌ No | 9300 | The port the HTTP server binds to. |

## 🚀 Usage

### 🐳 Run as container (Recommended)

Build the image:

```bash
docker build -t s3-exporter:latest .
```

Run the container:

```bash
docker run \
  --name s3-exporter \
  -p 9300:9300 \
  -e AWS_ACCESS_KEY_ID="your-access-key" \
  -e AWS_SECRET_ACCESS_KEY="your-secret-key" \
  -e AWS_REGION="us-east-1" \
  -e S3_ENDPOINT="http://your-s3-cluster:9000" \
  s3-exporter:latest
```

Or in compose:

```yaml
services:
  s3-exporter:
    image: ghcr.io/virtuos/s3-exporter:latest
    container_name: s3-exporter
    restart: unless-stopped
    ports:
      - "9300:9300"
    env_file:
      - .env
```

### Running Locally (Development)

Ensure you have Go 1.21+ installed, then run:

```bash
export AWS_ACCESS_KEY_ID="your-access-key"
export AWS_SECRET_ACCESS_KEY="your-secret-key"
export AWS_REGION="us-east-1"
export S3_ENDPOINT="http://localhost:9000"

go mod tidy
go run main.go
```

Verify it works by curling the endpoint:

```bash
curl http://localhost:9300/s3-metrics
```

## 🔗 Prometheus Scrape Configuration

Add the following scrape job to your `prometheus.yml`:


```yaml
scrape_configs:
  - job_name: 's3-exporter'
    metrics_path: '/s3-metrics'
    static_configs:
      - targets: ['s3-exporter:9300']
```

### 🚨 Alerting Examples

Below you find two examples for alerting rules for the following scenarios:

1. You might use S3 to save backups. In that case you can see from the last modified date of a bucket, when the last backup took place.
2. We can not easily retrieve the quotas from the S# environment, but if you know them, you can alert if the bucket grows too big.

```yaml
groups:
  - name: s3-exporter-alerts
    rules:
      # ==========================================
      # Alert: Bucket hasn't been updated in 3 days
      # useful to monitor backups into S3
      # ==========================================
      - alert: S3BucketStale
        # time() gets current Unix timestamp. 
        # 3 days = 3 * 24 hours * 3600 seconds
        expr: (time() - s3_bucket_last_modified_timestamp_seconds) > (3 * 24 * 3600)
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "S3 Bucket '{{ $labels.bucket }}' is stale"
          description: >
            No objects have been uploaded or modified in the bucket '{{ $labels.bucket }}' 
            for over 3 days. Check if the upstream backup/upload jobs are failing.

      # ==========================================
      # Alert: Bucket size exceeds 100 GB
      # ==========================================
      - alert: S3BucketTooLarge
        # 100 GB calculated in bytes (100 * 1024^3)
        expr: s3_bucket_size_bytes > (100 * 1024 * 1024 * 1024)
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "S3 Bucket '{{ $labels.bucket }}' exceeded 100 GB"
          description: >
            The S3 bucket '{{ $labels.bucket }}' has exceeded the 100 GB threshold. 
            Current size is {{ $value | humanize1024 }}.
```

## ⚠️ Performance Note: Large Buckets

This exporter performs a synchronous scrape. Every time Prometheus queries the `/s3-metrics` endpoint, the exporter iterates through your S3 buckets in real-time. This works best in smaller buckets:

- Small to Medium Buckets: This happens in milliseconds and works perfectly.
- Massive Buckets: The AWS SDK will need to paginate extensively (1,000 objects per page). This will take time and cause high API load on your S3 cluster. If you have extremely large buckets, you may need to increase your Prometheus scrape_timeout.

## 📝 License

MIT

## ✒️ Authors

virtUOS, Osnabrueck University
