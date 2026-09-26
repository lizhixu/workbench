package snapshots

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// S3Target represents one S3-compatible storage destination.
type S3Target struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`           // e.g. "Cloudflare R2" or "公司内网 MinIO"
	Provider       string    `json:"provider"`       // "r2" | "minio" | "aws" | "aliyun" | "tencent" | "custom"
	Endpoint       string    `json:"endpoint"`       // e.g. "https://xxx.r2.cloudflarestorage.com"
	Region         string    `json:"region"`         // e.g. "auto" or "us-east-1"
	Bucket         string    `json:"bucket"`
	Prefix         string    `json:"prefix,omitempty"` // e.g. "backups/"
	AccessKey      string    `json:"access_key"`
	SecretKey      string    `json:"secret_key,omitempty"` // stored 0600 on server
	ForcePathStyle bool      `json:"force_path_style"`     // true for MinIO/custom, false for AWS/R2
	IsDefault      bool      `json:"is_default"`
	CreatedAt      time.Time `json:"created_at"`
}

// Redacted returns a copy with SecretKey masked for safe public viewing.
func (s *S3Target) Redacted() *S3Target {
	if s == nil {
		return nil
	}
	cp := *s
	if cp.SecretKey != "" {
		cp.SecretKey = "••••••••"
	}
	return &cp
}

// S3Client handles S3 SigV4 API calls and pre-signed URL generation.
type S3Client struct {
	target *S3Target
	http   *http.Client
}

// NewS3Client creates an S3 client for a target.
func NewS3Client(target *S3Target) *S3Client {
	return &S3Client{
		target: target,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// ObjectKey computes the full key with target prefix.
func (c *S3Client) ObjectKey(name string) string {
	p := strings.Trim(c.target.Prefix, "/")
	if p == "" {
		return name
	}
	return p + "/" + name
}

// buildURL constructs the endpoint URL (Path-Style or Virtual-Host style).
func (c *S3Client) buildURL(key string) (*url.URL, error) {
	rawEndpoint := strings.TrimRight(c.target.Endpoint, "/")
	if !strings.HasPrefix(rawEndpoint, "http://") && !strings.HasPrefix(rawEndpoint, "https://") {
		rawEndpoint = "https://" + rawEndpoint
	}
	u, err := url.Parse(rawEndpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint: %w", err)
	}

	cleanKey := strings.TrimLeft(key, "/")
	if c.target.ForcePathStyle || strings.Contains(u.Host, "localhost") || strings.Contains(u.Host, "127.0.0.1") {
		// Path-style: https://endpoint/bucket/key
		u.Path = fmt.Sprintf("/%s/%s", c.target.Bucket, cleanKey)
	} else {
		// Virtual-host style: https://bucket.endpoint/key
		u.Host = fmt.Sprintf("%s.%s", c.target.Bucket, u.Host)
		u.Path = "/" + cleanKey
	}
	return u, nil
}

// PresignPut generates a pre-signed PUT URL using AWS SigV4 query authentication.
func (c *S3Client) PresignPut(key string, expires time.Duration) (string, error) {
	return c.presign("PUT", key, expires)
}

// PresignGet generates a pre-signed GET URL for downloading/restoring.
func (c *S3Client) PresignGet(key string, expires time.Duration) (string, error) {
	return c.presign("GET", key, expires)
}

func (c *S3Client) presign(method, key string, expires time.Duration) (string, error) {
	u, err := c.buildURL(key)
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	region := c.target.Region
	if region == "" {
		region = "auto"
	}
	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)

	query := make(url.Values)
	query.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	query.Set("X-Amz-Credential", fmt.Sprintf("%s/%s", c.target.AccessKey, credentialScope))
	query.Set("X-Amz-Date", amzDate)
	query.Set("X-Amz-Expires", fmt.Sprintf("%d", int(expires.Seconds())))
	query.Set("X-Amz-SignedHeaders", "host")

	canonicalQuery := query.Encode()

	// Canonical Request
	canonicalURI := u.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalHeaders := fmt.Sprintf("host:%s\n", u.Host)
	signedHeaders := "host"
	payloadHash := "UNSIGNED-PAYLOAD"

	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		method, canonicalURI, canonicalQuery, canonicalHeaders, signedHeaders, payloadHash)

	reqSum := sha256.Sum256([]byte(canonicalRequest))
	reqHex := hex.EncodeToString(reqSum[:])

	// String to sign
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, credentialScope, reqHex)

	// Signing key
	signingKey := getSignatureKey(c.target.SecretKey, dateStamp, region, "s3")
	sig := hmacSHA256(signingKey, []byte(stringToSign))
	signatureHex := hex.EncodeToString(sig)

	// Final URL with query
	query.Set("X-Amz-Signature", signatureHex)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// PutObject uploads a stream directly from the server.
func (c *S3Client) PutObject(ctx context.Context, key string, data io.Reader, size int64) error {
	u, err := c.buildURL(key)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "PUT", u.String(), data)
	if err != nil {
		return err
	}
	req.ContentLength = size

	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	region := c.target.Region
	if region == "" {
		region = "auto"
	}
	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)

	req.Header.Set("Host", u.Host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")

	canonicalURI := u.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:UNSIGNED-PAYLOAD\nx-amz-date:%s\n", u.Host, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := fmt.Sprintf("PUT\n%s\n\n%s\n%s\nUNSIGNED-PAYLOAD",
		canonicalURI, canonicalHeaders, signedHeaders)

	reqSum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, credentialScope, hex.EncodeToString(reqSum[:]))

	signingKey := getSignatureKey(c.target.SecretKey, dateStamp, region, "s3")
	sig := hmacSHA256(signingKey, []byte(stringToSign))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.target.AccessKey, credentialScope, signedHeaders, hex.EncodeToString(sig))
	req.Header.Set("Authorization", authHeader)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("S3 PutObject %s: %d %s", key, resp.StatusCode, string(body))
	}
	return nil
}

// DeleteObject removes an object from S3.
func (c *S3Client) DeleteObject(ctx context.Context, key string) error {
	u, err := c.buildURL(key)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	region := c.target.Region
	if region == "" {
		region = "auto"
	}
	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)

	req, err := http.NewRequestWithContext(ctx, "DELETE", u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Host", u.Host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") // empty sha256

	canonicalURI := u.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\nx-amz-date:%s\n", u.Host, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := fmt.Sprintf("DELETE\n%s\n\n%s\n%s\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		canonicalURI, canonicalHeaders, signedHeaders)

	reqSum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, credentialScope, hex.EncodeToString(reqSum[:]))

	signingKey := getSignatureKey(c.target.SecretKey, dateStamp, region, "s3")
	sig := hmacSHA256(signingKey, []byte(stringToSign))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.target.AccessKey, credentialScope, signedHeaders, hex.EncodeToString(sig))
	req.Header.Set("Authorization", authHeader)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// TestBucket probes bucket connectivity by uploading and immediately deleting a 0-byte probe object.
func (c *S3Client) TestBucket(ctx context.Context) error {
	probeKey := c.ObjectKey(".watchman_connectivity_probe")
	if err := c.PutObject(ctx, probeKey, bytes.NewReader([]byte("ok")), 2); err != nil {
		return fmt.Errorf("S3 连通性测试失败: %w", err)
	}
	_ = c.DeleteObject(ctx, probeKey)
	return nil
}

// SigV4 helper functions
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func getSignatureKey(key, dateStamp, regionName, serviceName string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+key), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(regionName))
	kService := hmacSHA256(kRegion, []byte(serviceName))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}
