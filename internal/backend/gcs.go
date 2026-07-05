package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// gcsBackend implements Backend against Google Cloud Storage.
//
// Multipart upload is emulated using temporary objects + compose:
//   - Each part is written to  _sgw_parts/{uploadID}/{partNum:05d}
//   - CompleteMultipartUpload composes them into the final object and deletes the temp keys.
//   - For > 32 parts, compose is applied in a tree (GCS compose limit is 32 sources).
type gcsBackend struct {
	client *storage.Client
	sa     gcsServiceAccount // extracted for presigned URL signing
}

type gcsServiceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
}

func newGCSBackend(cfg GCSConfig) (Backend, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithCredentialsJSON(cfg.CredentialsJSON))
	if err != nil {
		return nil, fmt.Errorf("creating gcs client: %w", err)
	}

	var sa gcsServiceAccount
	if err := json.Unmarshal(cfg.CredentialsJSON, &sa); err != nil {
		return nil, fmt.Errorf("parsing gcs service account: %w", err)
	}

	return &gcsBackend{client: client, sa: sa}, nil
}

func (b *gcsBackend) Close() error {
	return b.client.Close()
}

func (b *gcsBackend) HeadObject(ctx context.Context, in HeadObjectInput) (*HeadObjectOutput, error) {
	attrs, err := b.client.Bucket(in.Bucket).Object(in.Key).Attrs(ctx)
	if err != nil {
		return nil, mapGCSErr(err)
	}
	return &HeadObjectOutput{
		ContentLength: attrs.Size,
		ContentType:   attrs.ContentType,
		ETag:          attrs.Etag,
		LastModified:  attrs.Updated,
		Metadata:      attrs.Metadata,
	}, nil
}

func (b *gcsBackend) GetObject(ctx context.Context, in GetObjectInput) (*GetObjectOutput, error) {
	obj := b.client.Bucket(in.Bucket).Object(in.Key)

	// Fetch full object attributes for ETag (ReaderObjectAttrs does not include it).
	fullAttrs, err := obj.Attrs(ctx)
	if err != nil {
		return nil, mapGCSErr(err)
	}

	var rc *storage.Reader
	var contentRange string
	contentLength := fullAttrs.Size

	if in.Range != "" {
		offset, length, parseErr := parseHTTPRange(in.Range)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid range %q: %w", in.Range, parseErr)
		}
		rc, err = obj.NewRangeReader(ctx, offset, length)
		if err == nil {
			// rc.Attrs.Size is the full object size; Remain() is the number of
			// bytes this range reader will actually return.
			contentLength = rc.Remain()
			end := offset + contentLength - 1
			contentRange = fmt.Sprintf("bytes %d-%d/%d", offset, end, fullAttrs.Size)
		}
	} else {
		rc, err = obj.NewReader(ctx)
	}
	if err != nil {
		return nil, mapGCSErr(err)
	}

	return &GetObjectOutput{
		Body:          rc,
		ContentLength: contentLength,
		ContentRange:  contentRange,
		ContentType:   fullAttrs.ContentType,
		ETag:          fullAttrs.Etag,
		LastModified:  fullAttrs.Updated,
		Metadata:      fullAttrs.Metadata,
	}, nil
}

func (b *gcsBackend) PutObject(ctx context.Context, in PutObjectInput) (*PutObjectOutput, error) {
	obj := b.client.Bucket(in.Bucket).Object(in.Key)
	wc := obj.NewWriter(ctx)
	if in.ContentType != "" {
		wc.ContentType = in.ContentType
	}
	wc.Metadata = in.Metadata

	if _, err := io.Copy(wc, in.Body); err != nil {
		_ = wc.Close()
		return nil, fmt.Errorf("%w: copying to gcs: %w", ErrUpstreamError, err)
	}
	if err := wc.Close(); err != nil {
		return nil, mapGCSErr(err)
	}
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return nil, mapGCSErr(err)
	}
	return &PutObjectOutput{ETag: attrs.Etag}, nil
}

func (b *gcsBackend) DeleteObject(ctx context.Context, in DeleteObjectInput) error {
	err := b.client.Bucket(in.Bucket).Object(in.Key).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil // idempotent
	}
	return mapGCSErr(err)
}

