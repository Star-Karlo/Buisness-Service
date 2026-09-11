// Package storage signs short-lived S3 URLs so the browser can upload and
// download files without the service ever handling the bytes.
//
// The alternative — POSTing a file to the API and having it forward to S3 —
// doubles the transfer, holds a request open for the length of the upload, and
// makes a 15 MB proof-of-delivery photo a memory problem for every service
// instance. A signed URL moves the transfer to the two parties that care.
package storage

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

// ErrNotConfigured is returned when no bucket is set. Uploads then fail
// cleanly, and the frontend disables its file fields with a reason rather than
// letting somebody choose a file and lose it on submit.
var ErrNotConfigured = errors.New("storage: no bucket configured")

// ErrPurpose is returned for a purpose this service does not recognise. The
// purpose decides the key prefix, so an unknown one would either land files in
// an unswept corner of the bucket or let a caller choose their own path.
var ErrPurpose = errors.New("storage: unknown purpose")

// Purpose is what a file is for. It is NOT a path: the caller names the
// purpose and the service derives the location, so no request can write
// outside its own company's prefix.
type Purpose string

const (
	PurposeTruckDocument   Purpose = "truckDocument"
	PurposeTruckPhoto      Purpose = "truckPhoto"
	PurposeAgreementDoc    Purpose = "agreementDocument"
	PurposeOrderPOD        Purpose = "orderPod"
	PurposeCompanyDocument Purpose = "companyDocument"
)

// prefixes maps a purpose onto its folder. A table rather than something
// derived from the string, so a typo in a client cannot invent a new prefix.
var prefixes = map[Purpose]string{
	PurposeTruckDocument:   "truck-documents",
	PurposeTruckPhoto:      "truck-photos",
	PurposeAgreementDoc:    "agreement-documents",
	PurposeOrderPOD:        "order-pod",
	PurposeCompanyDocument: "company-documents",
}

// limits caps each kind of upload, in bytes. Enforced when signing, not only
// in the browser: a signed URL is a capability, and one obtained for a small
// document must not be usable to push a gigabyte.
var limits = map[Purpose]int64{
	PurposeTruckDocument:   15 << 20, // A scanned STNK or KIR.
	PurposeTruckPhoto:      10 << 20,
	PurposeAgreementDoc:    25 << 20, // Contracts run long.
	PurposeOrderPOD:        15 << 20,
	PurposeCompanyDocument: 15 << 20,
}

// allowedTypes is what each purpose accepts. A deny-by-default list rather
// than a block list: the interesting attack is uploading text/html to a bucket
// something later serves, and enumerating every dangerous type is a game you
// lose.
var allowedTypes = map[Purpose][]string{
	PurposeTruckDocument:   {"application/pdf", "image/jpeg", "image/png"},
	PurposeTruckPhoto:      {"image/jpeg", "image/png", "image/webp"},
	PurposeAgreementDoc:    {"application/pdf", "image/jpeg", "image/png"},
	PurposeOrderPOD:        {"image/jpeg", "image/png", "image/webp", "application/pdf"},
	PurposeCompanyDocument: {"application/pdf", "image/jpeg", "image/png"},
}

type Client struct {
	bucket  string
	presign *s3.PresignClient
	putTTL  time.Duration
	getTTL  time.Duration
}

type Config struct {
	Bucket string
	Region string
	// Endpoint overrides the AWS host. Set for MinIO or another S3-compatible
	// server; empty means real AWS.
	Endpoint string
	// AccessKey and SecretKey are optional: left empty the SDK uses the
	// ambient credential chain (task role, profile, environment), which is
	// what production should do.
	AccessKey string
	SecretKey string
}

// New builds a client. An empty bucket yields a client whose every call
// returns ErrNotConfigured, so a deployment without storage still starts.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Bucket == "" {
		return &Client{}, nil
	}

	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.AccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("storage: loading aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			// MinIO and most S3-compatible servers do not do virtual-host
			// style addressing on a bare host.
			o.UsePathStyle = true
		}
	})

	return &Client{
		bucket:  cfg.Bucket,
		presign: s3.NewPresignClient(client),
		// Long enough for a slow mobile connection to finish a 15 MB scan,
		// short enough that a leaked URL is not a standing grant.
		putTTL: 15 * time.Minute,
		getTTL: 5 * time.Minute,
	}, nil
}

// Configured reports whether uploads are available.
func (c *Client) Configured() bool { return c != nil && c.bucket != "" }

// Validate checks a request before any URL is signed.
func Validate(purpose Purpose, contentType string, sizeBytes int64) error {
	types, ok := allowedTypes[purpose]
	if !ok {
		return fmt.Errorf("%w: %q", ErrPurpose, purpose)
	}
	if sizeBytes <= 0 {
		return errors.New("storage: sizeBytes must be greater than zero")
	}
	if max := limits[purpose]; sizeBytes > max {
		return fmt.Errorf("storage: file is %d bytes, the limit for %s is %d", sizeBytes, purpose, max)
	}
	// Compared on the media type alone: browsers append parameters such as
	// "; charset=utf-8", and strict equality rejects a legitimate PDF.
	media := strings.TrimSpace(strings.Split(contentType, ";")[0])
	for _, t := range types {
		if strings.EqualFold(media, t) {
			return nil
		}
	}
	return fmt.Errorf("storage: %s does not accept %q", purpose, media)
}

// Key builds the object key for an upload.
//
// Shaped company/purpose/uuid.ext. The company comes first so a bucket policy
// or lifecycle rule can be written per tenant. The filename is REPLACED by a
// uuid rather than sanitised: a name chosen by a caller is a path traversal
// waiting to happen, and two drivers photographing "pod.jpg" must not collide.
// The original name is kept on the record, not in the key.
func Key(companyID string, purpose Purpose, fileName string) (string, error) {
	prefix, ok := prefixes[purpose]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrPurpose, purpose)
	}
	ext := strings.ToLower(path.Ext(fileName))
	if len(ext) > 10 || strings.ContainsAny(ext, `/\`) {
		ext = "" // Not a real extension.
	}
	return fmt.Sprintf("%s/%s/%s%s", companyID, prefix, uuid.NewString(), ext), nil
}

// PresignPut signs an upload. ContentType is bound into the signature, so the
// URL cannot be reused to upload something other than what was validated.
func (c *Client) PresignPut(ctx context.Context, key, contentType string) (string, time.Duration, error) {
	if !c.Configured() {
		return "", 0, ErrNotConfigured
	}
	req, err := c.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}, s3.WithPresignExpires(c.putTTL))
	if err != nil {
		return "", 0, fmt.Errorf("storage: signing upload: %w", err)
	}
	return req.URL, c.putTTL, nil
}

// PresignGet signs a read. Objects are private, so a stored key is not a URL
// anybody can open; this turns a key back into something a browser can fetch
// for as long as the person looking at the record needs it.
func (c *Client) PresignGet(ctx context.Context, key string) (string, time.Duration, error) {
	if !c.Configured() {
		return "", 0, ErrNotConfigured
	}
	req, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(c.getTTL))
	if err != nil {
		return "", 0, fmt.Errorf("storage: signing download: %w", err)
	}
	return req.URL, c.getTTL, nil
}

// OwnedBy reports whether a key belongs to a company.
//
// The download path needs this: without it any authenticated user could sign a
// GET for another tenant's contract simply by pasting its key.
func OwnedBy(key, companyID string) bool {
	return companyID != "" && strings.HasPrefix(key, companyID+"/")
}
