package backend

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	blobpkg "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
)

// azureBackend implements Backend against Azure Blob Storage.
//
// Multipart upload maps to the Azure Block Blob API:
//   - CreateMultipartUpload  → generates an upload ID (no upstream call)
//   - UploadPart             → StageBlock; parts are buffered to a temp file because
//     the Azure SDK requires io.ReadSeekCloser for retry support
//   - CompleteMultipartUpload → CommitBlockList in ascending part order
//   - AbortMultipartUpload   → no-op (staged blocks auto-expire after 7 days)
//
// The S3 Bucket parameter maps to an Azure container name.
type azureBackend struct {
	client      *azblob.Client
	credential  *azblob.SharedKeyCredential
	accountName string
	serviceURL  string
}

func newAzureBackend(cfg AzureConfig) (Backend, error) {
	serviceURL := cfg.ServiceURL
	if serviceURL == "" {
		serviceURL = fmt.Sprintf("https://%s.blob.core.windows.net", cfg.AccountName)
	}

	cred, err := azblob.NewSharedKeyCredential(cfg.AccountName, cfg.AccountKey)
	if err != nil {
		return nil, fmt.Errorf("creating azure credential: %w", err)
	}

	client, err := azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("creating azure client: %w", err)
	}

	return &azureBackend{
		client:      client,
		credential:  cred,
		accountName: cfg.AccountName,
		serviceURL:  serviceURL,
	}, nil
}

func (b *azureBackend) blockBlobClient(containerName, blobName string) *blockblob.Client {
	return b.client.ServiceClient().NewContainerClient(containerName).NewBlockBlobClient(blobName)
}

func (b *azureBackend) containerClient(name string) *container.Client {
	return b.client.ServiceClient().NewContainerClient(name)
}

func (b *azureBackend) HeadObject(ctx context.Context, in HeadObjectInput) (*HeadObjectOutput, error) {
	resp, err := b.blockBlobClient(in.Bucket, in.Key).GetProperties(ctx, nil)
	if err != nil {
		return nil, mapAzureErr(err)
	}
	meta := make(map[string]string, len(resp.Metadata))
	for k, v := range resp.Metadata {
		if v != nil {
			meta[k] = *v
		}
	}
	return &HeadObjectOutput{
		ContentLength: derefInt64(resp.ContentLength),
		ContentType:   derefStr(resp.ContentType),
		ETag:          derefAzETag(resp.ETag),
		LastModified:  derefTime(resp.LastModified),
		Metadata:      meta,
	}, nil
}

func (b *azureBackend) GetObject(ctx context.Context, in GetObjectInput) (*GetObjectOutput, error) {
	opts := &blobpkg.DownloadStreamOptions{}
	if in.Range != "" {
		offset, count, err := parseAzureRange(in.Range)
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %w", ErrInvalidRange, in.Range, err)
		}
		opts.Range = blobpkg.HTTPRange{Offset: offset, Count: count}
	}

	resp, err := b.blockBlobClient(in.Bucket, in.Key).DownloadStream(ctx, opts)
	if err != nil {
		return nil, mapAzureErr(err)
	}

	var contentRange string
	if in.Range != "" {
		contentRange = derefStr(resp.ContentRange)
	}

	return &GetObjectOutput{
		Body:          resp.Body,
		ContentLength: derefInt64(resp.ContentLength),
		ContentRange:  contentRange,
		ContentType:   derefStr(resp.ContentType),
		ETag:          derefAzETag(resp.ETag),
		LastModified:  derefTime(resp.LastModified),
	}, nil
}

func (b *azureBackend) PutObject(ctx context.Context, in PutObjectInput) (*PutObjectOutput, error) {
	opts := &blockblob.UploadStreamOptions{}
	if in.ContentType != "" {
		ct := in.ContentType
		opts.HTTPHeaders = &blobpkg.HTTPHeaders{BlobContentType: &ct}
	}
	if len(in.Metadata) > 0 {
		m := make(map[string]*string, len(in.Metadata))
		for k, v := range in.Metadata {
			v := v
			m[k] = &v
		}
		opts.Metadata = m
	}

	resp, err := b.blockBlobClient(in.Bucket, in.Key).UploadStream(ctx, in.Body, opts)
	if err != nil {
		return nil, mapAzureErr(err)
	}
	return &PutObjectOutput{ETag: derefAzETag(resp.ETag)}, nil
}