func (b *gcsBackend) DeleteObjects(ctx context.Context, in DeleteObjectsInput) (*DeleteObjectsOutput, error) {
	result := &DeleteObjectsOutput{}
	for _, obj := range in.Objects {
		err := b.client.Bucket(in.Bucket).Object(obj.Key).Delete(ctx)
		if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			result.Errors = append(result.Errors, DeleteError{
				Key:     obj.Key,
				Code:    "InternalError",
				Message: err.Error(),
			})
		} else if !in.Quiet {
			result.Deleted = append(result.Deleted, DeletedObject{Key: obj.Key})
		}
	}
	return result, nil
}

func (b *gcsBackend) HeadBucket(ctx context.Context, in HeadBucketInput) error {
	_, err := b.client.Bucket(in.Bucket).Attrs(ctx)
	return mapGCSErr(err)
}

func (b *gcsBackend) ListObjects(ctx context.Context, in ListObjectsInput) (*ListObjectsOutput, error) {
	query := &storage.Query{
		Prefix:      in.Prefix,
		Delimiter:   in.Delimiter,
		StartOffset: in.StartAfter,
	}
	it := b.client.Bucket(in.Bucket).Objects(ctx, query)

	maxKeys := int(in.MaxKeys)
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	pager := iterator.NewPager(it, maxKeys, in.ContinuationToken)
	var page []*storage.ObjectAttrs
	nextToken, err := pager.NextPage(&page)
	if err != nil {
		return nil, mapGCSErr(err)
	}

	result := &ListObjectsOutput{
		IsTruncated:           nextToken != "",
		NextContinuationToken: nextToken,
	}
	for _, attrs := range page {
		if attrs.Prefix != "" {
			result.CommonPrefixes = append(result.CommonPrefixes, attrs.Prefix)
		} else {
			result.Contents = append(result.Contents, ObjectInfo{
				Key:          attrs.Name,
				Size:         attrs.Size,
				ETag:         attrs.Etag,
				LastModified: attrs.Updated,
			})
		}
	}
	result.KeyCount = int32(len(result.Contents))
	return result, nil
}

// ---------------------------------------------------------------------------
// Multipart — emulated via temporary objects + compose
// ---------------------------------------------------------------------------

func (b *gcsBackend) CreateMultipartUpload(_ context.Context, in CreateMultipartUploadInput) (*CreateMultipartUploadOutput, error) {
	// No upstream call needed; the upload ID is generated here.
	// Parts will be stored as temp objects under the prefix below.
	uploadID := fmt.Sprintf("%s/%d", in.Key, time.Now().UnixNano())
	return &CreateMultipartUploadOutput{UploadID: uploadID}, nil
}

func (b *gcsBackend) UploadPart(ctx context.Context, in UploadPartInput) (*UploadPartOutput, error) {
	partKey := gcsPartKey(in.UploadID, in.PartNumber)
	wc := b.client.Bucket(in.Bucket).Object(partKey).NewWriter(ctx)
	if _, err := io.Copy(wc, in.Body); err != nil {
		_ = wc.Close()
		return nil, fmt.Errorf("%w: writing gcs part: %w", ErrUpstreamError, err)
	}
	if err := wc.Close(); err != nil {
		return nil, mapGCSErr(err)
	}
	attrs, err := b.client.Bucket(in.Bucket).Object(partKey).Attrs(ctx)
	if err != nil {
		return nil, mapGCSErr(err)
	}
	return &UploadPartOutput{ETag: attrs.Etag}, nil
}

func (b *gcsBackend) CompleteMultipartUpload(ctx context.Context, in CompleteMultipartUploadInput) (*CompleteMultipartUploadOutput, error) {
	partKeys := make([]string, len(in.Parts))
	for i, p := range in.Parts {
		partKeys[i] = gcsPartKey(in.UploadID, p.PartNumber)
	}

	if err := b.composeTree(ctx, in.Bucket, partKeys, in.Key); err != nil {
		return nil, err
	}

	// Delete temp parts asynchronously; failures are non-fatal.
	go func() {
		bkt := b.client.Bucket(in.Bucket)
		for _, k := range partKeys {
			_ = bkt.Object(k).Delete(context.Background())
		}
	}()

	attrs, err := b.client.Bucket(in.Bucket).Object(in.Key).Attrs(ctx)
	if err != nil {
		return nil, mapGCSErr(err)
	}
	return &CompleteMultipartUploadOutput{ETag: attrs.Etag}, nil
}

