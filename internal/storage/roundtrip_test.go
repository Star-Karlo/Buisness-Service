package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestRoundTripAgainstRealS3 proves the whole chain: sign a PUT, upload with a
// plain HTTP client exactly as a browser would, sign a GET, read it back.
//
// Skipped unless STORAGE_ENDPOINT is set, so `go test ./...` stays hermetic.
// Point it at MinIO to run it:
//
//	STORAGE_ENDPOINT=http://localhost:9000 STORAGE_BUCKET=karlo-uploads \
//	STORAGE_ACCESS_KEY=<minio-user> STORAGE_SECRET_KEY=<minio-password> go test ./internal/storage/ -run RoundTrip -v
func TestRoundTripAgainstRealS3(t *testing.T) {
	endpoint := os.Getenv("STORAGE_ENDPOINT")
	bucket := os.Getenv("STORAGE_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("set STORAGE_ENDPOINT and STORAGE_BUCKET to run the round trip")
	}
	ctx := context.Background()
	cfg := Config{
		Bucket:    bucket,
		Region:    "us-east-1",
		Endpoint:  endpoint,
		AccessKey: os.Getenv("STORAGE_ACCESS_KEY"),
		SecretKey: os.Getenv("STORAGE_SECRET_KEY"),
	}

	// Make sure the bucket exists; MinIO starts empty.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	if _, err := raw.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		// Already-exists is fine; anything else means the test cannot run.
		if !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") &&
			!strings.Contains(err.Error(), "BucketAlreadyExists") {
			t.Fatalf("create bucket: %v", err)
		}
	}

	client, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !client.Configured() {
		t.Fatal("client should be configured")
	}

	const body = "%PDF-1.4 pretend scanned STNK"
	key, err := Key("acme-co", PurposeTruckDocument, "stnk.pdf")
	if err != nil {
		t.Fatal(err)
	}

	putURL, _, err := client.PresignPut(ctx, key, "application/pdf")
	if err != nil {
		t.Fatal(err)
	}

	// Exactly what the browser does: a bare PUT, no AWS credentials.
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/pdf")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(res.Body)
		t.Fatalf("upload failed: %d %s", res.StatusCode, msg)
	}

	getURL, _, err := client.PresignGet(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := http.Get(getURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = got.Body.Close() }()
	read, _ := io.ReadAll(got.Body)
	if string(read) != body {
		t.Fatalf("round trip changed the bytes: got %q", read)
	}

	// A signature is bound to its content type; reusing the URL for another
	// must be refused, or the type validation would be decorative.
	req2, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader([]byte("<html>")))
	req2.Header.Set("Content-Type", "text/html")
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res2.Body.Close() }()
	if res2.StatusCode == http.StatusOK {
		t.Fatal("a signed URL must not accept a different Content-Type")
	}
	t.Logf("round trip OK; content-type swap refused with %d", res2.StatusCode)
}
