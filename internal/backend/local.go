package backend

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// localBackend implements Backend against the local filesystem.
// Intended for development and integration testing only — not for production.
//
// Layout:  <RootPath>/<bucket>/<key>
// Multipart parts are staged under <RootPath>/.sgw_parts/<uploadID>/<partNum>
//
// PresignURL always returns ErrUpstreamError since there is no public URL.
type localBackend struct {
	root string
}

func newLocalBackend(cfg LocalConfig) (Backend, error) {
	if cfg.RootPath == "" {
		return nil, fmt.Errorf("local backend: root_path is required")
	}
	if err := os.MkdirAll(cfg.RootPath, 0o750); err != nil {
		return nil, fmt.Errorf("local backend: creating root path: %w", err)
	}
	return &localBackend{root: cfg.RootPath}, nil
}

func (b *localBackend) objectPath(bucket, key string) string {
	return filepath.Join(b.root, bucket, key)
}

func (b *localBackend) partPath(uploadID string, partNum int32) string {
	return filepath.Join(b.root, ".sgw_parts", uploadID, fmt.Sprintf("%05d", partNum))
}

func (b *localBackend) HeadObject(_ context.Context, in HeadObjectInput) (*HeadObjectOutput, error) {
	info, err := os.Stat(b.objectPath(in.Bucket, in.Key))
	if os.IsNotExist(err) {
		return nil, ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: stat: %w", ErrUpstreamError, err)
	}
	return &HeadObjectOutput{
		ContentLength: info.Size(),
		ContentType:   "application/octet-stream",
		ETag:          localETag(info),
		LastModified:  info.ModTime(),
	}, nil
}

func (b *localBackend) GetObject(_ context.Context, in GetObjectInput) (*GetObjectOutput, error) {
	path := b.objectPath(in.Bucket, in.Key)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: open: %w", ErrUpstreamError, err)
	}
	info, _ := f.Stat()
	return &GetObjectOutput{
		Body:          f,
		ContentLength: info.Size(),
		ContentType:   "application/octet-stream",
		ETag:          localETag(info),
		LastModified:  info.ModTime(),
	}, nil
}