func (b *gcsBackend) AbortMultipartUpload(ctx context.Context, in AbortMultipartUploadInput) error {
	// List all temp parts for this upload and delete them.
	prefix := "_sgw_parts/" + in.UploadID + "/"
	it := b.client.Bucket(in.Bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return mapGCSErr(err)
		}
		_ = b.client.Bucket(in.Bucket).Object(attrs.Name).Delete(ctx)
	}
	return nil
}

// composeTree recursively composes sources into dest, handling GCS's 32-source limit.
func (b *gcsBackend) composeTree(ctx context.Context, bucket string, sources []string, dest string) error {
	const gcsComposeLimit = 32

	if len(sources) <= gcsComposeLimit {
		return b.composeDirect(ctx, bucket, sources, dest)
	}

	// Split into chunks and compose each into an intermediate temp object.
	var intermediates []string
	for i := 0; i < len(sources); i += gcsComposeLimit {
		end := i + gcsComposeLimit
		if end > len(sources) {
			end = len(sources)
		}
		tmpKey := fmt.Sprintf("_sgw_tmp/%d-%d", i, time.Now().UnixNano())
		if err := b.composeDirect(ctx, bucket, sources[i:end], tmpKey); err != nil {
			b.deleteKeys(ctx, bucket, intermediates)
			return err
		}
		intermediates = append(intermediates, tmpKey)
	}

	// Recursively compose intermediates (handles > 32² = 1024 parts, etc.)
	if err := b.composeTree(ctx, bucket, intermediates, dest); err != nil {
		b.deleteKeys(ctx, bucket, intermediates)
		return err
	}
	b.deleteKeys(ctx, bucket, intermediates)
	return nil
}

func (b *gcsBackend) composeDirect(ctx context.Context, bucket string, sources []string, dest string) error {
	handles := make([]*storage.ObjectHandle, len(sources))
	bkt := b.client.Bucket(bucket)
	for i, k := range sources {
		handles[i] = bkt.Object(k)
	}
	_, err := bkt.Object(dest).ComposerFrom(handles...).Run(ctx)
	return mapGCSErr(err)
}

func (b *gcsBackend) deleteKeys(ctx context.Context, bucket string, keys []string) {
	bkt := b.client.Bucket(bucket)
	for _, k := range keys {
		_ = bkt.Object(k).Delete(ctx)
	}
}

func (b *gcsBackend) PresignURL(_ context.Context, in PresignInput) (*PresignOutput, error) {
	url, err := storage.SignedURL(in.Bucket, in.Key, &storage.SignedURLOptions{
		GoogleAccessID: b.sa.ClientEmail,
		PrivateKey:     []byte(b.sa.PrivateKey),
		Method:         in.Method,
		Expires:        time.Now().Add(in.Expires),
		Scheme:         storage.SigningSchemeV4,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: gcs presign: %w", ErrUpstreamError, err)
	}
	return &PresignOutput{URL: url}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func gcsPartKey(uploadID string, partNum int32) string {
	return fmt.Sprintf("_sgw_parts/%s/%05d", uploadID, partNum)
}

// parseHTTPRange parses "bytes=start-end" or "bytes=start-" into GCS-friendly
// (offset, length) where length=-1 means "to end of object".
func parseHTTPRange(r string) (offset, length int64, err error) {
	r = strings.TrimPrefix(r, "bytes=")
	parts := strings.SplitN(r, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed range")
	}
	if parts[0] == "" {
		return 0, 0, fmt.Errorf("suffix ranges not supported")
	}
	offset, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range start: %w", err)
	}
	if parts[1] == "" {
		return offset, -1, nil // bytes=N-
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range end: %w", err)
	}
	return offset, end - offset + 1, nil
}

func mapGCSErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return ErrObjectNotFound
	}
	if errors.Is(err, storage.ErrBucketNotExist) {
		return ErrBucketNotFound
	}
	var gErr *googleapi.Error
	if errors.As(err, &gErr) {
		switch gErr.Code {
		case 404:
			return ErrObjectNotFound
		case 403:
			return ErrAccessDenied
		}
	}
	return fmt.Errorf("%w: %w", ErrUpstreamError, err)
}