func (b *azureBackend) DeleteObject(ctx context.Context, in DeleteObjectInput) error {
	_, err := b.client.DeleteBlob(ctx, in.Bucket, in.Key, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil
		}
	}
	return mapAzureErr(err)
}

func (b *azureBackend) DeleteObjects(ctx context.Context, in DeleteObjectsInput) (*DeleteObjectsOutput, error) {
	result := &DeleteObjectsOutput{}
	for _, obj := range in.Objects {
		_, err := b.client.DeleteBlob(ctx, in.Bucket, obj.Key, nil)
		if err != nil {
			var respErr *azcore.ResponseError
			if errors.As(err, &respErr) && respErr.StatusCode == 404 {
				if !in.Quiet {
					result.Deleted = append(result.Deleted, DeletedObject{Key: obj.Key})
				}
				continue
			}
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

func (b *azureBackend) HeadBucket(ctx context.Context, in HeadBucketInput) error {
	_, err := b.containerClient(in.Bucket).GetProperties(ctx, nil)
	return mapAzureErr(err)
}

func (b *azureBackend) ListObjects(ctx context.Context, in ListObjectsInput) (*ListObjectsOutput, error) {
	maxResults := in.MaxKeys
	if maxResults <= 0 {
		maxResults = 1000
	}
	result := &ListObjectsOutput{}

	if in.Delimiter != "" {
		pager := b.containerClient(in.Bucket).NewListBlobsHierarchyPager(in.Delimiter, &container.ListBlobsHierarchyOptions{
			Prefix:     strPtr(in.Prefix),
			MaxResults: &maxResults,
			Marker:     strPtr(in.ContinuationToken),
		})
		if pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, mapAzureErr(err)
			}
			for _, item := range page.Segment.BlobItems {
				result.Contents = append(result.Contents, ObjectInfo{
					Key:          derefStr(item.Name),
					Size:         derefInt64(item.Properties.ContentLength),
					ETag:         derefAzETag(item.Properties.ETag),
					LastModified: derefTime(item.Properties.LastModified),
				})
			}
			for _, pfx := range page.Segment.BlobPrefixes {
				result.CommonPrefixes = append(result.CommonPrefixes, derefStr(pfx.Name))
			}
			if page.NextMarker != nil && *page.NextMarker != "" {
				result.IsTruncated = true
				result.NextContinuationToken = *page.NextMarker
			}
		}
	} else {
		pager := b.containerClient(in.Bucket).NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
			Prefix:     strPtr(in.Prefix),
			MaxResults: &maxResults,
			Marker:     strPtr(in.ContinuationToken),
		})
		if pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, mapAzureErr(err)
			}
			for _, item := range page.Segment.BlobItems {
				result.Contents = append(result.Contents, ObjectInfo{
					Key:          derefStr(item.Name),
					Size:         derefInt64(item.Properties.ContentLength),
					ETag:         derefAzETag(item.Properties.ETag),
					LastModified: derefTime(item.Properties.LastModified),
				})
			}
			if page.NextMarker != nil && *page.NextMarker != "" {
				result.IsTruncated = true
				result.NextContinuationToken = *page.NextMarker
			}
		}
	}

	result.KeyCount = int32(len(result.Contents))
	return result, nil
}

// ---------------------------------------------------------------------------
// Multipart — Azure Block Blob StageBlock / CommitBlockList
// ---------------------------------------------------------------------------

func (b *azureBackend) CreateMultipartUpload(_ context.Context, in CreateMultipartUploadInput) (*CreateMultipartUploadOutput, error) {
	uploadID := fmt.Sprintf("sgw-%s-%d", strings.ReplaceAll(in.Key, "/", "-"), time.Now().UnixNano())
	return &CreateMultipartUploadOutput{UploadID: uploadID}, nil
}