func (b *localBackend) PutObject(_ context.Context, in PutObjectInput) (*PutObjectOutput, error) {
	path := b.objectPath(in.Bucket, in.Key)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("%w: mkdir: %w", ErrUpstreamError, err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("%w: create: %w", ErrUpstreamError, err)
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(io.MultiWriter(f, h), in.Body); err != nil {
		return nil, fmt.Errorf("%w: write: %w", ErrUpstreamError, err)
	}
	return &PutObjectOutput{ETag: hex.EncodeToString(h.Sum(nil))}, nil
}

func (b *localBackend) DeleteObject(_ context.Context, in DeleteObjectInput) error {
	err := os.Remove(b.objectPath(in.Bucket, in.Key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (b *localBackend) DeleteObjects(ctx context.Context, in DeleteObjectsInput) (*DeleteObjectsOutput, error) {
	result := &DeleteObjectsOutput{}
	for _, obj := range in.Objects {
		if err := b.DeleteObject(ctx, DeleteObjectInput{Bucket: in.Bucket, Key: obj.Key}); err != nil {
			result.Errors = append(result.Errors, DeleteError{
				Key: obj.Key, Code: "InternalError", Message: err.Error(),
			})
		} else if !in.Quiet {
			result.Deleted = append(result.Deleted, DeletedObject{Key: obj.Key})
		}
	}
	return result, nil
}

func (b *localBackend) HeadBucket(_ context.Context, in HeadBucketInput) error {
	info, err := os.Stat(filepath.Join(b.root, in.Bucket))
	if os.IsNotExist(err) || (err == nil && !info.IsDir()) {
		return ErrBucketNotFound
	}
	return err
}

func (b *localBackend) ListObjects(_ context.Context, in ListObjectsInput) (*ListObjectsOutput, error) {
	bucketPath := filepath.Join(b.root, in.Bucket)
	entries, err := os.ReadDir(bucketPath)
	if os.IsNotExist(err) {
		return nil, ErrBucketNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: readdir: %w", ErrUpstreamError, err)
	}

	maxKeys := int(in.MaxKeys)
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	result := &ListObjectsOutput{}
	seenPrefixes := map[string]struct{}{}

	for _, e := range entries {
		key := e.Name()
		if !strings.HasPrefix(key, in.Prefix) {
			continue
		}
		if in.StartAfter != "" && key <= in.StartAfter {
			continue
		}
		if in.ContinuationToken != "" && key <= in.ContinuationToken {
			continue
		}

		if in.Delimiter != "" {
			rest := strings.TrimPrefix(key, in.Prefix)
			if idx := strings.Index(rest, in.Delimiter); idx >= 0 {
				prefix := in.Prefix + rest[:idx+len(in.Delimiter)]
				if _, seen := seenPrefixes[prefix]; !seen {
					seenPrefixes[prefix] = struct{}{}
					result.CommonPrefixes = append(result.CommonPrefixes, prefix)
				}
				continue
			}
		}

		if len(result.Contents) >= maxKeys {
			result.IsTruncated = true
			result.NextContinuationToken = key
			break
		}

		info, err := e.Info()
		if err != nil {
			continue
		}
		result.Contents = append(result.Contents, ObjectInfo{
			Key:          key,
			Size:         info.Size(),
			ETag:         localETag(info),
			LastModified: info.ModTime(),
		})
	}

	sort.Strings(result.CommonPrefixes)
	result.KeyCount = int32(len(result.Contents))
	return result, nil
}

// ---------------------------------------------------------------------------
// Multipart — staged files concatenated at completion
// ---------------------------------------------------------------------------

func (b *localBackend) CreateMultipartUpload(_ context.Context, in CreateMultipartUploadInput) (*CreateMultipartUploadOutput, error) {
	uploadID := fmt.Sprintf("%s-%d", strings.ReplaceAll(in.Key, "/", "-"), time.Now().UnixNano())
	dir := filepath.Join(b.root, ".sgw_parts", uploadID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("%w: mkdir parts: %w", ErrUpstreamError, err)
	}
	return &CreateMultipartUploadOutput{UploadID: uploadID}, nil
}

func (b *localBackend) UploadPart(_ context.Context, in UploadPartInput) (*UploadPartOutput, error) {
	path := b.partPath(in.UploadID, in.PartNumber)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("%w: mkdir part dir: %w", ErrUpstreamError, err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("%w: create part: %w", ErrUpstreamError, err)
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(io.MultiWriter(f, h), in.Body); err != nil {
		return nil, fmt.Errorf("%w: write part: %w", ErrUpstreamError, err)
	}
	return &UploadPartOutput{ETag: hex.EncodeToString(h.Sum(nil))}, nil
}

func (b *localBackend) CompleteMultipartUpload(_ context.Context, in CompleteMultipartUploadInput) (*CompleteMultipartUploadOutput, error) {
	destPath := b.objectPath(in.Bucket, in.Key)
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return nil, fmt.Errorf("%w: mkdir dest: %w", ErrUpstreamError, err)
	}
	dest, err := os.Create(destPath)
	if err != nil {
		return nil, fmt.Errorf("%w: create dest: %w", ErrUpstreamError, err)
	}
	defer dest.Close()

	h := md5.New()
	w := io.MultiWriter(dest, h)
	for _, p := range in.Parts {
		partFile, err := os.Open(b.partPath(in.UploadID, p.PartNumber))
		if err != nil {
			return nil, fmt.Errorf("%w: open part %d: %w", ErrUpstreamError, p.PartNumber, err)
		}
		_, copyErr := io.Copy(w, partFile)
		partFile.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("%w: copy part %d: %w", ErrUpstreamError, p.PartNumber, copyErr)
		}
	}

	_ = os.RemoveAll(filepath.Join(b.root, ".sgw_parts", in.UploadID))
	return &CompleteMultipartUploadOutput{ETag: hex.EncodeToString(h.Sum(nil))}, nil
}

func (b *localBackend) AbortMultipartUpload(_ context.Context, in AbortMultipartUploadInput) error {
	return os.RemoveAll(filepath.Join(b.root, ".sgw_parts", in.UploadID))
}

func (b *localBackend) PresignURL(_ context.Context, _ PresignInput) (*PresignOutput, error) {
	return nil, fmt.Errorf("%w: local backend does not support presigned URLs", ErrUpstreamError)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func localETag(info os.FileInfo) string {
	h := md5.New()
	fmt.Fprintf(h, "%d-%d", info.Size(), info.ModTime().UnixNano())
	return hex.EncodeToString(h.Sum(nil))
}
