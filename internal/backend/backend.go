package backend

import (
	"context"
	"io"
	"time"
)

// Backend is the upstream storage abstraction. Every provider (S3, GCS, Azure, local)
// implements this interface. All methods must be safe for concurrent use.
//
// Bucket names passed to every method are already resolved to the backend bucket
// name (i.e. the value from bucket_mappings.backend_bucket, not the gateway name).
type Backend interface {
	// HeadObject returns metadata without downloading the body.
	HeadObject(ctx context.Context, in HeadObjectInput) (*HeadObjectOutput, error)

	// GetObject opens a streaming read. The caller must close GetObjectOutput.Body.
	// If in.Range is set (e.g. "bytes=0-1023"), only that byte range is returned.
	GetObject(ctx context.Context, in GetObjectInput) (*GetObjectOutput, error)

	// PutObject streams in.Body to the upstream. If in.Size < 0 the size is unknown
	// (chunked transfer); providers that cannot handle unknown sizes return ErrUnknownSize.
	PutObject(ctx context.Context, in PutObjectInput) (*PutObjectOutput, error)

	// DeleteObject removes a single object. Deleting a non-existent key is not an error.
	DeleteObject(ctx context.Context, in DeleteObjectInput) error

	// DeleteObjects removes up to 1000 objects in one call. Results report per-key
	// outcomes; a partial failure is not a top-level error.
	DeleteObjects(ctx context.Context, in DeleteObjectsInput) (*DeleteObjectsOutput, error)

	// HeadBucket verifies the bucket is reachable and the credentials have access.
	HeadBucket(ctx context.Context, in HeadBucketInput) error

	// ListObjects returns objects matching the query (S3 ListObjectsV2 semantics).
	ListObjects(ctx context.Context, in ListObjectsInput) (*ListObjectsOutput, error)

	// --- Multipart upload ---

	// CreateMultipartUpload initiates a multipart upload on the upstream.
	CreateMultipartUpload(ctx context.Context, in CreateMultipartUploadInput) (*CreateMultipartUploadOutput, error)

	// UploadPart streams a single part. Parts may arrive out of order.
	UploadPart(ctx context.Context, in UploadPartInput) (*UploadPartOutput, error)

	// CompleteMultipartUpload assembles the parts in the order specified by in.Parts.
	CompleteMultipartUpload(ctx context.Context, in CompleteMultipartUploadInput) (*CompleteMultipartUploadOutput, error)

	// AbortMultipartUpload cancels the upload and releases any staged storage.
	AbortMultipartUpload(ctx context.Context, in AbortMultipartUploadInput) error

	// PresignURL generates a time-limited URL that allows the bearer to access the
	// object directly on the upstream provider (used when a store is in redirect mode).
	PresignURL(ctx context.Context, in PresignInput) (*PresignOutput, error)
}

// ---------------------------------------------------------------------------
// Input / output types
// ---------------------------------------------------------------------------

type HeadObjectInput struct {
	Bucket string
	Key    string
}

type HeadObjectOutput struct {
	ContentLength int64
	ContentType   string
	ETag          string // without quotes
	LastModified  time.Time
	Metadata      map[string]string
}

type GetObjectInput struct {
	Bucket string
	Key    string
	// Range is the verbatim HTTP Range header value, e.g. "bytes=0-1023".
	// Empty means the full object.
	Range string
}

type GetObjectOutput struct {
	Body          io.ReadCloser
	ContentLength int64  // -1 if unknown
	ContentRange  string // e.g. "bytes 0-1023/4096" — present only for partial content
	ContentType   string
	ETag          string // without quotes
	LastModified  time.Time
	Metadata      map[string]string
}

type PutObjectInput struct {
	Bucket      string
	Key         string
	Body        io.Reader
	Size        int64 // -1 if unknown
	ContentType string
	Metadata    map[string]string
}

type PutObjectOutput struct {
	ETag string // without quotes
}

type DeleteObjectInput struct {
	Bucket string
	Key    string
}

type ObjectIdentifier struct {
	Key string
}

type DeleteObjectsInput struct {
	Bucket  string
	Objects []ObjectIdentifier
	// Quiet suppresses the per-key deleted list; only errors are reported.
	Quiet bool
}

type DeletedObject struct {
	Key string
}

type DeleteError struct {
	Key     string
	Code    string
	Message string
}

type DeleteObjectsOutput struct {
	Deleted []DeletedObject
	Errors  []DeleteError
}

type HeadBucketInput struct {
	Bucket string
}

type ListObjectsInput struct {
	Bucket            string
	Prefix            string
	Delimiter         string
	MaxKeys           int32
	StartAfter        string
	ContinuationToken string
}

type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string // without quotes
	LastModified time.Time
	StorageClass string
}

type ListObjectsOutput struct {
	Contents              []ObjectInfo
	CommonPrefixes        []string
	IsTruncated           bool
	NextContinuationToken string
	KeyCount              int32
}

type CreateMultipartUploadInput struct {
	Bucket      string
	Key         string
	ContentType string
	Metadata    map[string]string
}

type CreateMultipartUploadOutput struct {
	UploadID string
}

type UploadPartInput struct {
	Bucket     string
	Key        string
	UploadID   string
	PartNumber int32
	Body       io.Reader
	Size       int64
}

type UploadPartOutput struct {
	ETag string // without quotes
}

type CompletedPart struct {
	PartNumber int32
	ETag       string // without quotes
}

type CompleteMultipartUploadInput struct {
	Bucket   string
	Key      string
	UploadID string
	Parts    []CompletedPart // must be in ascending PartNumber order
}

type CompleteMultipartUploadOutput struct {
	ETag string // without quotes
}

type AbortMultipartUploadInput struct {
	Bucket   string
	Key      string
	UploadID string
}

type PresignInput struct {
	Bucket  string
	Key     string
	Method  string // "GET" or "PUT"
	Expires time.Duration
}

type PresignOutput struct {
	URL string
}
