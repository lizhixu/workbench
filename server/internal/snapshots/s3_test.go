package snapshots

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestS3Presign(t *testing.T) {
	target := &S3Target{
		ID:             "s3_test",
		Name:           "Test MinIO",
		Provider:       "minio",
		Endpoint:       "http://127.0.0.1:9000",
		Region:         "us-east-1",
		Bucket:         "my-bucket",
		Prefix:         "backups/prod",
		AccessKey:      "minioadmin",
		SecretKey:      "minioadmin",
		ForcePathStyle: true,
		IsDefault:      true,
		CreatedAt:      time.Now(),
	}

	client := NewS3Client(target)
	key := client.ObjectKey("snapshot-01.tar.gz")
	if key != "backups/prod/snapshot-01.tar.gz" {
		t.Fatalf("expected prefix key, got: %s", key)
	}

	// Test Presigned PUT
	putURL, err := client.PresignPut(key, 1*time.Hour)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if !strings.Contains(putURL, "X-Amz-Signature=") || !strings.Contains(putURL, "my-bucket") {
		t.Fatalf("malformed presigned PUT URL: %s", putURL)
	}

	// Test Presigned GET
	getURL, err := client.PresignGet(key, 1*time.Hour)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if !strings.Contains(getURL, "X-Amz-Signature=") || !strings.Contains(getURL, "my-bucket") {
		t.Fatalf("malformed presigned GET URL: %s", getURL)
	}
}

func TestS3DeleteObjectStatuses(t *testing.T) {
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()
	target := &S3Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "ak", SecretKey: "sk", Region: "auto"}
	client := NewS3Client(target)

	// 204 is success; 404 means already gone and must not strand metadata.
	for _, code := range []int{http.StatusNoContent, http.StatusNotFound} {
		status = code
		if err := client.DeleteObject(context.Background(), "apps/a/vbk.tar.gz"); err != nil {
			t.Fatalf("status %d: unexpected error %v", code, err)
		}
	}

	// Any other status must surface: callers keep the metadata record for retry.
	status = http.StatusInternalServerError
	if err := client.DeleteObject(context.Background(), "apps/a/vbk.tar.gz"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("status 500: want error containing 500, got %v", err)
	}
}

func TestS3TargetStore(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Add target
	t1 := &S3Target{
		Name:           "Cloudflare R2",
		Provider:       "r2",
		Endpoint:       "https://account.r2.cloudflarestorage.com",
		Bucket:         "watchman-backup",
		AccessKey:      "ak123",
		SecretKey:      "sk123",
		IsDefault:      true,
	}
	if err := s.PutS3Target(t1); err != nil {
		t.Fatalf("PutS3Target: %v", err)
	}

	// Reload from disk
	s2, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	targets := s2.ListS3Targets()
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	// Verify SecretKey masked in List
	if targets[0].SecretKey != "••••••••" {
		t.Fatalf("expected masked secret key, got %s", targets[0].SecretKey)
	}

	// Verify unmasked in Get
	got, ok := s2.GetS3Target(targets[0].ID)
	if !ok || got.SecretKey != "sk123" {
		t.Fatalf("expected unmasked secret key in GetS3Target")
	}

	// Verify default target resolution
	def, ok := s2.GetDefaultS3Target()
	if !ok || def.ID != targets[0].ID {
		t.Fatalf("GetDefaultS3Target failed")
	}
}
