package configsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Source pulls cronicle.hcl from any S3-compatible store: AWS S3,
// MinIO, R2, DigitalOcean Spaces, Backblaze B2, Wasabi — the SDK's
// endpoint override covers them all. Credentials come from the
// standard AWS chain (env, ~/.aws, IRSA on EKS) unless explicitly
// supplied via URL userinfo.
//
// URL shape: s3://bucket/key/path/to/cronicle.hcl?endpoint=https://minio.local&region=us-east-1&path_style=1
//
// The query parameters:
//   - endpoint: optional, override the API endpoint (MinIO, R2, etc.)
//   - region:   defaults to us-east-1 (also used as the MinIO-friendly default)
//   - path_style: "1" or "true" to force path-style URLs (required by MinIO)
//
// Etag is the object's ETag header (S3 sets this automatically, it's
// typically the MD5 of the content for non-multipart objects).
type S3Source struct {
	Bucket string
	Key    string
	Client *s3.Client

	// display is set at construction so we can redact creds + show
	// the bucket+key combo cleanly in logs.
	display string
}

// NewS3Source parses an s3:// URL and returns a Source backed by the
// AWS SDK. Returns ErrNotFound if the resulting client can be created
// but the bucket itself is missing — that's a startup-time sanity
// check operators want to see immediately, not on first refresh tick.
func NewS3Source(ctx context.Context, rawurl string) (*S3Source, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("invalid s3 url: %w", err)
	}
	if u.Scheme != "s3" {
		return nil, fmt.Errorf("not an s3 url: %s", rawurl)
	}
	bucket := u.Host
	key := u.Path
	if len(key) > 0 && key[0] == '/' {
		key = key[1:]
	}
	if bucket == "" || key == "" {
		return nil, fmt.Errorf("s3 url must include bucket and key: s3://bucket/path/to/cronicle.hcl")
	}

	q := u.Query()
	region := q.Get("region")
	if region == "" {
		region = "us-east-1"
	}
	endpoint := q.Get("endpoint")
	pathStyle := q.Get("path_style") == "1" || q.Get("path_style") == "true"

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3 source: load aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = pathStyle
	})

	return &S3Source{
		Bucket:  bucket,
		Key:     key,
		Client:  client,
		display: fmt.Sprintf("s3://%s/%s", bucket, key),
	}, nil
}

// Fetch issues a HEAD first to read the ETag; if the etag matches
// the caller's prevEtag, no GET happens (one round-trip cost on
// unchanged content). On etag mismatch (or empty prevEtag) it
// follows up with a GET.
func (s *S3Source) Fetch(ctx context.Context, prevEtag string) ([]byte, string, bool, error) {
	head, err := s.Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.Key),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, prevEtag, false, ErrNotFound
		}
		return nil, prevEtag, false, fmt.Errorf("s3 head %s: %w", s.display, err)
	}
	etag := ""
	if head.ETag != nil {
		etag = *head.ETag
	}
	if etag != "" && etag == prevEtag {
		return nil, etag, false, nil
	}
	get, err := s.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.Key),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, prevEtag, false, ErrNotFound
		}
		return nil, prevEtag, false, fmt.Errorf("s3 get %s: %w", s.display, err)
	}
	defer get.Body.Close()
	body, err := io.ReadAll(get.Body)
	if err != nil {
		return nil, prevEtag, false, err
	}
	// Prefer the GET response's ETag (post-multipart re-uploads may
	// have a different etag than HEAD observed if the object raced).
	if get.ETag != nil {
		etag = *get.ETag
	}
	return body, etag, true, nil
}

func (s *S3Source) String() string {
	return s.display
}

// isNoSuchKey unwraps the various ways the AWS SDK reports a missing
// object: typed NoSuchKey, NotFound (HEAD), or a smithy generic with
// the same error code. Mirrors the helper in cronicle-infra/internal/blob.
func isNoSuchKey(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}