func (b *azureBackend) UploadPart(ctx context.Context, in UploadPartInput) (*UploadPartOutput, error) {
	blockID := azureBlockID(in.PartNumber)

	// StageBlock requires io.ReadSeekCloser for internal retry support.
	// Buffer the part to a temp file, then pass the file handle.
	tmp, err := os.CreateTemp("", "sgw-part-*")
	if err != nil {
		return nil, fmt.Errorf("%w: creating temp file: %w", ErrUpstreamError, err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	if _, err := io.Copy(tmp, in.Body); err != nil {
		return nil, fmt.Errorf("%w: buffering part: %w", ErrUpstreamError, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("%w: seeking temp file: %w", ErrUpstreamError, err)
	}

	if _, err := b.blockBlobClient(in.Bucket, in.Key).StageBlock(ctx, blockID, tmp, nil); err != nil {
		return nil, mapAzureErr(err)
	}
	return &UploadPartOutput{ETag: blockID}, nil
}

func (b *azureBackend) CompleteMultipartUpload(ctx context.Context, in CompleteMultipartUploadInput) (*CompleteMultipartUploadOutput, error) {
	blockIDs := make([]string, len(in.Parts))
	for i, p := range in.Parts {
		blockIDs[i] = azureBlockID(p.PartNumber)
	}

	resp, err := b.blockBlobClient(in.Bucket, in.Key).CommitBlockList(ctx, blockIDs, nil)
	if err != nil {
		return nil, mapAzureErr(err)
	}
	return &CompleteMultipartUploadOutput{ETag: derefAzETag(resp.ETag)}, nil
}

func (b *azureBackend) AbortMultipartUpload(_ context.Context, _ AbortMultipartUploadInput) error {
	// Staged (uncommitted) blocks auto-expire after 7 days. No explicit abort exists.
	return nil
}

func (b *azureBackend) PresignURL(_ context.Context, in PresignInput) (*PresignOutput, error) {
	perms := sas.BlobPermissions{Read: true}
	if in.Method != "GET" {
		perms = sas.BlobPermissions{Write: true, Create: true}
	}

	now := time.Now().UTC()
	sasValues := sas.BlobSignatureValues{
		Protocol:      sas.ProtocolHTTPS,
		StartTime:     now.Add(-10 * time.Second),
		ExpiryTime:    now.Add(in.Expires),
		ContainerName: in.Bucket,
		BlobName:      in.Key,
		Permissions:   perms.String(),
	}

	queryParams, err := sasValues.SignWithSharedKey(b.credential)
	if err != nil {
		return nil, fmt.Errorf("%w: azure presign: %w", ErrUpstreamError, err)
	}

	sasURL := fmt.Sprintf("%s/%s/%s?%s", b.serviceURL, in.Bucket, in.Key, queryParams.Encode())
	return &PresignOutput{URL: sasURL}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func azureBlockID(partNum int32) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%05d", partNum)))
}

// parseAzureRange converts "bytes=N-M" / "bytes=N-" to (offset, count).
// count=0 means "to end of blob" in the Azure SDK.
func parseAzureRange(r string) (offset, count int64, err error) {
	r = strings.TrimPrefix(r, "bytes=")
	parts := strings.SplitN(r, "-", 2)
	if len(parts) != 2 || parts[0] == "" {
		return 0, 0, fmt.Errorf("unsupported range format")
	}
	offset, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return
	}
	if parts[1] == "" {
		return offset, 0, nil
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return
	}
	return offset, end - offset + 1, nil
}

func mapAzureErr(err error) error {
	if err == nil {
		return nil
	}
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.StatusCode {
		case 404:
			if respErr.ErrorCode == "BlobNotFound" {
				return ErrObjectNotFound
			}
			return ErrBucketNotFound
		case 403:
			return ErrAccessDenied
		case 416:
			return fmt.Errorf("%w: %w", ErrInvalidRange, err)
		}
	}
	return fmt.Errorf("%w: %w", ErrUpstreamError, err)
}

func derefAzETag(e *azcore.ETag) string {
	if e == nil {
		return ""
	}
	return stripQuotes(string(*e))
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
